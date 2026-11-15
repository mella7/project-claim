package controller

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	platformv1alpha1 "github.com/mella7/project-claim/api/v1alpha1"
)

// The budget rules and the desired-state builders are pure functions, so
// they are tested here as plain table tests without a cluster.

func cpu(s string) corev1.ResourceList {
	return corev1.ResourceList{corev1.ResourceRequestsCPU: qty(s)}
}

func claimWithUID(name string) *platformv1alpha1.ProjectClaim {
	return &platformv1alpha1.ProjectClaim{ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID("uid-" + name)}}
}

func tenantHolding(budget string, held map[string]string) *platformv1alpha1.Tenant {
	tenant := &platformv1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "acme"}}
	if budget != "" {
		tenant.Spec.QuotaBudget = cpu(budget)
	}
	for name, amount := range held {
		setReservation(&tenant.Status, claimWithUID(name), cpu(amount))
	}
	return tenant
}

func TestCheckBudget(t *testing.T) {
	two := int32(2)
	cases := []struct {
		name   string
		tenant *platformv1alpha1.Tenant
		claim  string
		want   corev1.ResourceList
		reason string
		detail string
	}{
		{name: "no budget means no limit", tenant: tenantHolding("", nil), claim: "a", want: cpu("1000")},
		{name: "fits exactly", tenant: tenantHolding("8", map[string]string{"a": "6"}), claim: "b", want: cpu("2")},
		{
			name:   "one millicore too much",
			tenant: tenantHolding("8", map[string]string{"a": "6"}), claim: "b", want: cpu("2001m"),
			reason: ReasonQuotaBudgetExceeded, detail: "wants 2001m, 2 free of 8",
		},
		{
			// A claim's own reservation must not count against it, or no
			// claim could ever be reconciled twice.
			name:   "own reservation is not counted twice",
			tenant: tenantHolding("8", map[string]string{"a": "6", "b": "2"}), claim: "b", want: cpu("2"),
		},
		{
			name:   "growing within what is free",
			tenant: tenantHolding("8", map[string]string{"a": "4", "b": "2"}), claim: "b", want: cpu("4"),
		},
		{
			name:   "growing beyond what is free",
			tenant: tenantHolding("8", map[string]string{"a": "4", "b": "2"}), claim: "b", want: cpu("5"),
			reason: ReasonQuotaBudgetExceeded, detail: "wants 5, 4 free of 8",
		},
		{
			name:   "budgeted resource without a quota",
			tenant: tenantHolding("8", nil), claim: "a", want: corev1.ResourceList{corev1.ResourcePods: qty("10")},
			reason: ReasonBudgetResourceUnbounded,
		},
		{
			name: "project limit reached",
			tenant: func() *platformv1alpha1.Tenant {
				tenant := tenantHolding("", map[string]string{"a": "1", "b": "1"})
				tenant.Spec.MaxProjects = &two
				return tenant
			}(),
			claim: "c", want: cpu("1"), reason: ReasonProjectLimitReached,
		},
		{
			name: "project limit does not block a claim that is already in",
			tenant: func() *platformv1alpha1.Tenant {
				tenant := tenantHolding("", map[string]string{"a": "1", "b": "1"})
				tenant.Spec.MaxProjects = &two
				return tenant
			}(),
			claim: "b", want: cpu("1"),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := checkBudget(tc.tenant, claimWithUID(tc.claim), tc.want)
			switch {
			case tc.reason == "" && got != nil:
				t.Fatalf("expected admission, got %s: %s", got.Reason, got.Message)
			case tc.reason != "" && got == nil:
				t.Fatalf("expected refusal %s, got admission", tc.reason)
			case tc.reason != "" && got.Reason != tc.reason:
				t.Fatalf("expected refusal %s, got %s", tc.reason, got.Reason)
			case tc.detail != "" && !strings.Contains(got.Message, tc.detail):
				t.Fatalf("message %q does not contain %q", got.Message, tc.detail)
			}
		})
	}
}

