#!/usr/bin/env bash
# Walks through what the operator does, against whatever cluster KUBECONFIG
# points at, and checks each step. Works on the local sandbox (`make sandbox`),
# on kind and on OpenShift. The operator has to be running already.
#
#   make sandbox            # terminal 1
#   make run-sandbox        # terminal 2
#   make demo               # terminal 3
set -uo pipefail

KUBECTL="${KUBECTL:-kubectl}"
TENANT=demo-shop
FAILED=0

say()  { printf '\n\033[1m== %s\033[0m\n' "$*"; }
pass() { printf '   \033[32mok\033[0m    %s\n' "$*"; }
fail() { printf '   \033[31mFAIL\033[0m  %s\n' "$*"; FAILED=$((FAILED + 1)); }
k()    { "$KUBECTL" "$@"; }

# until <seconds> <command...>: retry until the command succeeds.
until_ok() {
  local deadline=$((SECONDS + $1)); shift
  until "$@" >/dev/null 2>&1; do
    [ "$SECONDS" -ge "$deadline" ] && return 1
    sleep 0.5
  done
}
check()   { local what=$1; shift; if until_ok 30 "$@"; then pass "$what"; else fail "$what"; fi; }
field()   { k get "$1" "$2" ${4:+-n "$4"} -o "jsonpath=$3" 2>/dev/null; }
is()      { [ "$(field "$1" "$2" "$3" "${5:-}")" = "$4" ]; }
missing() { ! k get "$@" >/dev/null 2>&1; }
ready()   { is projectclaim "$1" '{.status.phase}' Ready; }
reason()  { is projectclaim "$1" '{.status.conditions[?(@.type=="Admitted")].reason}' "$2"; }

# The sandbox has no kube-controller-manager, so nothing finishes the deletion
# of a namespace. This does what the namespace controller would do.
finalize_namespace() {
  k get namespace "$1" -o json 2>/dev/null | sed 's/"kubernetes"//' |
    k replace --raw "/api/v1/namespaces/$1/finalize" -f - >/dev/null 2>&1
}
is_sandbox() { [ -z "$(k get nodes -o name 2>/dev/null)" ]; }

cleanup() {
  k delete projectclaim -l demo=projectclaim --wait=false >/dev/null 2>&1
  if is_sandbox; then
    sleep 2
    for ns in demo-shop-web demo-shop-api demo-shop-big; do finalize_namespace "$ns"; done
  fi
  k delete tenant "$TENANT" --ignore-not-found --wait=false >/dev/null 2>&1
  k delete projecttier demo-standard --ignore-not-found --wait=false >/dev/null 2>&1
  k delete namespace demo-shop-taken --ignore-not-found --wait=false >/dev/null 2>&1
  if is_sandbox; then finalize_namespace demo-shop-taken; fi
}

claim() { # claim <name> [extra spec lines...]
  local name=$1; shift
  {
    cat <<EOF
apiVersion: platform.mella7.me/v1alpha1
kind: ProjectClaim
metadata:
  name: $name
  labels: {demo: projectclaim}
spec:
  projectName: $name
  tenantRef: {name: $TENANT}
  tier: demo-standard
  access:
    requester: alice@example.com
    editorGroups: [shop-devs]
EOF
    for line in "$@"; do printf '  %s\n' "$line"; done
  } | k apply -f - >/dev/null
}

say "0. Preflight"
if ! k get crd projectclaims.platform.mella7.me >/dev/null 2>&1; then
  fail "CRDs are not installed (run: make install)"; exit 1
fi
pass "CRDs are installed"
cleanup; sleep 1

say "1. A tier, a tenant with a budget of 5 CPU, and a first project"
k apply -f - >/dev/null <<EOF
apiVersion: platform.mella7.me/v1alpha1
kind: ProjectTier
metadata: {name: demo-standard}
spec:
  quota: {requests.cpu: "2", requests.memory: 4Gi, pods: "20"}
  containerDefaults:
    limits: {cpu: 500m, memory: 512Mi}
    requests: {cpu: 50m, memory: 128Mi}
---
apiVersion: platform.mella7.me/v1alpha1
kind: Tenant
metadata: {name: $TENANT}
spec:
  namespacePrefix: demo-shop-
  quotaBudget: {requests.cpu: "5"}
EOF
claim demo-shop-web 'quota: {pods: "60"}'
check "claim demo-shop-web is Ready" ready demo-shop-web
for obj in resourcequota/projectclaim-quota limitrange/projectclaim-limits \
           rolebinding/projectclaim-admin rolebinding/projectclaim-edit \
           networkpolicy/projectclaim-isolation; do
  check "created $obj" k get "$obj" -n demo-shop-web
