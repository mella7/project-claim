package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	platformv1alpha1 "github.com/mella7/project-claim/api/v1alpha1"
)

// namespaceRetryInterval is how long to wait before looking again at a
// namespace that is in the way and on its way out. That namespace is not
// ours, so no watch event will tell us when it is gone.
const namespaceRetryInterval = 10 * time.Second

// ProjectClaimReconciler turns a ProjectClaim into a namespace with its
// guardrails and keeps the two in sync.
type ProjectClaimReconciler struct {
	// Client reads from the informer cache and writes to the API server.
	client.Client

	// APIReader reads straight from the API server. It is used for the few
	// decisions that must not be made on a stale cache: reserving budget and
	// deciding that cleanup is complete.
	APIReader client.Reader

	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	Clock    clock.PassiveClock

	MaxConcurrentReconciles int
}

// admission is what a claim gets once tenant and tier policy let it through.
type admission struct {
	tier  *platformv1alpha1.ProjectTier
	quota corev1.ResourceList
}

// +kubebuilder:rbac:groups=platform.mella7.me,resources=projectclaims,verbs=get;list;watch;update;patch;delete
// +kubebuilder:rbac:groups=platform.mella7.me,resources=projectclaims/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=platform.mella7.me,resources=projectclaims/finalizers,verbs=update
// +kubebuilder:rbac:groups=platform.mella7.me,resources=projecttiers,verbs=get;list;watch
// "administer" is not an API verb. The tenant ownership admission policy asks
// the authorizer for it to tell platform admins and this operator apart from
// ordinary users, so the operator can keep editing finalizers on any claim.
// +kubebuilder:rbac:groups=platform.mella7.me,resources=tenants,verbs=get;list;watch;administer
// +kubebuilder:rbac:groups=platform.mella7.me,resources=tenants/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=resourcequotas;limitranges,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=rolebindings,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete
// The API server refuses to let anyone create a RoleBinding to a role whose
// permissions they do not hold themselves. The bind verb is the supported way
// around that, limited here to the three roles the operator hands out.
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles,verbs=bind,resourceNames=admin;edit;view

// Reconcile is called with nothing but the name of a claim. It is not told
// what changed or why, so it works out everything from the current state and
// has to be safe to run any number of times.
func (r *ProjectClaimReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	claim := &platformv1alpha1.ProjectClaim{}
	if err := r.Get(ctx, req.NamespacedName, claim); err != nil {
		// Gone means the finalizer was removed and cleanup already ran.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !claim.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, claim)
	}

	// The finalizer goes on before anything is created. Were it the other
	// way round, a crash in between would leave a namespace that no delete
	// would ever clean up.
	if !controllerutil.ContainsFinalizer(claim, Finalizer) {
		patch := client.MergeFromWithOptions(claim.DeepCopy(), client.MergeFromWithOptimisticLock{})
		controllerutil.AddFinalizer(claim, Finalizer)
		if err := r.Patch(ctx, claim, patch); err != nil {
			return ctrl.Result{}, fmt.Errorf("adding finalizer: %w", err)
		}
	}

	// Every step below only edits the in-memory status. It is written once,
	// at the end, and only if it differs from what is stored.
	before := claim.DeepCopy()
	result, err := r.reconcileNormal(ctx, claim)
	if statusErr := r.patchStatus(ctx, before, claim); statusErr != nil {
		return ctrl.Result{}, errors.Join(err, statusErr)
	}
	return result, err
}

