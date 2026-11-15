package controller

import "strings"

const (
	// Finalizer keeps a ProjectClaim around until its namespace is gone (or
	// detached) and its budget reservation is released.
	Finalizer = "platform.mella7.me/projectclaim"

	// ManagerName identifies this operator as a field manager and in the
	// app.kubernetes.io/managed-by label.
	ManagerName = "projectclaim-operator"

	LabelManagedBy = "app.kubernetes.io/managed-by"
	LabelClaim     = "platform.mella7.me/claim"
	LabelTenant    = "platform.mella7.me/tenant"
	LabelTier      = "platform.mella7.me/tier"

	// AnnotationManagedLabels remembers which namespace labels the operator
	// set, so a label dropped from the claim is also dropped from the
	// namespace instead of lingering forever.
	AnnotationManagedLabels = "platform.mella7.me/managed-labels"

	// AnnotationAdoptInto lets a cluster admin hand an existing namespace to
	// a claim. Without it the operator never touches a namespace it did not
	// create.
	AnnotationAdoptInto = "platform.mella7.me/adopt-into"

	// AnnotationReleasedFrom marks a namespace that was kept when its claim
	// was deleted with reclaimPolicy Retain.
	AnnotationReleasedFrom = "platform.mella7.me/released-from"

	// OpenShift reads these to render the namespace as a Project.
	AnnotationDisplayName = "openshift.io/display-name"
	AnnotationDescription = "openshift.io/description"
	AnnotationRequester   = "openshift.io/requester"

	LabelPodSecurityEnforce = "pod-security.kubernetes.io/enforce"
	LabelPodSecurityAudit   = "pod-security.kubernetes.io/audit"
	LabelPodSecurityWarn    = "pod-security.kubernetes.io/warn"

	QuotaName         = "projectclaim-quota"
	LimitRangeName    = "projectclaim-limits"
	NetworkPolicyName = "projectclaim-isolation"
	AdminBindingName  = "projectclaim-admin"
	EditBindingName   = "projectclaim-edit"
	ViewBindingName   = "projectclaim-view"
)

// Reasons reported on the Admitted and Ready conditions.
const (
	ReasonAdmitted                 = "Admitted"
	ReasonProvisioned              = "Provisioned"
	ReasonProvisioning             = "Provisioning"
	ReasonProvisioningFailed       = "ProvisioningFailed"
	ReasonNotAdmitted              = "NotAdmitted"
	ReasonSpecNotApplied           = "SpecNotApplied"
	ReasonTerminating              = "Terminating"
	ReasonTierNotFound             = "TierNotFound"
	ReasonTenantNotFound           = "TenantNotFound"
	ReasonTierNotAllowed           = "TierNotAllowed"
	ReasonNamespacePrefixMismatch  = "NamespacePrefixMismatch"
	ReasonNetworkIsolationRequired = "NetworkIsolationRequired"
	ReasonNamespaceConflict        = "NamespaceConflict"
	ReasonNamespaceTerminating     = "NamespaceTerminating"
	ReasonBudgetResourceUnbounded  = "BudgetResourceUnbounded"
	ReasonQuotaBudgetExceeded      = "QuotaBudgetExceeded"
	ReasonProjectLimitReached      = "ProjectLimitReached"
)

// reservedLabelPrefixes cannot be set through spec.extraLabels. The CRD
// rejects them at admission; this list is the second line of defence for
// objects that got past it.
var reservedLabelPrefixes = []string{
	"platform.mella7.me/",
	"pod-security.kubernetes.io/",
	"kubernetes.io/",
	"openshift.io/",
}

func reservedLabel(key string) bool {
	if key == LabelManagedBy {
		return true
	}
	for _, p := range reservedLabelPrefixes {
		if strings.HasPrefix(key, p) {
			return true
		}
	}
	return false
}