done
check "quota override applied (pods=60)" is resourcequota projectclaim-quota '{.spec.hard.pods}' 60 demo-shop-web
check "tier value kept (requests.cpu=2)" is resourcequota projectclaim-quota '{.spec.hard.requests\.cpu}' 2 demo-shop-web
check "tenant ledger shows 2 CPU reserved" is tenant "$TENANT" '{.status.allocated.requests\.cpu}' 2

say "2. Drift: delete and edit what the operator manages"
k delete resourcequota projectclaim-quota -n demo-shop-web >/dev/null
check "deleted quota came back" k get resourcequota projectclaim-quota -n demo-shop-web
k patch resourcequota projectclaim-quota -n demo-shop-web --type merge -p '{"spec":{"hard":{"pods":"9999"}}}' >/dev/null
check "edited quota was reverted to 60" is resourcequota projectclaim-quota '{.spec.hard.pods}' 60 demo-shop-web
k label namespace demo-shop-web pod-security.kubernetes.io/enforce=privileged --overwrite >/dev/null
check "lowered Pod Security level was reverted" is namespace demo-shop-web '{.metadata.labels.pod-security\.kubernetes\.io/enforce}' baseline

say "3. Budget: 5 CPU, each project takes 2"
claim demo-shop-api
check "second project fits (4 of 5 CPU)" ready demo-shop-api
claim demo-shop-big
check "third project is held: QuotaBudgetExceeded" reason demo-shop-big QuotaBudgetExceeded
check "and gets no namespace" missing namespace demo-shop-big
printf '         %s\n' "$(field projectclaim demo-shop-big '{.status.conditions[?(@.type=="Admitted")].message}')"
k patch tenant "$TENANT" --type merge -p '{"spec":{"quotaBudget":{"requests.cpu":"6"}}}' >/dev/null
check "raising the budget to 6 admits it, untouched" ready demo-shop-big

say "4. A namespace the operator did not create"
k create namespace demo-shop-taken >/dev/null
k label namespace demo-shop-taken important=yes >/dev/null
k apply -f - >/dev/null <<EOF
apiVersion: platform.mella7.me/v1alpha1
kind: ProjectClaim
metadata: {name: demo-hostile, labels: {demo: projectclaim}}
spec:
  projectName: demo-shop-taken
  tenantRef: {name: $TENANT}
  tier: demo-standard
  access: {requester: mallory@example.com}
EOF
check "claim is refused: NamespaceConflict" reason demo-hostile NamespaceConflict
[ -z "$(field namespace demo-shop-taken '{.metadata.ownerReferences}')" ] && pass "namespace has no owner reference" || fail "namespace was adopted"
[ -z "$(field namespace demo-shop-taken '{.metadata.labels.app\.kubernetes\.io/managed-by}')" ] && pass "namespace was not labelled" || fail "namespace was labelled"
k delete projectclaim demo-hostile >/dev/null 2>&1
check "namespace survives the claim's deletion" is namespace demo-shop-taken '{.status.phase}' Active

say "5. Rules the API server enforces before the operator sees anything"
out=$(k patch projectclaim demo-shop-web --type merge -p '{"spec":{"projectName":"renamed"}}' 2>&1)
case $out in *immutable*) pass "rename rejected: projectName is immutable" ;; *) fail "rename was accepted" ;; esac
out=$(printf 'apiVersion: platform.mella7.me/v1alpha1\nkind: ProjectClaim\nmetadata: {name: demo-bad}\nspec:\n  projectName: kube-system\n  tenantRef: {name: x}\n  tier: x\n  access: {requester: x}\n' | k apply --dry-run=server -f - 2>&1)
case $out in *reserved*) pass "projectName kube-system rejected" ;; *) fail "kube-system was accepted" ;; esac

say "6. Deletion: the claim waits for its namespace"
k delete projectclaim demo-shop-api --wait=false >/dev/null
going() { is namespace demo-shop-api '{.status.phase}' Terminating || missing namespace demo-shop-api; }
check "namespace deletion was requested" going
if is_sandbox; then
  sleep 3
  k get projectclaim demo-shop-api >/dev/null 2>&1 && pass "claim is still there, held by its finalizer" || fail "claim vanished before its namespace"
  check "its 2 CPU are still reserved" is tenant "$TENANT" '{.status.projectCount}' 3
  finalize_namespace demo-shop-api
fi
check "namespace gone, then claim gone" missing projectclaim demo-shop-api
check "budget released (2 projects left)" is tenant "$TENANT" '{.status.projectCount}' 2

say "State at the end"
k get projectclaims,tenants

cleanup
if [ "$FAILED" -eq 0 ]; then printf '\n\033[32mAll checks passed.\033[0m\n'; else printf '\n\033[31m%d check(s) failed.\033[0m\n' "$FAILED"; fi
exit "$FAILED"