func (r *ProjectClaimReconciler) reconcileNormal(ctx context.Context, claim *platformv1alpha1.ProjectClaim) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	tier, err := r.getTier(ctx, claim.Spec.Tier)
	if err != nil {
		r.markError(claim, err)
		return ctrl.Result{}, err
	}

	result := ctrl.Result{}
	claim.Status.ExpiresAt = expiry(claim, tier)
	if expires := claim.Status.ExpiresAt; expires != nil {
		left := expires.Sub(r.Clock.Now())
		if left <= 0 {
			logger.Info("lifetime ran out, deleting the claim", "expiredAt", expires.Time)
			r.event(claim, corev1.EventTypeNormal, "Expired", "lifetime ran out, deleting the claim")
			// From here on the delete path takes over, like for any other delete.
			err := r.Delete(ctx, claim, client.Preconditions{UID: &claim.UID})
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
		result.RequeueAfter = left
	}

	granted, why, err := r.admit(ctx, claim, tier)
	if err != nil {
		r.markError(claim, err)
		return ctrl.Result{}, err
	}
	if why != nil {
		// A claim that owns no namespace must not sit on budget.
		if claim.Status.NamespaceName == "" {
			if err := r.release(ctx, claim, false); err != nil {
				r.markError(claim, err)
				return ctrl.Result{}, err
			}
		}
		r.markRefused(claim, why)
		if why.Reason == ReasonNamespaceTerminating {
			result.RequeueAfter = namespaceRetryInterval
		}
		// No error is returned: retrying cannot change the outcome. The
		// watches on Tenant and ProjectTier bring the claim back when it can.
		return result, nil
	}
	setCondition(claim, platformv1alpha1.ConditionAdmitted, metav1.ConditionTrue, ReasonAdmitted,
		"tenant and tier policy passed and the quota is reserved")

	if err := r.provision(ctx, claim, granted); err != nil {
		r.markError(claim, err)
		// Returning the error hands the retry to the workqueue, which backs
		// off exponentially per claim.
		return ctrl.Result{}, err
	}

	claim.Status.Phase = platformv1alpha1.PhaseReady
	claim.Status.ObservedGeneration = claim.Generation
	setCondition(claim, platformv1alpha1.ConditionReady, metav1.ConditionTrue, ReasonProvisioned,
		"the namespace and its guardrails match the claim")
	return result, nil
}

// admit checks the claim against its tier and tenant and reserves its quota.
// It returns a refusal when the claim has to wait or cannot be served, and an
// error only for failures worth retrying.
func (r *ProjectClaimReconciler) admit(ctx context.Context, claim *platformv1alpha1.ProjectClaim, tier *platformv1alpha1.ProjectTier) (*admission, *refusal, error) {
	if tier == nil {
		return nil, refuse(ReasonTierNotFound, "ProjectTier %q does not exist", claim.Spec.Tier), nil
	}

	tenant := &platformv1alpha1.Tenant{}
	if err := r.Get(ctx, client.ObjectKey{Name: claim.Spec.TenantRef.Name}, tenant); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, refuse(ReasonTenantNotFound, "Tenant %q does not exist", claim.Spec.TenantRef.Name), nil
		}
		return nil, nil, fmt.Errorf("reading tenant: %w", err)
	}

	if allowed := tenant.Spec.AllowedTiers; len(allowed) > 0 && !slices.Contains(allowed, tier.Name) {
		return nil, refuse(ReasonTierNotAllowed, "tenant %q may not use tier %q (allowed: %s)",
			tenant.Name, tier.Name, strings.Join(allowed, ", ")), nil
	}
	if prefix := tenant.Spec.NamespacePrefix; prefix != "" && !strings.HasPrefix(claim.Spec.ProjectName, prefix) {
		return nil, refuse(ReasonNamespacePrefixMismatch, "projects of tenant %q must be named %s*", tenant.Name, prefix), nil
	}
	if tier.Spec.RequireNetworkIsolation && !claim.NetworkIsolationEnabled() {
		return nil, refuse(ReasonNetworkIsolationRequired, "tier %q does not allow turning network isolation off", tier.Name), nil
	}

	// Look at the namespace before reserving anything, so that a claim that
	// can never be served does not hold budget.
	why, err := r.inspectNamespace(ctx, claim)
	if err != nil || why != nil {
		return nil, why, err
	}

	quota := EffectiveQuota(tier, claim)
	why, err = r.reserve(ctx, claim, tenant, quota)
	if err != nil || why != nil {
		return nil, why, err
	}
	return &admission{tier: tier, quota: quota}, nil, nil
}

