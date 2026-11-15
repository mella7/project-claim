package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	platformv1alpha1 "github.com/mella7/project-claim/api/v1alpha1"
)

// reserve books the claim's quota against the tenant budget before anything
// is created.
//
// Checking the budget against the informer cache would be a race: two claims
// reconciled at the same moment would both see the old total and both get in.
// Instead the reservation is written into the tenant's status with an Update,
// which carries the resourceVersion that was read. If another claim got there
// first, the API server answers with a conflict, the tenant is read again and
// the check is repeated against the new total. The API server decides the
// order, so the budget holds no matter how many workers or replicas run.
func (r *ProjectClaimReconciler) reserve(ctx context.Context, claim *platformv1alpha1.ProjectClaim, cached *platformv1alpha1.Tenant, want corev1.ResourceList) (*refusal, error) {
	// A claim that owns a namespace keeps its reservation for as long as it
	// lives, so the cached copy can be trusted to say "already booked".
	// That keeps a settled claim from costing an API call per reconcile.
	if claim.Status.NamespaceName != "" {
		if i := findReservation(&cached.Status, claim); i >= 0 &&
			apiequality.Semantic.DeepEqual(cached.Status.Reservations[i].Quota, want) {
			return nil, nil
		}
	}

	var why *refusal
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		why = nil
		tenant := &platformv1alpha1.Tenant{}
		if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(cached), tenant); err != nil {
			if apierrors.IsNotFound(err) {
				why = refuse(ReasonTenantNotFound, "Tenant %q does not exist", cached.Name)
				return nil
			}
			return err
		}
		if why = checkBudget(tenant, claim, want); why != nil {
			return nil
		}
		if !setReservation(&tenant.Status, claim, want) {
			return nil
		}
		return r.Status().Update(ctx, tenant)
	})
	if err != nil {
		return nil, fmt.Errorf("reserving budget: %w", err)
	}
	return why, nil
}

// release gives the claim's reservation back. With force unset it trusts the
// cache to say there is nothing to release, which is the common case for a
// claim that is waiting. The delete path sets force and always asks the API
// server, because that is the last chance to release.
func (r *ProjectClaimReconciler) release(ctx context.Context, claim *platformv1alpha1.ProjectClaim, force bool) error {
	key := client.ObjectKey{Name: claim.Spec.TenantRef.Name}

	if !force {
		cached := &platformv1alpha1.Tenant{}
		if err := r.Get(ctx, key, cached); err != nil {
			return client.IgnoreNotFound(err)
		}
		if findReservation(&cached.Status, claim) < 0 {
			return nil
		}
	}

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		tenant := &platformv1alpha1.Tenant{}
		if err := r.APIReader.Get(ctx, key, tenant); err != nil {
			return client.IgnoreNotFound(err)
		}
		if !dropReservation(&tenant.Status, claim) {
			return nil
		}
		return r.Status().Update(ctx, tenant)
	})
	if err != nil {
		return fmt.Errorf("releasing budget: %w", err)
	}
	return nil
}
