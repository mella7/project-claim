package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	platformv1alpha1 "github.com/mella7/project-claim/api/v1alpha1"
)

// reconcileDelete runs while the claim carries a deletion timestamp.
//
// A delete of an object with a finalizer does not delete it. The API server
// sets the timestamp and waits. This function does the cleanup and takes the
// finalizer off at the very end, and only then does the object disappear.
//
// The rule it keeps: the finalizer comes off only after the namespace is
// confirmed gone (or detached) and the budget is released. Garbage collection
// through owner references would also remove the namespace eventually, but
// it gives no confirmation, so the claim would vanish while its namespace is
// still stuck terminating and nobody would know.
func (r *ProjectClaimReconciler) reconcileDelete(ctx context.Context, claim *platformv1alpha1.ProjectClaim) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(claim, Finalizer) {
		return ctrl.Result{}, nil
	}

	before := claim.DeepCopy()
	claim.Status.Phase = platformv1alpha1.PhaseTerminating
	setCondition(claim, platformv1alpha1.ConditionReady, metav1.ConditionFalse, ReasonTerminating, "cleanup in progress")
	if err := r.patchStatus(ctx, before, claim); err != nil {
		return ctrl.Result{}, err
	}

	done, err := r.cleanupNamespace(ctx, claim)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !done {
		// Nothing to poll for. The namespace is owned by the claim, so its
		// delete event comes back here through the Owns() watch.
		return ctrl.Result{}, nil
	}

	if err := r.release(ctx, claim, true); err != nil {
		return ctrl.Result{}, err
	}

	patch := client.MergeFromWithOptions(claim.DeepCopy(), client.MergeFromWithOptimisticLock{})
	controllerutil.RemoveFinalizer(claim, Finalizer)
	if err := r.Patch(ctx, claim, patch); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(fmt.Errorf("removing finalizer: %w", err))
	}
	log.FromContext(ctx).Info("cleanup complete, claim released")
	return ctrl.Result{}, nil
}

// cleanupNamespace reports whether the claim is finished with its namespace.
func (r *ProjectClaimReconciler) cleanupNamespace(ctx context.Context, claim *platformv1alpha1.ProjectClaim) (bool, error) {
	logger := log.FromContext(ctx)

	// Read from the API server, not the cache. Taking the finalizer off
	// cannot be undone, and a cache that has not yet seen a namespace
	// created a moment ago would report it as absent.
	ns := &corev1.Namespace{}
	err := r.APIReader.Get(ctx, client.ObjectKey{Name: claim.Spec.ProjectName}, ns)
	if apierrors.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("reading namespace: %w", err)
	}

	if !ownedBy(ns, claim) {
		// The claim never owned this namespace, for example because it was
		// refused with NamespaceConflict. Leave it alone and let the claim go.
		logger.Info("namespace does not belong to this claim, leaving it", "namespace", ns.Name)
		return true, nil
	}

	if claim.EffectiveReclaimPolicy() == platformv1alpha1.ReclaimRetain {
		if err := r.detach(ctx, claim, ns); err != nil {
			return false, err
		}
		r.event(claim, corev1.EventTypeNormal, "Retained", fmt.Sprintf("namespace %s was kept and detached", ns.Name))
		return true, nil
	}

	if ns.DeletionTimestamp.IsZero() {
		// The UID precondition makes sure this deletes the namespace that
		// was just read and not another one created under the same name.
		if err := r.Delete(ctx, ns, client.Preconditions{UID: &ns.UID}); client.IgnoreNotFound(err) != nil {
			return false, fmt.Errorf("deleting namespace: %w", err)
		}
		logger.Info("namespace deletion requested", "namespace", ns.Name)
		r.event(claim, corev1.EventTypeNormal, "DeletingNamespace", fmt.Sprintf("waiting for namespace %s to terminate", ns.Name))
	}
	return false, nil
}

// detach implements reclaimPolicy Retain. It removes the claim's owner
// reference from the namespace and from everything the operator created in
// it. If the references stayed, the garbage collector would delete those
// objects as soon as the claim is gone.
func (r *ProjectClaimReconciler) detach(ctx context.Context, claim *platformv1alpha1.ProjectClaim, ns *corev1.Namespace) error {
	lists := []client.ObjectList{
		&corev1.ResourceQuotaList{},
		&corev1.LimitRangeList{},
		&rbacv1.RoleBindingList{},
		&networkingv1.NetworkPolicyList{},
	}
	for _, list := range lists {
		if err := r.APIReader.List(ctx, list, client.InNamespace(ns.Name), client.MatchingLabels{LabelClaim: claim.Name}); err != nil {
			return fmt.Errorf("listing objects to detach: %w", err)
		}
		err := meta.EachListItem(list, func(item runtime.Object) error {
			obj, ok := item.(client.Object)
			if !ok {
				return nil
			}
			return r.dropOwner(ctx, claim, obj, nil)
		})
		if err != nil {
			return err
		}
	}
	return r.dropOwner(ctx, claim, ns, func() {
		if ns.Annotations == nil {
			ns.Annotations = map[string]string{}
		}
		ns.Annotations[AnnotationReleasedFrom] = claim.Name
	})
}

func (r *ProjectClaimReconciler) dropOwner(ctx context.Context, claim *platformv1alpha1.ProjectClaim, obj client.Object, extra func()) error {
	before, ok := obj.DeepCopyObject().(client.Object)
	if !ok {
		return nil
	}
	refs := obj.GetOwnerReferences()
	kept := make([]metav1.OwnerReference, 0, len(refs))
	for _, ref := range refs {
		if ref.UID != claim.UID {
			kept = append(kept, ref)
		}
	}
	if len(kept) == len(refs) && extra == nil {
		return nil
	}
	obj.SetOwnerReferences(kept)
	if extra != nil {
		extra()
	}
	if err := r.Patch(ctx, obj, client.MergeFrom(before)); client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("detaching %s %s: %w", r.kindOf(obj), client.ObjectKeyFromObject(obj), err)
	}
	return nil
}
