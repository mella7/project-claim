# Design decisions

Each section states a decision, the reason for it, and what was considered instead.

- [Why an operator at all](#why-an-operator-at-all)
- [Cluster-scoped resources](#cluster-scoped-resources)
- [Enforcing the tenant budget](#enforcing-the-tenant-budget)
- [Refusals are not errors](#refusals-are-not-errors)
- [A refused change never tears anything down](#a-refused-change-never-tears-anything-down)
- [The adoption guard](#the-adoption-guard)
- [The finalizer](#the-finalizer)
- [Create-or-update, or Server-Side Apply](#create-or-update-or-server-side-apply)
- [What the cache holds, and when it is not trusted](#what-the-cache-holds-and-when-it-is-not-trusted)
- [Watches, and keeping reconciles rare](#watches-and-keeping-reconciles-rare)
- [Who may claim: admission policy, not a webhook](#who-may-claim-admission-policy-not-a-webhook)
- [Validation in the schema](#validation-in-the-schema)
- [Status](#status)
- [Known limits](#known-limits)

## Why an operator at all

A Helm chart or a GitOps template can create a namespace with a quota and some role bindings. It renders once and has no opinion about what happens afterwards. If someone edits the quota by hand, nothing notices until the next sync, and nothing can answer "does this team still have budget for another project?" because that depends on every other project.

Both of those need something that keeps watching and that can see across namespaces. That is a controller.

## Cluster-scoped resources

`ProjectClaim`, `Tenant` and `ProjectTier` are all cluster-scoped.

For `ProjectClaim` this follows from the garbage collector's rules. A namespaced object may be owned by a cluster-scoped one. A cluster-scoped object, such as a Namespace, may only be owned by another cluster-scoped object. A namespaced claim that set itself as owner of a Namespace would get an `OwnerRefInvalidNamespace` event and a reference that is ignored. Since the claim has to own both the namespace and the objects inside it, cluster scope is the only shape where every owner reference is valid.

The cost is that RBAC cannot scope claims by namespace. That is what the [admission policy](#who-may-claim-admission-policy-not-a-webhook) is for.

## Enforcing the tenant budget

A tenant has a budget, for example 24 CPU of requests across all its projects. A claim may only be admitted while the sum stays within it.

**The obvious implementation is wrong.** List the tenant's claims from the informer cache, add up their quotas, compare. Two claims reconciled at the same moment by two workers both see the old total, both conclude they fit, and both are admitted. With several workers, or a second replica during a leader change, this is a matter of time.

**What the operator does.** The tenant's status holds a ledger: one reservation per admitted claim, keyed by the claim's UID. To be admitted, a claim:

1. reads the tenant from the API server, not the cache,
2. checks its request against the budget minus everyone else's reservations,
3. adds its reservation and writes the status with `Update`.

`Update` sends the `resourceVersion` that was read. If another claim wrote in between, the API server answers `409 Conflict`, and the claim goes back to step 1 with fresh data. The API server decides the order, so the budget holds for any number of workers and replicas without a lock of the operator's own.

The reservation is written before anything is created. A claim that does not fit never gets a namespace.

Using `Update` rather than a merge patch matters. A merge patch without a `resourceVersion` would apply on top of whatever is there, and the last writer would silently drop the other's reservation. The mutation check "the budget is checked on the cache and written without a resourceVersion" makes exactly that change and the concurrency test fails.

Alternatives considered:

- **OpenShift `ClusterResourceQuota`.** It enforces a quota across the namespaces a selector matches, and it enforces actual usage, which this ledger does not. It is OpenShift-only, and it rejects pods at runtime instead of rejecting the project up front. The two fit together well: the ledger decides whether a project may exist, a `ClusterResourceQuota` could cap what the tenant's pods use.
- **A validating webhook that sums the claims.** It has the same race, since two admission requests can be evaluated concurrently, and it adds a component that must be highly available.
- **A lock, or one worker.** Works for one process. Leader election narrows the window for two processes but does not close it.

Two details of the ledger:

- Reservations are keyed by UID. A claim that is deleted and recreated under the same name is a new object and replaces the old reservation instead of inheriting it.
- If the tenant budgets a resource that the project has no quota for, the claim is refused. Admitting it would leave the project unbounded in exactly the resource the tenant wanted to cap.

## Refusals are not errors

A claim can fail in two different ways.

A **transient failure**, such as a conflict or an API server that is briefly away, will probably work on the next attempt. `Reconcile` returns the error, and the workqueue retries with exponential backoff per claim.

A **refusal**, such as "tenant is out of budget" or "tier not allowed", will fail the same way however often it is retried. Returning an error would put the claim into a backoff loop that produces log noise and never fixes anything. So `Reconcile` records the reason on the claim's `Admitted` condition, emits one event, and returns without error.

What brings a refused claim back is a change to what it depends on. The claim controller watches `Tenant` and `ProjectTier`, so raising a budget, freeing a slot or creating the missing tier wakes exactly the claims that were waiting. There is no polling. The one exception is a namespace that is in the way and terminating: it is not ours, no watch will report it, so that case is rechecked every ten seconds.

## A refused change never tears anything down

If a running project's claim is changed to something that cannot be admitted, for example a quota beyond the budget, the operator keeps the last admitted configuration in place: the namespace's quota and the ledger entry stay as they were. The claim reports `Degraded` with `Ready=False, reason=SpecNotApplied` and the refusal message.

The alternative, converging to "not admitted" by removing the namespace, would turn a typo in a manifest into an outage.

## The adoption guard

Namespace names are one flat, cluster-wide space, and a claim can ask for any name. Without a check, create-or-update would find an existing namespace, set the claim as its owner and delete it when the claim is deleted.

Three layers prevent that:

1. The schema rejects `default`, `openshift`, and anything starting with `kube-` or `openshift-`.
2. A tenant can be given a name prefix, which keeps tenants out of each other's names.
3. Before writing anything, the operator checks whether an existing namespace of that name is controlled by this claim, by UID. If it is not, the claim is refused with `NamespaceConflict` and the namespace is left exactly as it was. On deletion the same check runs, so a refused claim can be deleted without touching the namespace.

Taking over an existing namespace is possible, but it has to be asked for from the namespace's side: a cluster admin annotates it with `platform.mella7.me/adopt-into: <claim name>`. Whoever can annotate a namespace already controls it, so this does not widen anyone's reach.

## The finalizer

Deleting an object that has a finalizer does not delete it. The API server sets `deletionTimestamp` and waits for the finalizer to be removed.

The operator removes its finalizer only when both of these are true:

- a read from the API server, not the cache, says the namespace is gone (or it has been detached, see below),
- the claim's reservation has been removed from the tenant's ledger.

Owner references alone would also get the namespace deleted eventually, through the garbage collector. What they do not give is confirmation. The claim would disappear at once, and if the namespace then hangs in `Terminating` because something inside it has a finalizer of its own, nobody is told, and the tenant's budget shows space that is not really free. With the finalizer, a stuck namespace is visible as a claim stuck in `Terminating`, and there is an alert for it.

While waiting, the operator does not poll. The namespace is owned by the claim, so its deletion arrives as a watch event.

**`reclaimPolicy: Retain`** keeps the namespace. Before letting the claim go, the operator removes the claim's owner reference from the namespace and from every object it created inside it. If any reference stayed, the garbage collector would delete that object as soon as its owner is gone.

**Order of operations on create.** The finalizer is added before the first object is created. The other order has a window in which a crash leaves a namespace and no finalizer to clean it up.

## Create-or-update, or Server-Side Apply

Managed objects are written with controller-runtime's `CreateOrUpdate`: read the object, run a function that sets the desired fields, compare before and after, and write only if they differ.

That last part is what makes a settled project free, and it only works if the function is deterministic and assigns fields instead of appending to them. It also has a less obvious requirement: the desired state must match the object as the API server stores it, including the server's own defaulting. `LimitRange` shows why. If a default limit is set and no default request, the API server fills in the request from the limit. A desired state without that field never compares equal to the stored object, so every reconcile sends an update. The server discards it as a no-op, nothing looks wrong, and the operator makes one pointless write per project per reconcile forever. The operator therefore applies the same defaulting rule itself (`limitRangeSpec` in `desired.go`).

**Server-Side Apply** would avoid that class of problem, because the server merges only the fields the operator declares. It also records field ownership, which matters when several controllers write to the same object. It was not chosen as the default here for two reasons: an SSA patch is a write on every reconcile unless the operator first works out whether anything would change, and the read-compare-write cycle is easier to reason about and to test for "wrote nothing". For objects shared with other controllers, SSA would be the better choice, and the namespace is the candidate if that ever becomes necessary.

On the namespace the operator already behaves as a polite co-owner. It sets only its own labels and annotations and leaves all others alone. It records which labels it set in an annotation, so a label removed from the claim can be removed from the namespace without guessing.

## What the cache holds, and when it is not trusted

Watching a kind makes the informer cache hold every object of that kind. For RoleBindings, ResourceQuotas, LimitRanges and NetworkPolicies that would be every such object in the cluster, for an operator that cares about a handful per project. The cache is therefore restricted with a label selector to objects labelled `app.kubernetes.io/managed-by=projectclaim-operator`, so the API server sends only those.

Two consequences are handled explicitly:

- An object with one of the operator's names but without its label is invisible to the cache. Creating it fails with `AlreadyExists`. The operator then reads it from the API server and takes it over.
- Namespaces are not filtered. The adoption guard has to see namespaces that are not the operator's.

Reads normally come from the cache. Three decisions are made on a direct read from the API server instead, because acting on stale data there cannot be undone:

- reserving budget,
- deciding that the namespace is gone and the finalizer may come off,
- deciding that a reservation's claim no longer exists and the reservation may be dropped.

## Watches, and keeping reconciles rare

| Source | Wakes |
|---|---|
| `ProjectClaim` | itself |
| Namespace, ResourceQuota, LimitRange, RoleBinding, NetworkPolicy | the claim in their controller reference |
| `ProjectTier` | every claim on that tier (field index) |
| `Tenant` | see below |

Three things keep this from becoming a storm:

- **ResourceQuota status is ignored.** The quota controller rewrites `status.used` whenever a pod starts or stops. A predicate drops updates that change nothing but status.
- **A tenant's status changes wake only unsettled claims.** Every reservation is a write to the tenant's status. If each one woke all the tenant's claims, admitting N projects would cost N² reconciles. A status change can only matter to a claim that is waiting, so only those are enqueued. A spec change goes to all.
- **A settled claim costs no API calls.** It finds its reservation in the cached tenant and its objects in the cache, compares, and stops.

The workqueue never gives the same claim to two workers at once, so `--max-concurrent-reconciles` runs different claims in parallel without races on a single claim. The budget is the one thing claims share, and that is what the ledger protects.

## Who may claim: admission policy, not a webhook

Claims are cluster-scoped, so RBAC can only say "may create claims", not "may create claims for tenant X". The rule that a claim's author must be an owner of its tenant needs the identity of the caller, and a controller never sees that. Only admission does.

The check is a `ValidatingAdmissionPolicy`, evaluated in CEL inside the API server. Compared with a validating webhook there is no server to run, no certificate to issue and rotate, no network hop, and no failure mode in which claims cannot be written because the webhook is down.

It uses the Tenant as the policy's parameter. The binding selects all Tenants, so the expression is evaluated once per Tenant: it passes immediately for tenants the claim does not reference, and for the one it does, the caller must be in `owners` or in one of `ownerGroups`.

Platform admins and the operator itself have to get past the policy too, and neither is named in it. The expression asks the authorizer whether the caller holds the verb `administer` on that tenant. `administer` is not an API verb, it exists only to be granted through RBAC. The operator's ClusterRole has it, `cluster-admin` has every verb, and a platform team can grant it per tenant. Who is exempt is then visible and auditable in RBAC.

Status updates are not subject to the policy, since it matches `projectclaims` and not `projectclaims/status`.

## Validation in the schema

Anything that can be decided from the object alone is in the CRD, so a bad claim is rejected at `kubectl apply` with a message instead of being accepted and failing later:

- `projectName` is a valid namespace name, is not reserved, and is immutable. A rename would leave the old namespace behind with nothing pointing at it.
- `tenantRef` is immutable. Moving a project between tenants would move its cost between budgets and is better done deliberately.
- `extraLabels` may not use the operator's, Kubernetes', OpenShift's or Pod Security's label prefixes, so a claim cannot lower its own Pod Security level through a label.
- The claim's name is at most 63 characters, because it is used as a label value.

What depends on other objects (does the tier exist, does the tenant allow it, is there budget) cannot be checked in the schema and is the controller's job.

## Status

- `phase` is a one-word summary for people. Automation should read the conditions.
- `Admitted` says whether the current spec passed policy and holds its reservation. `Ready` says whether the cluster matches the spec. Keeping them apart makes "held by policy" and "working on it" distinguishable.
- `observedGeneration` tells a reader whether the status describes the spec they are looking at.
- Conditions are set through the apimachinery helper, which keeps `lastTransitionTime` unless the status value changes.
- Status is written once per reconcile and only if it differs, compared semantically so that `1Gi` and `1024Mi` count as equal. A status write that changes nothing would still cause a watch event and another reconcile.
- `expiresAt` is cut to whole seconds, the precision the API server stores. A finer value would never compare equal to what was read back.

## Known limits

- **The ledger lives in one object.** Each reservation is a few hundred bytes, and etcd's limit is about 1.5 MB per object, so a tenant can hold a few thousand projects. Beyond that the ledger would need to be split.
- **All namespaces are cached.** Needed for the adoption guard. On very large clusters a metadata-only informer would cut the memory.
- **Deleting a Tenant does not delete its projects.** Its claims report `TenantNotFound`, running projects keep running as `Degraded`. Whether to cascade is a policy choice and was left out.
- **Deleting a tier that is in use** has the same effect, for the same reason.
- **The budget counts reserved quota, not usage.**
- **Lifetime is counted from the claim's creation** and cannot be extended except by raising `ttl`.
