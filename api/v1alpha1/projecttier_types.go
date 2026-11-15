package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PodSecurityLevel is a Pod Security Standards level.
// +kubebuilder:validation:Enum=privileged;baseline;restricted
type PodSecurityLevel string

const (
	PodSecurityPrivileged PodSecurityLevel = "privileged"
	PodSecurityBaseline   PodSecurityLevel = "baseline"
	PodSecurityRestricted PodSecurityLevel = "restricted"
)

// PodSecurity sets the Pod Security Admission labels of the namespace.
type PodSecurity struct {
	// Enforce rejects pods that violate this level.
	// +optional
	// +kubebuilder:default=baseline
	Enforce PodSecurityLevel `json:"enforce,omitempty"`

	// Audit records violations of this level in the audit log.
	// +optional
	// +kubebuilder:default=restricted
	Audit PodSecurityLevel `json:"audit,omitempty"`

	// Warn returns a warning to the client for violations of this level.
	// +optional
	// +kubebuilder:default=restricted
	Warn PodSecurityLevel `json:"warn,omitempty"`
}

// ContainerDefaults fills in resources for containers that do not declare
// their own. Without it, a quota on requests or limits rejects every pod that
// omits them.
type ContainerDefaults struct {
	// Limits are the default container limits.
	// +optional
	Limits corev1.ResourceList `json:"limits,omitempty"`

	// Requests are the default container requests. A resource that has a
	// default limit but no default request gets the limit as its request,
	// which is also what the API server does.
	// +optional
	Requests corev1.ResourceList `json:"requests,omitempty"`
}

// ProjectTierSpec defines what projects of this tier get by default.
type ProjectTierSpec struct {
	// Description says what the tier is meant for.
	// +optional
	// +kubebuilder:validation:MaxLength=256
	Description string `json:"description,omitempty"`

	// Quota is the default ResourceQuota of the namespace. A claim can
	// override single entries.
	// +kubebuilder:validation:XValidation:rule="size(self) > 0",message="quota must set at least one resource"
	Quota corev1.ResourceList `json:"quota"`

	// ContainerDefaults becomes the namespace's LimitRange.
	// +optional
	ContainerDefaults ContainerDefaults `json:"containerDefaults,omitempty"`

	// PodSecurity sets the Pod Security Admission labels of the namespace.
	// +optional
	// +kubebuilder:default={}
	PodSecurity PodSecurity `json:"podSecurity,omitempty"`

	// RequireNetworkIsolation forbids claims of this tier from turning
	// network isolation off.
	// +optional
	RequireNetworkIsolation bool `json:"requireNetworkIsolation,omitempty"`

	// DefaultTTL is the lifetime of claims that do not set their own.
	// Useful for sandbox tiers that should clean up after themselves.
	// +optional
	DefaultTTL *metav1.Duration `json:"defaultTTL,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=ptier,categories=platform
// +kubebuilder:printcolumn:name="Enforce",type=string,JSONPath=`.spec.podSecurity.enforce`
// +kubebuilder:printcolumn:name="Isolation",type=boolean,JSONPath=`.spec.requireNetworkIsolation`
// +kubebuilder:printcolumn:name="Default TTL",type=string,JSONPath=`.spec.defaultTTL`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ProjectTier is a named set of defaults for projects: quota, container
// defaults and the Pod Security level. Changing a tier updates every project
// that uses it.
type ProjectTier struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec ProjectTierSpec `json:"spec"`
}

// +kubebuilder:object:root=true

// ProjectTierList is a list of ProjectTiers.
type ProjectTierList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ProjectTier `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ProjectTier{}, &ProjectTierList{})
}
