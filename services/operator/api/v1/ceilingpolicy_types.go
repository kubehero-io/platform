package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// CeilingPolicySpec defines a burn-rate triggered enforcement policy.
type CeilingPolicySpec struct {
	// BudgetRef is the name of the BudgetPolicy this ceiling references.
	// +required
	BudgetRef string `json:"budgetRef"`

	// Trigger defines when to invoke the escalation plan.
	// +required
	Trigger Trigger `json:"trigger"`

	// Escalation inherits semantics from BudgetPolicy.Escalation.
	// +optional
	Escalation []EscalationStep `json:"escalation,omitempty"`

	// Cooldown is the minimum time between consecutive escalations, e.g. "10m".
	// +optional
	Cooldown string `json:"cooldown,omitempty"`

	// HumanArm requires an operator arming step before any action runs. Defaults true.
	// +optional
	HumanArm *bool `json:"humanArm,omitempty"`
}

// Trigger specifies the burn-rate condition that activates a policy.
type Trigger struct {
	// BurnRateMilli is the multiple of the budgeted rate × 1000 (1500 = 1.5x).
	// +kubebuilder:validation:Minimum=1000
	// +required
	BurnRateMilli int32 `json:"burnRateMilli"`

	// Window is the duration the BurnRate must be sustained, e.g. "5m".
	// +required
	Window string `json:"window"`
}

// CeilingPolicyStatus defines the observed state of CeilingPolicy.
type CeilingPolicyStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	// For Kubernetes API conventions, see:
	// https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md#typical-status-properties

	// conditions represent the current state of the CeilingPolicy resource.
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

// CeilingPolicy is the Schema for the ceilingpolicies API
type CeilingPolicy struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of CeilingPolicy
	// +required
	Spec CeilingPolicySpec `json:"spec"`

	// status defines the observed state of CeilingPolicy
	// +optional
	Status CeilingPolicyStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// CeilingPolicyList contains a list of CeilingPolicy
type CeilingPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []CeilingPolicy `json:"items"`
}

func init() {
	SchemeBuilder.Register(&CeilingPolicy{}, &CeilingPolicyList{})
}
