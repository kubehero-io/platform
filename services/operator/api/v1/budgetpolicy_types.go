package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// BudgetPolicySpec is the declarative spending intent for a set of workloads.
type BudgetPolicySpec struct {
	// Scope selects the clusters and namespaces this policy applies to.
	// +required
	Scope Scope `json:"scope"`

	// Ceiling is a human-readable spend limit, e.g. "$100000/mo" or "$300/hr".
	// +required
	Ceiling string `json:"ceiling"`

	// HardStop indicates whether the escalation plan should be allowed to run
	// once the ceiling is crossed. If false, this policy is advisory / alert-only.
	// +optional
	HardStop bool `json:"hardStop,omitempty"`

	// HumanArm requires an operator to arm the policy via the dashboard or CLI
	// before any escalation step can run. Defaults true.
	// +optional
	HumanArm *bool `json:"humanArm,omitempty"`

	// Escalation is the ordered list of steps to run when the ceiling is crossed.
	// Each step waits its WaitAfter before the next step runs.
	// +optional
	Escalation []EscalationStep `json:"escalation,omitempty"`

	// AlertChannels receives notifications at threshold crossings (50/80/95/100 by default).
	// +optional
	AlertChannels []string `json:"alertChannels,omitempty"`
}

// Scope narrows a policy to specific clusters and namespaces.
type Scope struct {
	// +optional
	ClusterSelector *metav1.LabelSelector `json:"clusterSelector,omitempty"`
	// +optional
	NamespaceSelector *metav1.LabelSelector `json:"namespaceSelector,omitempty"`
}

// EscalationStep is one action in an ordered escalation plan.
type EscalationStep struct {
	// Action name: hpa.cap | pod.evict | nodepool.cordon | alert
	// +kubebuilder:validation:Enum=hpa.cap;pod.evict;nodepool.cordon;alert
	// +required
	Action string `json:"action"`

	// Ratio for scaling actions as a percentage (1..100). Unused for alert.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	// +optional
	RatioPercent *int32 `json:"ratioPercent,omitempty"`

	// WaitAfter is the delay before the next step, e.g. "2m".
	// +optional
	WaitAfter string `json:"waitAfter,omitempty"`

	// Channels for alert action (e.g. "slack://ops-oncall").
	// +optional
	Channels []string `json:"channels,omitempty"`
}

// BudgetPolicyStatus defines the observed state of BudgetPolicy.
type BudgetPolicyStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	// For Kubernetes API conventions, see:
	// https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md#typical-status-properties

	// conditions represent the current state of the BudgetPolicy resource.
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

// BudgetPolicy is the Schema for the budgetpolicies API
type BudgetPolicy struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of BudgetPolicy
	// +required
	Spec BudgetPolicySpec `json:"spec"`

	// status defines the observed state of BudgetPolicy
	// +optional
	Status BudgetPolicyStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// BudgetPolicyList contains a list of BudgetPolicy
type BudgetPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []BudgetPolicy `json:"items"`
}

func init() {
	SchemeBuilder.Register(&BudgetPolicy{}, &BudgetPolicyList{})
}