func TestReservations(t *testing.T) {
	status := &platformv1alpha1.TenantStatus{}
	a, b := claimWithUID("a"), claimWithUID("b")

	if !setReservation(status, b, cpu("2")) || !setReservation(status, a, cpu("3")) {
		t.Fatal("new reservations must report a change")
	}
	if setReservation(status, a, cpu("3000m")) {
		t.Fatal("the same amount in another notation is not a change")
	}
	if status.Reservations[0].ClaimName != "a" {
		t.Fatal("reservations must be kept in a stable order")
	}
	if total := status.Allocated[corev1.ResourceRequestsCPU]; total.Cmp(qty("5")) != 0 || status.ProjectCount != 2 {
		t.Fatalf("totals are off: %s cpu, %d projects", total.String(), status.ProjectCount)
	}

	// Same name, new UID: the claim was deleted and created again. The new
	// one must replace the old reservation, not sit next to it.
	reborn := claimWithUID("a")
	reborn.UID = "uid-a-second-life"
	setReservation(status, reborn, cpu("1"))
	if status.ProjectCount != 2 || findReservation(status, a) != -1 || findReservation(status, reborn) < 0 {
		t.Fatalf("a recreated claim must replace its predecessor, got %+v", status.Reservations)
	}

	if !dropReservation(status, b) || dropReservation(status, b) {
		t.Fatal("dropping must report whether there was something to drop")
	}
	dropReservation(status, reborn)
	if status.ProjectCount != 0 || status.Allocated != nil {
		t.Fatalf("an empty ledger must have empty totals, got %+v", status)
	}
}

func TestEffectiveQuotaDoesNotAliasTheTier(t *testing.T) {
	tier := &platformv1alpha1.ProjectTier{Spec: platformv1alpha1.ProjectTierSpec{
		Quota: corev1.ResourceList{corev1.ResourcePods: qty("20"), corev1.ResourceRequestsCPU: qty("2")},
	}}
	claim := &platformv1alpha1.ProjectClaim{Spec: platformv1alpha1.ProjectClaimSpec{
		Quota: corev1.ResourceList{corev1.ResourcePods: qty("80")},
	}}

	got := EffectiveQuota(tier, claim)
	if pods := got[corev1.ResourcePods]; pods.Cmp(qty("80")) != 0 {
		t.Fatalf("override lost: pods=%s", pods.String())
	}
	if c := got[corev1.ResourceRequestsCPU]; c.Cmp(qty("2")) != 0 {
		t.Fatalf("tier value lost: cpu=%s", c.String())
	}

	got[corev1.ResourceRequestsCPU] = qty("999")
	if c := tier.Spec.Quota[corev1.ResourceRequestsCPU]; c.Cmp(qty("2")) != 0 {
		t.Fatal("changing the result changed the tier")
	}
}

func TestMutateNamespaceIsStable(t *testing.T) {
	tier := &platformv1alpha1.ProjectTier{}
	claim := &platformv1alpha1.ProjectClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "shop"},
		Spec: platformv1alpha1.ProjectClaimSpec{
			ProjectName: "shop",
			ExtraLabels: map[string]string{"team": "shop", LabelPodSecurityEnforce: "privileged"},
		},
	}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"foreign": "kept"}}}

	mutateNamespace(ns, claim, tier)
	first := ns.DeepCopy()
	mutateNamespace(ns, claim, tier)

	if ns.Labels[LabelPodSecurityEnforce] != "baseline" {
		t.Fatalf("a claim must not be able to set the Pod Security level, got %q", ns.Labels[LabelPodSecurityEnforce])
	}
	if ns.Annotations[AnnotationManagedLabels] != first.Annotations[AnnotationManagedLabels] || len(ns.Labels) != len(first.Labels) {
		t.Fatal("applying the same claim twice must give the same namespace")
	}

	claim.Spec.ExtraLabels = nil
	mutateNamespace(ns, claim, tier)
	if _, still := ns.Labels["team"]; still {
		t.Fatal("a label removed from the claim must be removed from the namespace")
	}
	if ns.Labels["foreign"] != "kept" {
		t.Fatal("labels set by others must survive")
	}
}
