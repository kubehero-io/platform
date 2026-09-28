package v1

import (
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// RightsizingPolicy modes. recommend and shadow never touch workloads;
// apply mutates container requests only when every guard passes.
const (
	RightsizingModeRecommend = "recommend"
	RightsizingModeShadow    = "shadow"
	RightsizingModeApply     = "apply"
)

// RightsizingPolicySpec defines how aggressively KubeHero right-sizes workloads in scope.
type RightsizingPolicySpec struct {
	// Scope narrows this policy to specific clusters / namespaces.
	// clusterSelector is matched against the operator's cluster labels
	// (CLUSTER_LABELS plus kubehero.io/cluster-id); unset matches every
	// cluster. namespaceSelector is matched against Namespace labels;
	// unset selects nothing and {} selects every non-system namespace.
	// +required
	Scope Scope `json:"scope"`

	// TargetUtilization is the desired p95 utilization percentage (0..100).
	// When set it wins over safety.p95HeadroomPct: the headroom sent to the
	// recommender is 100/targetUtilization − 1 (70 → ~43% headroom).
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	// +optional
	TargetUtilization *int32 `json:"targetUtilization,omitempty"`

	// Mode controls how recommendations are applied:
	// recommend — surface per-container recommendations in status only;
	// shadow — additionally compute and audit what apply WOULD change;
	// apply — patch container requests when every safety guard passes.
	// +kubebuilder:validation:Enum=recommend;apply;shadow
	// +required
	Mode string `json:"mode"`

	// Exclude is a list of workload names to skip entirely.
	// +optional
	Exclude []string `json:"exclude,omitempty"`

	// Safety caps change frequency to avoid thrash.
	// +optional
	Safety SafetyOptions `json:"safety,omitempty"`

	// HumanArm requires an operator to arm the policy (the
	// kubehero.kubehero.io/armed="true" annotation, set by
	// `kubehero cap --arm` or the dashboard) before apply mode mutates
	// anything. Defaults true.
	// +optional
	HumanArm *bool `json:"humanArm,omitempty"`

	// MinConfidence is the lowest recommendation confidence apply mode
	// acts on. Confidence reflects sample coverage of the observation
	// window. Defaults to medium.
	// +kubebuilder:validation:Enum=low;medium;high
	// +optional
	MinConfidence string `json:"minConfidence,omitempty"`

	// AdjustLimits also scales existing CPU / memory limits so the
	// limit:request ratio is preserved. When false (the default) only
	// requests change, and a request is never raised above its limit.
	// +optional
	AdjustLimits bool `json:"adjustLimits,omitempty"`
}

// SafetyOptions limit how often and aggressively the operator changes state.
type SafetyOptions struct {
	// MinReplicas skips workloads running fewer replicas than this — a
	// resource change rolls every pod, which a single replica can't do
	// without a gap. Defaults to 1.
	// +optional
	MinReplicas *int32 `json:"minReplicas,omitempty"`
	// P95HeadroomPct is the headroom added on top of the sized
	// percentile. Defaults to 15.
	// +optional
	P95HeadroomPct *int32 `json:"p95HeadroomPct,omitempty"`
	// ObservationWindow is the usage window recommendations are computed
	// over, e.g. "7d" or "36h" (1h..90d). Defaults to "7d".
	// +optional
	ObservationWindow string `json:"observationWindow,omitempty"`
	// MaxChangePerDay caps applied changes per workload per UTC day.
	// Defaults to 1; 0 blocks all changes.
	// +optional
	MaxChangePerDay *int32 `json:"maxChangePerDay,omitempty"`
}

// ContainerRecommendation is one container's right-size suggestion,
// joined against the live workload spec (current values are what the
// pod template requests right now, not what the recommender last saw).
type ContainerRecommendation struct {
	Namespace string `json:"namespace"`
	Workload  string `json:"workload"`
	// Kind is Deployment or StatefulSet.
	Kind      string `json:"kind"`
	Container string `json:"container"`

	// +optional
	CurrentCPURequest *resource.Quantity `json:"currentCpuRequest,omitempty"`
	// +optional
	RecommendedCPURequest *resource.Quantity `json:"recommendedCpuRequest,omitempty"`
	// +optional
	CurrentMemoryRequest *resource.Quantity `json:"currentMemoryRequest,omitempty"`
	// +optional
	RecommendedMemoryRequest *resource.Quantity `json:"recommendedMemoryRequest,omitempty"`

	// SavingsUSDMonth is the monthly saving in USD ("1234.56"); negative
	// means the container is under-provisioned and sizing up costs more.
	// +optional
	SavingsUSDMonth string `json:"savingsUsdMonth,omitempty"`
	// Confidence is low | medium | high (sample coverage of the window).
	// +optional
	Confidence string `json:"confidence,omitempty"`
	// Reason is the recommender's plain-English explanation.
	// +optional
	Reason string `json:"reason,omitempty"`
	// OOMKills observed in the window; > 0 blocks memory decreases.
	// +optional
	OOMKills int32 `json:"oomKills,omitempty"`
}

// QuantityChange is a from → to pair for one resource value.
type QuantityChange struct {
	From resource.Quantity `json:"from"`
	To   resource.Quantity `json:"to"`
}

// Planned-change outcomes.
const (
	ChangeOutcomePlanned = "planned" // shadow: every guard passed, would apply
	ChangeOutcomeApplied = "applied" // apply: patched onto the workload
	ChangeOutcomeBlocked = "blocked" // a guard refused it; see reason
	ChangeOutcomeFailed  = "failed"  // guards passed but the patch errored
)

// PlannedChange is a guard-evaluated change for one container.
type PlannedChange struct {
	Namespace string `json:"namespace"`
	Workload  string `json:"workload"`
	Kind      string `json:"kind"`
	Container string `json:"container"`

	// +optional
	CPURequest *QuantityChange `json:"cpuRequest,omitempty"`
	// +optional
	MemoryRequest *QuantityChange `json:"memoryRequest,omitempty"`
	// +optional
	CPULimit *QuantityChange `json:"cpuLimit,omitempty"`
	// +optional
	MemoryLimit *QuantityChange `json:"memoryLimit,omitempty"`

	// +optional
	SavingsUSDMonth string `json:"savingsUsdMonth,omitempty"`

	// Outcome is planned (shadow), applied, blocked, or failed.
	// +kubebuilder:validation:Enum=planned;applied;blocked;failed
	Outcome string `json:"outcome"`
	// Reason names the guard that blocked the change, or notes how the
	// target was bounded (step limit, observed max, limit ceiling).
	// +optional
	Reason string `json:"reason,omitempty"`
	// ChangeID ties an applied change to its audit event and to the
	// workload's kubehero.io/rightsize-previous annotation; pass it to
	// `kubehero undo` to restore the previous requests.
	// +optional
	ChangeID string `json:"changeId,omitempty"`
}

// RightsizingPolicyStatus defines the observed state of RightsizingPolicy.
type RightsizingPolicyStatus struct {
	// conditions represent the current state of the RightsizingPolicy resource.
	// Ready reports spec validity and scope; Armed the human-arm gate;
	// DataAvailable whether the control plane served live recommendations.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ObservedGeneration is the spec generation last evaluated.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// LastEvaluated is when recommendations were last fetched and evaluated.
	// +optional
	LastEvaluated *metav1.Time `json:"lastEvaluated,omitempty"`

	// WorkloadsInScope counts Deployments + StatefulSets the policy covers
	// after exclusions.
	// +optional
	WorkloadsInScope int32 `json:"workloadsInScope,omitempty"`

	// Recommendations lists per-container suggestions, largest saving
	// first (capped at 50 entries; totalSavingsUsdMonth covers them all).
	// +listType=atomic
	// +optional
	Recommendations []ContainerRecommendation `json:"recommendations,omitempty"`

	// TotalSavingsUSDMonth sums savingsUsdMonth over every in-scope
	// recommendation, in USD ("1234.56").
	// +optional
	TotalSavingsUSDMonth string `json:"totalSavingsUsdMonth,omitempty"`

	// PlannedChanges (shadow + apply modes) is the guard-evaluated change
	// set from the last evaluation (capped at 50 entries).
	// +listType=atomic
	// +optional
	PlannedChanges []PlannedChange `json:"plannedChanges,omitempty"`

	// PlannedChangesHash fingerprints the planned set so a shadow audit
	// event is emitted only when what WOULD change actually changes.
	// +optional
	PlannedChangesHash string `json:"plannedChangesHash,omitempty"`

	// LastApplied is when apply mode last patched a workload.
	// +optional
	LastApplied *metav1.Time `json:"lastApplied,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Mode",type=string,JSONPath=`.spec.mode`
// +kubebuilder:printcolumn:name="Workloads",type=integer,JSONPath=`.status.workloadsInScope`
// +kubebuilder:printcolumn:name="Savings/mo",type=string,JSONPath=`.status.totalSavingsUsdMonth`
// +kubebuilder:printcolumn:name="Armed",type=string,JSONPath=`.status.conditions[?(@.type=="Armed")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

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
