# Testing

| Level | Command | Needs | What it covers |
|---|---|---|---|
| Unit | `make test` | Go | Budget arithmetic and desired-state builders, as table tests |
| Integration | `make test` | Go | Both controllers against a real API server and etcd: 48 specs |
| Mutation | `make mutation-test` | Go, Python 3 | That the specs above fail when a guarantee is removed |
| Local walkthrough | `make sandbox`, `make run-sandbox`, `make demo` | Go, kubectl | The real binary, driven with kubectl, 28 checks |
| End to end | CI job `e2e`, or by hand on kind | Docker, kind | The real image as its real ServiceAccount, with a controller manager |

## How the integration suite is set up

[envtest](https://book.kubebuilder.io/reference/envtest) starts `kube-apiserver` and `etcd` as local processes. Everything at the API level behaves as in a cluster: admission, CEL validation, defaulting, the status subresource, RBAC, finalizers, watches.

There is no `kube-controller-manager`. Nothing garbage collects, nothing finishes the deletion of a namespace, and the built-in `admin`, `edit` and `view` roles are never filled in. The suite deals with each of these on purpose:

- **Namespace deletion.** A deleted namespace stays in `Terminating`. That is the situation the finalizer exists for, so the deletion spec first asserts that the claim, its finalizer and its reservation are all still there, and then plays the namespace controller itself (it clears the namespace's finalizers through the `finalize` subresource) and asserts that the claim goes and the budget is released.
- **Built-in roles.** The setup fills them with rules, so that binding someone to them is a real escalation. See "What the mutation checks found" below.

Three choices make the suite stricter than the default kubebuilder scaffold:

**The operator runs with its shipped ClusterRole.** Test suites usually run the controller with the admin credentials envtest hands out, which hides every missing RBAC rule until the first real deployment answers `forbidden`. Here the setup applies `config/rbac/role.yaml` as it is, binds it to the operator's ServiceAccount identity, and starts the manager as that user. The admission policy is applied from `config/admission` the same way.

**"Wrote nothing" is measured, not inferred.** The usual idempotency test compares `resourceVersion` before and after. That misses a wasteful operator, because the API server drops an update that changes nothing without bumping the version. The suite wraps the operator's client and records every create, update, patch and delete, including status writes. After three forced reconciles of a settled claim the list of writes touching that claim, its namespace or its tenant must be empty.

**Concurrency is real.** The manager runs four workers, and the budget spec creates twelve claims at once that fit four at a time.

Each spec uses its own tier, tenant and names, so specs do not depend on each other or on order, except inside the one ordered container that builds up a project step by step.

## Mutation checks

`hack/mutate.py` copies the repository, makes one change that removes a guarantee, and runs the spec that is supposed to protect it. The check passes when the spec fails. There are eleven, listed in the README.

A mutation is a text replacement. If the code it targets has been rewritten, the script reports the target as not found instead of passing vacuously, and a spec name that matches nothing is reported the same way.

### What the mutation checks found

**A write on every reconcile that no test saw.** The first mutation removed the line that mirrors the API server's defaulting of `LimitRange` requests. The idempotency spec, which at that point compared `resourceVersion`s, still passed. The operator was sending an update for every project on every reconcile, and the API server was discarding each one as a no-op, so the version never moved. That is why the spec now counts write calls.

**A permission that envtest could not miss.** Removing the `bind` verb from the operator's ClusterRole should make it unable to create a RoleBinding to `admin`. The specs still passed. In envtest the built-in roles are empty, because the controller that aggregates rules into them does not run, so binding to them escalated nothing and the API server allowed it. In a real cluster the same operator would have failed. The setup now fills those roles.

## The local walkthrough

`make sandbox` runs the same control plane as the tests and keeps it running, with the CRDs and the admission policy installed, and writes `bin/sandbox.kubeconfig`. `make run-sandbox` runs the operator against it. From there it is ordinary kubectl.

`make demo` (or `hack/demo.sh`) creates a tier, a tenant and a few claims and checks 28 things along the way: provisioning, drift correction, the budget, the adoption guard, schema validation and deletion. It works against any cluster. On the sandbox it also finishes namespace deletions itself, since nothing else there will.

Useful things to try by hand:

```bash
export KUBECONFIG=bin/sandbox.kubeconfig
kubectl apply -k config/samples

# Drift
kubectl delete networkpolicy projectclaim-isolation -n payments-staging
kubectl get networkpolicy -n payments-staging

# What the operator is doing
curl -s localhost:8080/metrics | grep '^projectclaim_'
kubectl get events --field-selector involvedObject.kind=ProjectClaim

# A change that is refused: the project keeps running as it was
kubectl patch projectclaim payments-staging --type merge -p '{"spec":{"quota":{"requests.cpu":"100"}}}'
kubectl get projectclaim payments-staging -o wide

# The admission policy. First give everyone plain RBAC access to claims,
# so that the policy is the only thing deciding.
kubectl apply -f config/rbac/claim_author_role.yaml
kubectl create clusterrolebinding claim-authors --clusterrole=projectclaim-author --group=system:authenticated

# Not an owner of tenant "payments": denied
kubectl annotate projectclaim payments-staging note=hello --dry-run=server \
  --as=mallory --as-group=system:authenticated
# Listed in the tenant's owners: allowed
kubectl annotate projectclaim payments-staging note=hello --dry-run=server \
  --as=alice@example.com --as-group=system:authenticated
```

## End to end on kind

The CI job builds the image, loads it into a kind cluster, deploys `dist/install.yaml` and runs `make demo`. By hand:

```bash
kind create cluster --name projectclaim
make docker-build IMG=project-claim:dev
kind load docker-image project-claim:dev --name projectclaim
make deploy IMG=project-claim:dev
kubectl -n projectclaim-system rollout status deployment/projectclaim-controller-manager
make demo
```

This is the level that has a real controller manager, so namespaces terminate by themselves and the operator runs in a pod under its own ServiceAccount.

Leader election can be watched here:

```bash
kubectl -n projectclaim-system get lease projectclaim.platform.mella7.me -o jsonpath='{.spec.holderIdentity}{"\n"}'
kubectl -n projectclaim-system delete pod <the holder>
```

The standby takes over. Nothing is lost in between, because the desired state is in etcd and not in the operator's memory.

kind's default CNI does not enforce NetworkPolicy. The policy object is created and kept in sync either way. To see traffic being blocked, use a cluster with Calico or Cilium.

## On OpenShift

Not covered by CI. Things worth checking by hand on CRC or a cluster:

```bash
# The pod is admitted under restricted-v2 without a fixed UID
oc -n projectclaim-system get pod -o jsonpath='{.items[*].metadata.annotations.openshift\.io/scc}{"\n"}'

# The namespace shows up as a Project, with requester and description
oc get project payments-staging
oc describe project payments-staging

# The requester really got access, and only there
oc auth can-i create deployments -n payments-staging --as=alice@example.com
oc auth can-i create deployments -n default --as=alice@example.com
```
