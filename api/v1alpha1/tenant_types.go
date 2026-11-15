package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// TenantSpec defines the limits a team's projects have to fit into.
type TenantSpec struct {
	// DisplayName is a human-readable name for the tenant.
	// +optional
	// +kubebuilder:validation:MaxLength=128
	DisplayName string `json:"displayName,omitempty"`

	// Owners are the users allowed to create and change claims of this
	// tenant. Enforced at admission by the ValidatingAdmissionPolicy that
	// ships with the operator.
	// +optional
	// +listType=set
	// +kubebuilder:validation:MaxItems=64
	Owners []string `json:"owners,omitempty"`

	// OwnerGroups are groups whose members count as owners.
	// +optional
	// +listType=set
	// +kubebuilder:validation:MaxItems=64
	OwnerGroups []string `json:"ownerGroups,omitempty"`

	// AllowedTiers restricts which ProjectTiers the tenant may use.
	// Empty means any tier.
	// +optional
	// +listType=set
	// +kubebuilder:validation:MaxItems=32
	AllowedTiers []string `json:"allowedTiers,omitempty"`

	// NamespacePrefix, if set, must prefix the projectName of every claim of
	// this tenant. It keeps tenants out of each other's names.
	// +optional
	// +kubebuilder:validation:MaxLength=40
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*)?$`
	NamespacePrefix string `json:"namespacePrefix,omitempty"`

	// MaxProjects caps the number of projects. Unset means no cap.
	// +optional
	// +kubebuilder:validation:Minimum=0
	MaxProjects *int32 `json:"maxProjects,omitempty"`

	// QuotaBudget caps the sum of the quotas of all the tenant's projects,
	// per resource. A claim is only admitted while the sum stays within the
	// budget. Resources that are not listed are not budgeted.
	// +optional
	QuotaBudget corev1.ResourceList `json:"quotaBudget,omitempty"`
}

// Reservation records the quota one claim holds against the tenant budget.
type Reservation struct {
	// ClaimName is the name of the ProjectClaim.
	ClaimName string `json:"claimName"`

	// ClaimUID pins the reservation to one incarnation of the claim, so a
	// claim that is deleted and recreated under the same name does not
	// inherit it.
	ClaimUID types.UID `json:"claimUID"`

	// Quota is the reserved amount.
	// +optional
	Quota corev1.ResourceList `json:"quota,omitempty"`
}

// TenantStatus is the budget ledger of a tenant.
type TenantStatus struct {
	// ProjectCount is the number of projects holding a reservation.
	// +optional
	ProjectCount int32 `json:"projectCount"`

	// Allocated is the sum of all reservations.
	// +optional
	Allocated corev1.ResourceList `json:"allocated,omitempty"`

	// Reservations lists what each claim holds. Claims reserve here before
	// they create anything. The write is guarded by the object's
	// resourceVersion, so two claims competing for the last of the budget
	// cannot both get it.
	// +optional
	// +listType=map
	// +listMapKey=claimName
	Reservations []Reservation `json:"reservations,omitempty"`

	// ObservedGeneration is the spec generation the status describes.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,categories=platform
// +kubebuilder:printcolumn:name="Projects",type=integer,JSONPath=`.status.projectCount`
// +kubebuilder:printcolumn:name="Max",type=integer,JSONPath=`.spec.maxProjects`
// +kubebuilder:printcolumn:name="CPU Used",type=string,JSONPath=`.status.allocated.requests\.cpu`
// +kubebuilder:printcolumn:name="CPU Budget",type=string,JSONPath=`.spec.quotaBudget.requests\.cpu`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Tenant is a team or customer that owns projects. It carries the budget all
// of its projects share and the rules its claims have to follow.
type Tenant struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   TenantSpec   `json:"spec,omitempty"`
	Status TenantStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// TenantList is a list of Tenants.
type TenantList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Tenant `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Tenant{}, &TenantList{})
}
