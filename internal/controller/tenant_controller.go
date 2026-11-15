package controller

import (
	"context"
	"fmt"

	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	platformv1alpha1 "github.com/mella7/project-claim/api/v1alpha1"
)

// TenantReconciler keeps the tenant's ledger honest.
//
// Claims write and remove their own reservations. This controller covers the
// case they cannot: a claim that disappeared without running its cleanup,
// because somebody stripped its finalizer by hand. Its reservation would
// otherwise hold budget forever.
type TenantReconciler struct {
	client.Client
	APIReader client.Reader
}

// +kubebuilder:rbac:groups=platform.mella7.me,resources=tenants,verbs=get;list;watch
// +kubebuilder:rbac:groups=platform.mella7.me,resources=tenants/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=platform.mella7.me,resources=projectclaims,verbs=get;list;watch

func (r *TenantReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	tenant := &platformv1alpha1.Tenant{}
	if err := r.Get(ctx, req.NamespacedName, tenant); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	orphans, err := r.orphans(ctx, tenant)
	if err != nil {
		return ctrl.Result{}, err
	}
	if len(orphans) == 0 && tenant.Status.ObservedGeneration == tenant.Generation {
		return ctrl.Result{}, nil
	}

	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		live := &platformv1alpha1.Tenant{}
		if err := r.APIReader.Get(ctx, req.NamespacedName, live); err != nil {
			return client.IgnoreNotFound(err)
		}
		before := live.Status.DeepCopy()

		kept := live.Status.Reservations[:0]
		for _, res := range live.Status.Reservations {
			if _, gone := orphans[res.ClaimUID]; !gone {
				kept = append(kept, res)
			}
		}
		live.Status.Reservations = kept
		summarize(&live.Status)
		live.Status.ObservedGeneration = live.Generation

		if apiequality.Semantic.DeepEqual(*before, live.Status) {
			return nil
		}
		return r.Status().Update(ctx, live)
	})
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("updating tenant ledger: %w", err)
	}
	if len(orphans) > 0 {
		log.FromContext(ctx).Info("dropped reservations of claims that no longer exist", "count", len(orphans))
	}
	return ctrl.Result{}, nil
}

// orphans returns the reservations whose claim is gone. The cache is asked
// first because it is free, but its word is not taken for it: a claim created
// a moment ago may not be in the cache yet, and dropping its reservation
// would hand the same budget out twice. Only the API server's "not found"
// counts.
func (r *TenantReconciler) orphans(ctx context.Context, tenant *platformv1alpha1.Tenant) (map[types.UID]struct{}, error) {
	orphans := map[types.UID]struct{}{}
	for _, res := range tenant.Status.Reservations {
		key := client.ObjectKey{Name: res.ClaimName}
		claim := &platformv1alpha1.ProjectClaim{}

		err := r.Get(ctx, key, claim)
		if err == nil && claim.UID == res.ClaimUID {
			continue
		}
		if err != nil && !apierrors.IsNotFound(err) {
			return nil, err
		}

		err = r.APIReader.Get(ctx, key, claim)
		switch {
		case apierrors.IsNotFound(err), err == nil && claim.UID != res.ClaimUID:
			orphans[res.ClaimUID] = struct{}{}
		case err != nil:
			return nil, err
		}
	}
	return orphans, nil
}

func (r *TenantReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Only the removal of a claim can orphan a reservation, so that is the
	// only claim event this controller wants.
	deletions := predicate.Funcs{
		CreateFunc:  func(event.CreateEvent) bool { return false },
		UpdateFunc:  func(event.UpdateEvent) bool { return false },
		GenericFunc: func(event.GenericEvent) bool { return false },
		DeleteFunc:  func(event.DeleteEvent) bool { return true },
	}
	tenantOfClaim := func(_ context.Context, obj client.Object) []reconcile.Request {
		claim, ok := obj.(*platformv1alpha1.ProjectClaim)
		if !ok {
			return nil
		}
		return []reconcile.Request{{NamespacedName: client.ObjectKey{Name: claim.Spec.TenantRef.Name}}}
	}
	return ctrl.NewControllerManagedBy(mgr).
		Named("tenant").
		For(&platformv1alpha1.Tenant{}).
		Watches(&platformv1alpha1.ProjectClaim{}, handler.EnqueueRequestsFromMapFunc(tenantOfClaim), builder.WithPredicates(deletions)).
		Complete(r)
}
