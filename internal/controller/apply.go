package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	platformv1alpha1 "github.com/mella7/project-claim/api/v1alpha1"
	"github.com/mella7/project-claim/internal/metrics"
)

// apply brings one object in line with the claim.
//
// CreateOrUpdate reads the object, runs mutate on the result and compares
// before and after. It only writes when the two differ, which is what makes
// repeated reconciles free. The catch is that mutate has to be deterministic
// and has to set fields rather than add to them. Everything passed in here
// follows that rule.
func (r *ProjectClaimReconciler) apply(ctx context.Context, claim *platformv1alpha1.ProjectClaim, obj client.Object, mutate func()) error {
	kind := r.kindOf(obj)
	key := client.ObjectKeyFromObject(obj)

	mutateAll := func() error {
		mutate()
		labels := obj.GetLabels()
		if labels == nil {
			labels = map[string]string{}
		}
		for k, v := range childLabels(claim) {
			labels[k] = v
		}
		obj.SetLabels(labels)
		// The controller reference does two jobs. It lets the garbage
		// collector clean up if the finalizer is ever bypassed, and it is
		// how a change to this object finds its way back to the claim.
		return controllerutil.SetControllerReference(claim, obj, r.Scheme)
	}

	op, err := controllerutil.CreateOrUpdate(ctx, r.Client, obj, mutateAll)
	if apierrors.IsAlreadyExists(err) {
		// The cache only holds objects that carry the operator's label, so
		// an object of this name that we did not label is invisible to it.
		// Read it from the API server and take it over.
		op, err = r.takeOver(ctx, obj, mutateAll)
	}
	if err != nil {
		metrics.ApplyTotal.WithLabelValues(kind, "error").Inc()
		return fmt.Errorf("%s %s: %w", kind, objectName(key), err)
	}

	metrics.ApplyTotal.WithLabelValues(kind, string(op)).Inc()
	if op != controllerutil.OperationResultNone {
		log.FromContext(ctx).Info("applied", "kind", kind, "object", objectName(key), "operation", op)
		r.event(claim, corev1.EventTypeNormal, "Applied", fmt.Sprintf("%s %s %s", kind, objectName(key), op))
	}
	return nil
}

func (r *ProjectClaimReconciler) takeOver(ctx context.Context, obj client.Object, mutate func() error) (controllerutil.OperationResult, error) {
	if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
		return controllerutil.OperationResultNone, err
	}
	before := obj.DeepCopyObject()
	if err := mutate(); err != nil {
		return controllerutil.OperationResultNone, err
	}
	if apiequality.Semantic.DeepEqual(before, obj) {
		// Already ours and already right. The cache just has not caught up.
		return controllerutil.OperationResultNone, nil
	}
	if err := r.Update(ctx, obj); err != nil {
		return controllerutil.OperationResultNone, err
	}
	return controllerutil.OperationResultUpdated, nil
}

// remove deletes an object the claim no longer asks for. It only touches
// objects that carry this claim's label, and it looks in the cache first so
// that the usual case, nothing to remove, costs no API call.
func (r *ProjectClaimReconciler) remove(ctx context.Context, claim *platformv1alpha1.ProjectClaim, obj client.Object) error {
	kind := r.kindOf(obj)
	key := client.ObjectKeyFromObject(obj)

	if err := r.Get(ctx, key, obj); err != nil {
		return client.IgnoreNotFound(err)
	}
	if obj.GetLabels()[LabelClaim] != claim.Name || !obj.GetDeletionTimestamp().IsZero() {
		return nil
	}
	if err := r.Delete(ctx, obj); client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("removing %s %s: %w", kind, objectName(key), err)
	}
	metrics.ApplyTotal.WithLabelValues(kind, "deleted").Inc()
	log.FromContext(ctx).Info("removed", "kind", kind, "object", objectName(key))
	r.event(claim, corev1.EventTypeNormal, "Removed", fmt.Sprintf("%s %s is no longer asked for", kind, objectName(key)))
	return nil
}

func (r *ProjectClaimReconciler) kindOf(obj client.Object) string {
	gvk, err := apiutil.GVKForObject(obj, r.Scheme)
	if err != nil {
		return "Unknown"
	}
	return gvk.Kind
}

// objectName prints namespace/name, or just the name for cluster-scoped
// objects. ObjectKey's own String() would print "/name" for those.
func objectName(key client.ObjectKey) string {
	if key.Namespace == "" {
		return key.Name
	}
	return key.String()
}
