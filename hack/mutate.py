#!/usr/bin/env python3
"""Checks that the tests can fail.

A green test suite says little if the tests would also pass with the
guarantee removed. For each guarantee the operator makes, this script takes
the guarantee out of a copy of the repository and runs the spec that is
supposed to protect it. The check passes when that spec FAILS.

Run it with `make mutation-test`. A mutation is a plain text replacement, so
when the code it targets is rewritten the script says "target not found"
instead of silently testing nothing.
"""
import os
import re
import shutil
import subprocess
import sys
import tempfile

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))


def replace(work, path, old, new):
    full = os.path.join(work, path)
    with open(full) as f:
        text = f.read()
    if old not in text:
        raise LookupError(f"target not found in {path}: {old.strip()[:70]!r}")
    with open(full, "w") as f:
        f.write(text.replace(old, new, 1))


# (what is broken, the spec that has to notice, [(file, old, new), ...])
MUTANTS = [
    (
        "the desired LimitRange ignores a default the API server applies",
        "writes nothing when it reconciles again",
        [("internal/controller/desired.go",
          "\tfor k, v := range limits {\n\t\tif _, ok := requests[k]; !ok {\n\t\t\trequests[k] = v.DeepCopy()\n\t\t}\n\t}\n",
          "")],
    ),
    (
        "the adoption guard is gone",
        "refuses a namespace it did not create",
        [("internal/controller/projectclaim_controller.go",
          "\tif ownedBy(ns, claim) || adoptable(ns, claim) {\n\t\treturn nil, nil\n\t}\n",
          "\treturn nil, nil\n")],
    ),
    (
        "the finalizer is released as soon as deletion is requested",
        "holds the claim until the namespace is really gone",
        [("internal/controller/delete.go",
          "to terminate\", ns.Name))\n\t}\n\treturn false, nil",
          "to terminate\", ns.Name))\n\t}\n\treturn true, nil")],
    ),
    (
        "the budget is checked on the cache and written without a resourceVersion",
        "never overbooks",
        [("internal/controller/reserve.go",
          "if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(cached), tenant); err != nil {",
          "if err := r.Get(ctx, client.ObjectKeyFromObject(cached), tenant); err != nil {"),
         ("internal/controller/reserve.go",
          "\t\tif !setReservation(&tenant.Status, claim, want) {\n\t\t\treturn nil\n\t\t}\n\t\treturn r.Status().Update(ctx, tenant)",
          "\t\tbase := tenant.DeepCopy()\n\t\tif !setReservation(&tenant.Status, claim, want) {\n\t\t\treturn nil\n\t\t}\n\t\treturn r.Status().Patch(ctx, tenant, client.MergeFrom(base))")],
    ),
    (
        "claims do not watch their tenant",
        "admits the next one when a slot frees up",
        [("internal/controller/setup.go",
          "\t\tWatches(&platformv1alpha1.Tenant{}, r.tenantHandler()).\n",
          "")],
    ),
    (
        "claims do not watch their tier",
        "rolls a change to the tier out",
        [("internal/controller/setup.go",
          "\t\tWatches(&platformv1alpha1.ProjectTier{}, handler.EnqueueRequestsFromMapFunc(r.claimsOfTier)).\n",
          "")],
    ),
    (
        "Retain leaves owner references behind",
        "detaches it when the reclaim policy is Retain",
        [("internal/controller/delete.go", "\tobj.SetOwnerReferences(kept)\n", "")],
    ),
    (
        "nobody cleans up after a claim whose finalizer was stripped",
        "removed behind its back",
        [("internal/controller/tenant_controller.go",
          "\t\tcase apierrors.IsNotFound(err), err == nil && claim.UID != res.ClaimUID:\n\t\t\torphans[res.ClaimUID] = struct{}{}\n",
          "\t\tcase apierrors.IsNotFound(err), err == nil && claim.UID != res.ClaimUID:\n")],
    ),
    (
        "the operator's ClusterRole lacks the bind verb",
        "binds the requester to admin",
        [("config/rbac/role.yaml", "  verbs:\n  - bind\n", "  verbs:\n  - get\n")],
    ),
    (
        "the operator's ClusterRole cannot delete namespaces",
        "holds the claim until the namespace is really gone",
        [("config/rbac/role.yaml",
          "  - limitranges\n  - namespaces\n  - resourcequotas\n  verbs:\n  - create\n  - delete\n",
          "  - limitranges\n  - namespaces\n  - resourcequotas\n  verbs:\n  - create\n")],
    ),
    (
        "the admission policy does not check who is asking",
        "rejects a claim from someone who is not an owner",
        [("config/admission/tenant-ownership.yaml",
          "object.spec.tenantRef.name != params.metadata.name || variables.isOwner || variables.isAdmin",
          "true")],
    ),
]


def run(index, what, focus, edits):
    work = tempfile.mkdtemp(prefix="projectclaim-mutant-")
    try:
        shutil.copytree(REPO, work, dirs_exist_ok=True, ignore=shutil.ignore_patterns("bin", "dist", ".git"))
        try:
            for path, old, new in edits:
                replace(work, path, old, new)
        except LookupError as err:
            return f"BROKEN: {err}"

        result = subprocess.run(
            ["go", "test", "./internal/controller/", "-count=1", "-run", "TestControllers",
             "-ginkgo.focus", re.escape(focus)],
            cwd=work, capture_output=True, text=True)
        output = result.stdout + result.stderr
        ran = re.search(r"Ran (\d+) of", output)
        if "[build failed]" in output or "[setup failed]" in output:
            return "BROKEN: the mutant does not compile\n" + output[-800:]
        if ran and ran.group(1) == "0":
            return f"BROKEN: no spec matches {focus!r}"
        if result.returncode != 0:
            return "caught"
        return "SURVIVED: the tests still pass"
    finally:
        shutil.rmtree(work, ignore_errors=True)


def main():
    if not os.environ.get("KUBEBUILDER_ASSETS"):
        sys.exit("KUBEBUILDER_ASSETS is not set. Run this through `make mutation-test`.")
    only = set(sys.argv[1:])
    bad = 0
    for index, (what, focus, edits) in enumerate(MUTANTS):
        if only and str(index) not in only:
            continue
        verdict = run(index, what, focus, edits)
        bad += verdict != "caught"
        print(f"[{index:2}] {verdict.splitlines()[0]:<10}  {what}", flush=True)
        for line in verdict.splitlines()[1:]:
            print("      " + line)
    print()
    if bad:
        sys.exit(f"{bad} mutation(s) were not caught")
    print("Every mutation was caught.")


if __name__ == "__main__":
    main()
