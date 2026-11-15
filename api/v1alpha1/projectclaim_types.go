package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Phase is a one-word summary of a claim for `kubectl get`. Conditions carry
// the detail and are what automation should read.
// +kubebuilder:validation:Enum=Pending;Provisioning;Ready;Degraded;Failed;Terminating
type Phase string

const (
	// PhasePending means the claim holds nothing yet: it is waiting on its
	// tenant, its tier or free budget.
	PhasePending Phase = "Pending"
	// PhaseProvisioning means the claim is admitted and its objects are being created.
	PhaseProvisioning Phase = "Provisioning"
	// PhaseReady means the namespace and its guardrails match the spec.
	PhaseReady Phase = "Ready"
	// PhaseDegraded means the namespace exists, but the current spec could not
	// be applied. The last admitted configuration stays in place.
	PhaseDegraded Phase = "Degraded"
	// PhaseFailed means the claim cannot make progress without a spec change.
	PhaseFailed Phase = "Failed"
	// PhaseTerminating means the claim is being deleted and cleanup is running.
	PhaseTerminating Phase = "Terminating"
)

const (
	// ConditionAdmitted is true when the current spec passed tenant and tier
	// policy and its quota is reserved against the tenant budget.
	ConditionAdmitted = "Admitted"
	// ConditionReady is true when the namespace and every managed object in it
	// match the current spec.
	ConditionReady = "Ready"
)

// ReclaimPolicy decides what happens to the namespace when its claim is deleted.
// +kubebuilder:validation:Enum=Delete;Retain
type ReclaimPolicy string

const (
	// ReclaimDelete removes the namespace together with the claim.
	ReclaimDelete ReclaimPolicy = "Delete"
	// ReclaimRetain keeps the namespace and its contents, and only detaches
	// them from the claim.
	ReclaimRetain ReclaimPolicy = "Retain"
)

// TenantReference names the Tenant a claim is billed against.
type TenantReference struct {
	// Name of the Tenant.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Name string `json:"name"`
}

// AccessSpec lists who gets which role inside the namespace.
type AccessSpec struct {
	// Requester is the user bound to the admin ClusterRole in the namespace.
	// It is also written to the openshift.io/requester annotation.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Requester string `json:"requester"`

	// EditorGroups are bound to the edit ClusterRole in the namespace.
	// +optional
	// +listType=set
	// +kubebuilder:validation:MaxItems=32
	EditorGroups []string `json:"editorGroups,omitempty"`

	// ViewerGroups are bound to the view ClusterRole in the namespace.
	// +optional
	// +listType=set
	// +kubebuilder:validation:MaxItems=32
	ViewerGroups []string `json:"viewerGroups,omitempty"`
}

