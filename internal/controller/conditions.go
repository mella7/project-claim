package controller

import (
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	platformv1alpha1 "github.com/mella7/project-claim/api/v1alpha1"
)

// setCondition goes through the apimachinery helper, which keeps
// lastTransitionTime when the status value did not change. A condition whose
// timestamp moved on every reconcile would be useless for alerting and would
// defeat the "only write on change" rule.
func setCondition(claim *platformv1alpha1.ProjectClaim, conditionType string, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&claim.Status.Conditions, metav1.Condition{
		Type:               conditionType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: claim.Generation,
	})
}

func findCondition(claim *platformv1alpha1.ProjectClaim, conditionType string) *metav1.Condition {
	return meta.FindStatusCondition(claim.Status.Conditions, conditionType)
}

// IsReady reports whether the claim is Ready for its current spec.
func IsReady(claim *platformv1alpha1.ProjectClaim) bool {
	return meta.IsStatusConditionTrue(claim.Status.Conditions, platformv1alpha1.ConditionReady) &&
		claim.Status.ObservedGeneration == claim.Generation
}