// inspectNamespace is the adoption guard. Namespace names are one flat,
// cluster-wide space, and nothing stops a claim from asking for a name that
// is taken. Without this check the operator would put its owner reference on
// somebody else's namespace and later delete it together with the claim.
func (r *ProjectClaimReconciler) inspectNamespace(ctx context.Context, claim *platformv1alpha1.ProjectClaim) (*refusal, error) {
	ns := &corev1.Namespace{}
	err := r.Get(ctx, client.ObjectKey{Name: claim.Spec.ProjectName}, ns)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading namespace: %w", err)
	}
	if !ns.DeletionTimestamp.IsZero() {
		return refuse(ReasonNamespaceTerminating, "namespace %q is being deleted, waiting for it to go", ns.Name), nil
	}
	if ownedBy(ns, claim) || adoptable(ns, claim) {
		return nil, nil
	}
	return refuse(ReasonNamespaceConflict,
		"namespace %q already exists and does not belong to this claim; to hand it over, annotate it with %s=%s",
		ns.Name, AnnotationAdoptInto, claim.Name), nil
}

// ownedBy compares UIDs, not names. A claim that was deleted and created
// again under the same name is a different object and must not inherit what
// its predecessor owned.
func ownedBy(obj client.Object, claim *platformv1alpha1.ProjectClaim) bool {
	ref := metav1.GetControllerOf(obj)
	return ref != nil && ref.UID == claim.UID
}

// adoptable reports whether an existing namespace was explicitly handed to
// this claim. That is how namespaces that predate the operator are brought
// under management.
func adoptable(ns *corev1.Namespace, claim *platformv1alpha1.ProjectClaim) bool {
	return metav1.GetControllerOf(ns) == nil && ns.Annotations[AnnotationAdoptInto] == claim.Name
}

// provision creates or corrects every object of the project. Each step is
// idempotent: once the cluster matches, running it again reads from the cache
// and writes nothing.
func (r *ProjectClaimReconciler) provision(ctx context.Context, claim *platformv1alpha1.ProjectClaim, granted *admission) error {
	if claim.Status.NamespaceName == "" {
		claim.Status.Phase = platformv1alpha1.PhaseProvisioning
	}

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: claim.Spec.ProjectName}}
	if err := r.apply(ctx, claim, ns, func() { mutateNamespace(ns, claim, granted.tier) }); err != nil {
		return err
	}
	claim.Status.NamespaceName = ns.Name

	quota := &corev1.ResourceQuota{ObjectMeta: metav1.ObjectMeta{Name: QuotaName, Namespace: ns.Name}}
	if err := r.apply(ctx, claim, quota, func() { quota.Spec.Hard = granted.quota.DeepCopy() }); err != nil {
		return err
	}
	claim.Status.AppliedQuota = granted.quota.DeepCopy()

	limits := &corev1.LimitRange{ObjectMeta: metav1.ObjectMeta{Name: LimitRangeName, Namespace: ns.Name}}
	if hasContainerDefaults(granted.tier) {
		// Assign the whole spec. Appending to the existing list would add an
		// entry on every reconcile.
		if err := r.apply(ctx, claim, limits, func() { limits.Spec = limitRangeSpec(granted.tier) }); err != nil {
			return err
		}
	} else if err := r.remove(ctx, claim, limits); err != nil {
		return err
	}

	for _, want := range desiredRoleBindings(claim) {
		if err := r.ensureRoleBinding(ctx, claim, ns.Name, want); err != nil {
			return err
		}
	}

	policy := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: NetworkPolicyName, Namespace: ns.Name}}
	if claim.NetworkIsolationEnabled() {
		return r.apply(ctx, claim, policy, func() { policy.Spec = networkPolicySpec(claim) })
	}
	// Bringing the cluster in line with the spec also means removing what
	// the spec no longer asks for.
	return r.remove(ctx, claim, policy)
}

func (r *ProjectClaimReconciler) ensureRoleBinding(ctx context.Context, claim *platformv1alpha1.ProjectClaim, namespace string, want roleBinding) error {
	binding := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: want.Name, Namespace: namespace}}
	if len(want.Subjects) == 0 {
		return r.remove(ctx, claim, binding)
	}
	err := r.apply(ctx, claim, binding, func() {
		binding.Subjects = want.Subjects
		binding.RoleRef = rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: want.ClusterRole}
	})
	if apierrors.IsInvalid(err) {
		// roleRef cannot be changed in place. A binding under our name that
		// points at another role has to be replaced; the retry creates the
		// right one.
		if delErr := r.Delete(ctx, binding); client.IgnoreNotFound(delErr) != nil {
			return errors.Join(err, delErr)
		}
	}
	return err
}