// ProjectClaimSpec is the desired state of a ProjectClaim.
type ProjectClaimSpec struct {
	// ProjectName is the name of the namespace to provision. It cannot be
	// changed after creation: a rename would orphan the namespace that
	// already exists.
	// +kubebuilder:validation:MinLength=2
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="projectName is immutable"
	// +kubebuilder:validation:XValidation:rule="!self.startsWith('kube-') && !self.startsWith('openshift-') && !(self in ['default', 'openshift'])",message="projectName must not use a reserved name or the kube- and openshift- prefixes"
	ProjectName string `json:"projectName"`

	// TenantRef is the Tenant this project belongs to. The project's quota
	// counts against that tenant's budget. Immutable.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="tenantRef is immutable"
	TenantRef TenantReference `json:"tenantRef"`

	// Tier is the name of the ProjectTier that supplies default quota,
	// container defaults and the Pod Security level.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Tier string `json:"tier"`

	// Access lists who gets which role inside the namespace.
	Access AccessSpec `json:"access"`

	// Description is shown as the OpenShift project description.
	// +optional
	// +kubebuilder:validation:MaxLength=256
	Description string `json:"description,omitempty"`

	// Quota overrides individual entries of the tier's quota. Keys that are
	// not listed here keep the tier's value.
	// +optional
	Quota corev1.ResourceList `json:"quota,omitempty"`

	// NetworkIsolation installs a NetworkPolicy that only admits traffic from
	// the namespace itself and from the cluster's ingress and monitoring
	// namespaces. Defaults to true. A tier can forbid turning it off.
	// +optional
	// +kubebuilder:default=true
	NetworkIsolation *bool `json:"networkIsolation,omitempty"`

	// TTL limits the lifetime of the project, counted from the claim's
	// creation. When it runs out the claim deletes itself. If unset, the
	// tier's defaultTTL applies, if it has one.
	// +optional
	TTL *metav1.Duration `json:"ttl,omitempty"`

	// ReclaimPolicy decides what happens to the namespace when the claim is
	// deleted. Delete removes it, Retain keeps it and detaches it.
	// +optional
	// +kubebuilder:default=Delete
	ReclaimPolicy ReclaimPolicy `json:"reclaimPolicy,omitempty"`

	// ExtraLabels are added to the namespace. Labels owned by the operator
	// and Pod Security labels cannot be set this way.
	// +optional
	// +kubebuilder:validation:MaxProperties=16
	// +kubebuilder:validation:XValidation:rule="self.all(k, !k.startsWith('platform.mella7.me/') && !k.startsWith('pod-security.kubernetes.io/') && !k.startsWith('kubernetes.io/') && !k.startsWith('openshift.io/') && k != 'app.kubernetes.io/managed-by')",message="extraLabels must not use label keys reserved for the operator, Kubernetes, OpenShift or Pod Security"
	ExtraLabels map[string]string `json:"extraLabels,omitempty"`
}

// ProjectClaimStatus is the observed state of a ProjectClaim.
type ProjectClaimStatus struct {
	// Phase is a one-word summary for `kubectl get`.
	// +optional
	Phase Phase `json:"phase,omitempty"`

	// NamespaceName is the namespace this claim owns. Empty until the
	// namespace has been created or adopted.
	// +optional
	NamespaceName string `json:"namespaceName,omitempty"`

	// ObservedGeneration is the spec generation the status describes.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// AppliedQuota is the quota currently enforced in the namespace: the tier
	// defaults with this claim's overrides merged in.
	// +optional
	AppliedQuota corev1.ResourceList `json:"appliedQuota,omitempty"`

	// ExpiresAt is when the claim deletes itself, if it has a lifetime.
	// +optional
	ExpiresAt *metav1.Time `json:"expiresAt,omitempty"`

	// Conditions holds the Admitted and Ready conditions.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=pclaim,categories=platform
// +kubebuilder:validation:XValidation:rule="size(self.metadata.name) <= 63",message="metadata.name must be at most 63 characters, because it is used as a label value"
// +kubebuilder:printcolumn:name="Namespace",type=string,JSONPath=`.status.namespaceName`
// +kubebuilder:printcolumn:name="Tenant",type=string,JSONPath=`.spec.tenantRef.name`
// +kubebuilder:printcolumn:name="Tier",type=string,JSONPath=`.spec.tier`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`,priority=1
// +kubebuilder:printcolumn:name="Expires",type=date,JSONPath=`.status.expiresAt`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ProjectClaim requests a namespace with quota, container defaults, role
// bindings, Pod Security labels and network isolation, managed as one unit.
type ProjectClaim struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ProjectClaimSpec   `json:"spec"`
	Status ProjectClaimStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ProjectClaimList is a list of ProjectClaims.
type ProjectClaimList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ProjectClaim `json:"items"`
}

// NetworkIsolationEnabled resolves the defaulted pointer.
func (c *ProjectClaim) NetworkIsolationEnabled() bool {
	return c.Spec.NetworkIsolation == nil || *c.Spec.NetworkIsolation
}

// EffectiveReclaimPolicy resolves the default for objects that skipped API
// server defaulting.
func (c *ProjectClaim) EffectiveReclaimPolicy() ReclaimPolicy {
	if c.Spec.ReclaimPolicy == "" {
		return ReclaimDelete
	}
	return c.Spec.ReclaimPolicy
}

func init() {
	SchemeBuilder.Register(&ProjectClaim{}, &ProjectClaimList{})
}
