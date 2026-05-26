package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// RightsizingPolicySpec defines how aggressively KubeHero right-sizes workloads in scope.
type RightsizingPolicySpec struct {
	// Scope narrows this policy to specific clusters / namespaces.
	// +required
	Scope Scope `json:"scope"`

	// TargetUtilization is the desired p95 utilization percentage (0..100).
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	// +optional
	TargetUtilization *int32 `json:"targetUtilization,omitempty"`

	// Mode controls how recommendations are applied.
	// +kubebuilder:validation:Enum=recommend;apply;shadow
	// +required
	Mode string `json:"mode"`

	// Exclude is a list of workload names to skip entirely.
	// +optional
	Exclude []string `json:"exclude,omitempty"`

	// Safety caps change frequency to avoid thrash.
	// +optional
	Safety SafetyOptions `json:"safety,omitempty"`
}

// SafetyOptions limit how often and aggressively the operator changes state.
type SafetyOptions struct {
	// +optional
	MinReplicas *int32 `json:"minReplicas,omitempty"`
	// +optional
	P95HeadroomPct *int32 `json:"p95HeadroomPct,omitempty"`
	// +optional
	ObservationWindow string `json:"observationWindow,omitempty"`
	// +optional
	MaxChangePerDay *int32 `json:"maxChangePerDay,omitempty"`
}

// RightsizingPolicyStatus defines the observed state of RightsizingPolicy.
type RightsizingPolicyStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	// For Kubernetes API conventions, see:
	// https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md#typical-status-properties

	// conditions represent the current state of the RightsizingPolicy resource.
	// Each condition has a unique type and reflects the status of a specific aspect of the resource.
	//
	// Standard condition types include:
	// - "Available": the resource is fully functional
	// - "Progressing": the resource is being created or updated
	// - "Degraded": the resource failed to reach or maintain its desired state
	//
	// The status of each condition is one of True, False, or Unknown.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// RightsizingPolicy is the Schema for the rightsizingpolicies API
type RightsizingPolicy struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of RightsizingPolicy
	// +required
	Spec RightsizingPolicySpec `json:"spec"`

	// status defines the observed state of RightsizingPolicy
	// +optional
	Status RightsizingPolicyStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// RightsizingPolicyList contains a list of RightsizingPolicy
type RightsizingPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []RightsizingPolicy `json:"items"`
}

func init() {
	SchemeBuilder.Register(&RightsizingPolicy{}, &RightsizingPolicyList{})
}
