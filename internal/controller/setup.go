package controller

import (
	"context"
	"reflect"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	platformv1alpha1 "github.com/mella7/project-claim/api/v1alpha1"
)

const (
	indexTenant = "spec.tenantRef.name"
	indexTier   = "spec.tier"
)

// CacheOptions narrows the informer cache to what the operator manages.
//
// Watching a kind caches every object of it. On a real cluster that would be
// every RoleBinding, ResourceQuota, LimitRange and NetworkPolicy there is,
// and the operator cares about a handful per project. The label selector
// makes the API server send only those. Namespaces are left unfiltered on
// purpose: the adoption guard has to see namespaces that are not ours.
func CacheOptions() cache.Options {
	managed := cache.ByObject{
		Label: labels.SelectorFromSet(labels.Set{LabelManagedBy: ManagerName}),
	}
	return cache.Options{
		ByObject: map[client.Object]cache.ByObject{
			&corev1.ResourceQuota{}:       managed,
			&corev1.LimitRange{}:          managed,
			&rbacv1.RoleBinding{}:         managed,
			&networkingv1.NetworkPolicy{}: managed,
		},
	}
}

// SetupIndexes registers the field indexes both controllers rely on. They
// turn "all claims of this tenant" and "all claims of this tier" into a cache
// lookup instead of a scan over every claim.
func SetupIndexes(ctx context.Context, mgr ctrl.Manager) error {
	indexer := mgr.GetFieldIndexer()
	if err := indexer.IndexField(ctx, &platformv1alpha1.ProjectClaim{}, indexTenant, func(obj client.Object) []string {
		return []string{obj.(*platformv1alpha1.ProjectClaim).Spec.TenantRef.Name}
	}); err != nil {
		return err
	}
	return indexer.IndexField(ctx, &platformv1alpha1.ProjectClaim{}, indexTier, func(obj client.Object) []string {
		return []string{obj.(*platformv1alpha1.ProjectClaim).Spec.Tier}
	})
}

// SetupWithManager wires up the watches.
//
// For() watches the claims themselves. Each Owns() watches a kind of child
// object and maps an event on it back to the claim named in its controller
// reference. That is the whole drift correction mechanism: delete or edit a
// managed object and its claim is reconciled within moments.
//
// The two Watches() cover what a claim depends on without owning: a change
// to a tier reaches every project on that tier, and a change to a tenant
// wakes the claims that are waiting on it.
func (r *ProjectClaimReconciler) SetupWithManager(mgr ctrl.Manager) error {
	workers := r.MaxConcurrentReconciles
	if workers < 1 {
		workers = 1
	}
	return ctrl.NewControllerManagedBy(mgr).
		Named("projectclaim").
		For(&platformv1alpha1.ProjectClaim{}).
		Owns(&corev1.Namespace{}).
		Owns(&corev1.ResourceQuota{}, builder.WithPredicates(ignoreQuotaUsage())).
		Owns(&corev1.LimitRange{}).
		Owns(&rbacv1.RoleBinding{}).
		Owns(&networkingv1.NetworkPolicy{}).
		Watches(&platformv1alpha1.ProjectTier{}, handler.EnqueueRequestsFromMapFunc(r.claimsOfTier)).
		Watches(&platformv1alpha1.Tenant{}, r.tenantHandler()).
		// The workqueue never hands the same claim to two workers at once,
		// so more workers means different claims in parallel, not races.
		WithOptions(controller.Options{MaxConcurrentReconciles: workers}).
		Complete(r)
}

// ignoreQuotaUsage drops the updates the quota controller makes to
// status.used every time a pod starts or stops. Without it, each of those
// would cause a reconcile that finds nothing to do.
func ignoreQuotaUsage() predicate.Predicate {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			before, ok1 := e.ObjectOld.(*corev1.ResourceQuota)
			after, ok2 := e.ObjectNew.(*corev1.ResourceQuota)
			if !ok1 || !ok2 {
				return true
			}
			return !apiequality.Semantic.DeepEqual(before.Spec, after.Spec) ||
				!reflect.DeepEqual(before.Labels, after.Labels) ||
				!reflect.DeepEqual(before.OwnerReferences, after.OwnerReferences) ||
				!before.DeletionTimestamp.Equal(after.DeletionTimestamp)
		},
	}
}

func (r *ProjectClaimReconciler) claimsOfTier(ctx context.Context, tier client.Object) []reconcile.Request {
	claims := &platformv1alpha1.ProjectClaimList{}
	if err := r.List(ctx, claims, client.MatchingFields{indexTier: tier.GetName()}); err != nil {
		log.FromContext(ctx).Error(err, "listing claims of tier", "tier", tier.GetName())
		return nil
	}
	requests := make([]reconcile.Request, 0, len(claims.Items))
	for i := range claims.Items {
		requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&claims.Items[i])})
	}
	return requests
}

// tenantHandler decides which claims a tenant event should wake.
//
// Every reservation is a write to the tenant's status, so a tenant with many
// projects changes often. Waking all its claims each time would cost a
// reconcile per claim per reservation. A status change can only matter to
// claims that are not settled, so only those are enqueued. A spec change, for
// example a larger budget or another list of allowed tiers, goes to all.
func (r *ProjectClaimReconciler) tenantHandler() handler.EventHandler {
	enqueue := func(ctx context.Context, tenant client.Object, all bool, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
		claims := &platformv1alpha1.ProjectClaimList{}
		if err := r.List(ctx, claims, client.MatchingFields{indexTenant: tenant.GetName()}); err != nil {
			log.FromContext(ctx).Error(err, "listing claims of tenant", "tenant", tenant.GetName())
			return
		}
		for i := range claims.Items {
			if all || !IsReady(&claims.Items[i]) {
				q.Add(reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&claims.Items[i])})
			}
		}
	}
	return handler.Funcs{
		CreateFunc: func(ctx context.Context, e event.CreateEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			enqueue(ctx, e.Object, true, q)
		},
		UpdateFunc: func(ctx context.Context, e event.UpdateEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			enqueue(ctx, e.ObjectNew, e.ObjectOld.GetGeneration() != e.ObjectNew.GetGeneration(), q)
		},
		DeleteFunc: func(ctx context.Context, e event.DeleteEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			enqueue(ctx, e.Object, true, q)
		},
	}
}