// expiry returns when the claim should delete itself, or nil if it has no
// lifetime. The result is cut to whole seconds because that is the precision
// the API server stores; a finer value would never compare equal to the
// stored one and would be rewritten on every reconcile.
func expiry(claim *platformv1alpha1.ProjectClaim, tier *platformv1alpha1.ProjectTier) *metav1.Time {
	ttl := claim.Spec.TTL
	if ttl == nil && tier != nil {
		ttl = tier.Spec.DefaultTTL
	}
	if ttl == nil || ttl.Duration <= 0 {
		return nil
	}
	at := metav1.NewTime(claim.CreationTimestamp.Add(ttl.Duration).Truncate(time.Second))
	return &at
}

func (r *ProjectClaimReconciler) getTier(ctx context.Context, name string) (*platformv1alpha1.ProjectTier, error) {
	tier := &platformv1alpha1.ProjectTier{}
	if err := r.Get(ctx, client.ObjectKey{Name: name}, tier); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading tier: %w", err)
	}
	return tier, nil
}

// markRefused records why the claim was not admitted. What that means for
// the phase depends on whether the claim already owns a namespace.
func (r *ProjectClaimReconciler) markRefused(claim *platformv1alpha1.ProjectClaim, why *refusal) {
	if previous := findCondition(claim, platformv1alpha1.ConditionAdmitted); previous == nil || previous.Reason != why.Reason {
		// Only on a change of reason. A waiting claim is reconciled often,
		// and repeating the same event each time would bury the useful ones.
		r.event(claim, corev1.EventTypeWarning, why.Reason, why.Message)
	}

	setCondition(claim, platformv1alpha1.ConditionAdmitted, metav1.ConditionFalse, why.Reason, why.Message)
	claim.Status.ObservedGeneration = claim.Generation

	switch {
	case claim.Status.NamespaceName != "":
		// Never tear down a running project because a later change was
		// refused. Keep what was admitted and say so.
		claim.Status.Phase = platformv1alpha1.PhaseDegraded
		setCondition(claim, platformv1alpha1.ConditionReady, metav1.ConditionFalse, ReasonSpecNotApplied,
			"the last admitted configuration stays in place: "+why.Message)
	case why.Reason == ReasonNamespaceConflict:
		claim.Status.Phase = platformv1alpha1.PhaseFailed
		setCondition(claim, platformv1alpha1.ConditionReady, metav1.ConditionFalse, ReasonNotAdmitted, why.Message)
	default:
		claim.Status.Phase = platformv1alpha1.PhasePending
		setCondition(claim, platformv1alpha1.ConditionReady, metav1.ConditionFalse, ReasonNotAdmitted, why.Message)
	}
}

func (r *ProjectClaimReconciler) markError(claim *platformv1alpha1.ProjectClaim, err error) {
	if claim.Status.Phase == "" {
		claim.Status.Phase = platformv1alpha1.PhaseProvisioning
	}
	setCondition(claim, platformv1alpha1.ConditionReady, metav1.ConditionFalse, ReasonProvisioningFailed, err.Error())
}

// patchStatus writes the status only if it changed. A write that changes
// nothing would still produce a watch event, the event would trigger a
// reconcile, and the operator would keep itself busy forever.
func (r *ProjectClaimReconciler) patchStatus(ctx context.Context, before, after *platformv1alpha1.ProjectClaim) error {
	// Semantic equality treats 1Gi and 1024Mi as the same quantity.
	if apiequality.Semantic.DeepEqual(before.Status, after.Status) {
		return nil
	}
	if err := r.Status().Patch(ctx, after, client.MergeFrom(before)); err != nil {
		return client.IgnoreNotFound(fmt.Errorf("writing status: %w", err))
	}
	return nil
}

func (r *ProjectClaimReconciler) event(claim *platformv1alpha1.ProjectClaim, eventType, reason, message string) {
	if r.Recorder != nil {
		r.Recorder.Event(claim, eventType, reason, message)
	}
}
