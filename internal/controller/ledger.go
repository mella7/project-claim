package controller

import (
	"fmt"
	"math"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"

	platformv1alpha1 "github.com/mella7/project-claim/api/v1alpha1"
)

// The tenant's status is a ledger of reservations. This file holds the
// arithmetic on that ledger. Like desired.go it is pure, so the budget rules
// can be tested without a cluster.

// refusal explains why a claim cannot be admitted right now.
type refusal struct {
	Reason  string
	Message string
}

func refuse(reason, format string, args ...any) *refusal {
	return &refusal{Reason: reason, Message: fmt.Sprintf(format, args...)}
}

// findReservation returns the index of the reservation held by this exact
// incarnation of the claim, or -1.
func findReservation(status *platformv1alpha1.TenantStatus, claim *platformv1alpha1.ProjectClaim) int {
	for i := range status.Reservations {
		if status.Reservations[i].ClaimUID == claim.UID {
			return i
		}
	}
	return -1
}

// checkBudget decides whether the claim may hold `want` against the tenant's
// budget, given what every other claim already holds. It does not modify
// anything.
func checkBudget(tenant *platformv1alpha1.Tenant, claim *platformv1alpha1.ProjectClaim, want corev1.ResourceList) *refusal {
	mine := findReservation(&tenant.Status, claim)

	if limit := tenant.Spec.MaxProjects; limit != nil && mine < 0 {
		if len(tenant.Status.Reservations) >= int(*limit) {
			return refuse(ReasonProjectLimitReached,
				"tenant %q already has %d of %d projects", tenant.Name, len(tenant.Status.Reservations), *limit)
		}
	}

	// Walk the budget in a fixed order so the same shortfall always produces
	// the same message, and with it the same status.
	names := make([]string, 0, len(tenant.Spec.QuotaBudget))
	for name := range tenant.Spec.QuotaBudget {
		names = append(names, string(name))
	}
	sort.Strings(names)

	var short []string
	for _, n := range names {
		name := corev1.ResourceName(n)
		budget := tenant.Spec.QuotaBudget[name]

		asked, bounded := want[name]
		if !bounded {
			// A project without a quota on a budgeted resource could use any
			// amount of it, so the budget would mean nothing.
			return refuse(ReasonBudgetResourceUnbounded,
				"tenant %q budgets %q, so the project needs a quota for it (set it on the tier or the claim)", tenant.Name, name)
		}

		total := asked.DeepCopy()
		for i := range tenant.Status.Reservations {
			if i == mine {
				continue
			}
			if held, ok := tenant.Status.Reservations[i].Quota[name]; ok {
				total.Add(held)
			}
		}
		if total.Cmp(budget) > 0 {
			free := budget.DeepCopy()
			free.Sub(total)
			free.Add(asked)
			if free.Sign() < 0 {
				free.Set(0)
			}
			short = append(short, fmt.Sprintf("%s: wants %s, %s free of %s", name, asked.String(), free.String(), budget.String()))
		}
	}
	if len(short) > 0 {
		return refuse(ReasonQuotaBudgetExceeded,
			"tenant %q is out of budget (%s)", tenant.Name, strings.Join(short, "; "))
	}
	return nil
}

// setReservation records `want` for the claim and reports whether the status
// changed.
func setReservation(status *platformv1alpha1.TenantStatus, claim *platformv1alpha1.ProjectClaim, want corev1.ResourceList) bool {
	if i := findReservation(status, claim); i >= 0 {
		if apiequality.Semantic.DeepEqual(status.Reservations[i].Quota, want) {
			return false
		}
		status.Reservations[i].Quota = want.DeepCopy()
		summarize(status)
		return true
	}
	// A reservation under the same name but another UID belongs to an
	// earlier incarnation of the claim. Replace it.
	kept := make([]platformv1alpha1.Reservation, 0, len(status.Reservations)+1)
	for _, r := range status.Reservations {
		if r.ClaimName != claim.Name {
			kept = append(kept, r)
		}
	}
	kept = append(kept, platformv1alpha1.Reservation{
		ClaimName: claim.Name,
		ClaimUID:  claim.UID,
		Quota:     want.DeepCopy(),
	})
	status.Reservations = kept
	summarize(status)
	return true
}

// dropReservation removes the claim's reservation and reports whether there
// was one.
func dropReservation(status *platformv1alpha1.TenantStatus, claim *platformv1alpha1.ProjectClaim) bool {
	i := findReservation(status, claim)
	if i < 0 {
		return false
	}
	status.Reservations = append(status.Reservations[:i], status.Reservations[i+1:]...)
	summarize(status)
	return true
}

// summarize recomputes the totals from the reservations and puts the list in
// a stable order.
func summarize(status *platformv1alpha1.TenantStatus) {
	sort.Slice(status.Reservations, func(i, j int) bool {
		return status.Reservations[i].ClaimName < status.Reservations[j].ClaimName
	})
	allocated := corev1.ResourceList{}
	for _, r := range status.Reservations {
		for name, q := range r.Quota {
			sum := allocated[name]
			sum.Add(q)
			allocated[name] = sum
		}
	}
	if len(allocated) == 0 {
		allocated = nil
	}
	status.Allocated = allocated
	status.ProjectCount = int32(min(len(status.Reservations), math.MaxInt32))
}
