package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// DrainScope selects which pods are evicted from a quarantined node.
// +kubebuilder:validation:Enum=gpu;all
type DrainScope string

const (
	// DrainGPUPods evicts only pods requesting nvidia.com/gpu.
	DrainGPUPods DrainScope = "gpu"
	// DrainAllPods evicts every pod except DaemonSet and mirror pods.
	DrainAllPods DrainScope = "all"
)

// Condition types reported on a GPUNodePolicy.
const (
	// ConditionReady is True when the policy is valid and being enforced.
	ConditionReady = "Ready"
	// ConditionBudgetExhausted is True when unhealthy nodes are waiting because
	// the policy's disruption budget is used up.
	ConditionBudgetExhausted = "BudgetExhausted"
)

// GPUNodePolicySpec says which GPU nodes a policy governs and how aggressively
// unhealthy ones are remediated. Every field except nodeSelector is optional;
// an unset field falls back to the controller's flag default, so a policy only
// needs to state what it changes.
type GPUNodePolicySpec struct {
	// NodeSelector picks the nodes this policy manages. It must set matchLabels
	// or matchExpressions: an empty selector would match every node in the
	// cluster, control plane included. If several policies match a node, the
	// first by name wins and the node counts only toward that policy's budget.
	// +kubebuilder:validation:XValidation:rule="has(self.matchLabels) || has(self.matchExpressions)",message="nodeSelector must set matchLabels or matchExpressions; an empty selector would match every node"
	NodeSelector metav1.LabelSelector `json:"nodeSelector"`

	// GracePeriod is how long GPUHealthy must stay False before quarantine.
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('0s')",message="must be a non-negative duration such as 2m"
	// +optional
	GracePeriod *metav1.Duration `json:"gracePeriod,omitempty"`

	// RecoveryPeriod is how long GPUHealthy must stay True before release.
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('0s')",message="must be a non-negative duration such as 10m"
	// +optional
	RecoveryPeriod *metav1.Duration `json:"recoveryPeriod,omitempty"`

	// StaleAfter: a node whose agent heartbeat is older than this is never acted on.
	// +kubebuilder:validation:XValidation:rule="duration(self) > duration('0s')",message="must be a positive duration such as 5m"
	// +optional
	StaleAfter *metav1.Duration `json:"staleAfter,omitempty"`

	// MaxUnavailable caps how many of this policy's nodes may be quarantined at
	// once. The larger of this and maxUnavailablePercent applies.
	// +kubebuilder:validation:Minimum=0
	// +optional
	MaxUnavailable *int32 `json:"maxUnavailable,omitempty"`

	// MaxUnavailablePercent is the same cap as a percentage of the policy's nodes.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	// +optional
	MaxUnavailablePercent *int32 `json:"maxUnavailablePercent,omitempty"`

	// DrainScope selects which pods are evicted: gpu or all.
	// +optional
	DrainScope *DrainScope `json:"drainScope,omitempty"`

	// DryRun logs and emits events but never cordons, taints or evicts.
	// +optional
	DryRun *bool `json:"dryRun,omitempty"`

	// Escalation, when set, repairs quarantined nodes that do not recover on
	// their own: reboot them (which resets the GPU state a critical XID or ECC
	// counter latches), then ask for a replacement once reboots are exhausted.
	// Omit it to leave quarantined nodes for a human.
	// +optional
	Escalation *EscalationSpec `json:"escalation,omitempty"`
}

// EscalationAction is the first-line repair for a node that stays unhealthy.
// +kubebuilder:validation:Enum=Reboot;RequestReplacement
type EscalationAction string

const (
	// EscalateReboot tries reboots first, then requests a replacement.
	EscalateReboot EscalationAction = "Reboot"
	// EscalateReplacement skips reboots and only requests a replacement.
	EscalateReplacement EscalationAction = "RequestReplacement"
)

// EscalationSpec bounds automated repair. Every reboot happens only on a node
// that is already cordoned, tainted and fully drained through the Eviction API.
type EscalationSpec struct {
	// After is how long a node must stay quarantined (and unhealthy) before the
	// first repair step.
	// +kubebuilder:validation:XValidation:rule="duration(self) > duration('0s')",message="must be a positive duration such as 15m"
	After metav1.Duration `json:"after"`

	// Action is the first-line repair. Default Reboot.
	// +optional
	Action *EscalationAction `json:"action,omitempty"`

	// MaxAttempts is how many reboots to try inside historyWindow before asking
	// for a replacement. Default 2.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=5
	// +optional
	MaxAttempts *int32 `json:"maxAttempts,omitempty"`

	// Cooldown is the minimum time between reboot attempts on one node; it must
	// cover the node coming back and its agent reporting. Default 20m.
	// +kubebuilder:validation:XValidation:rule="duration(self) > duration('0s')",message="must be a positive duration such as 20m"
	// +optional
	Cooldown *metav1.Duration `json:"cooldown,omitempty"`

	// MaxConcurrent caps reboots in flight across this policy's nodes. Default 1.
	// +kubebuilder:validation:Minimum=1
	// +optional
	MaxConcurrent *int32 `json:"maxConcurrent,omitempty"`

	// HistoryWindow: reboots older than this stop counting against maxAttempts,
	// so a node that stays healthy for a day gets a fresh budget. Default 24h.
	// +kubebuilder:validation:XValidation:rule="duration(self) > duration('0s')",message="must be a positive duration such as 24h"
	// +optional
	HistoryWindow *metav1.Duration `json:"historyWindow,omitempty"`
}

// GPUNodePolicyStatus summarizes the nodes a policy governs.
type GPUNodePolicyStatus struct {
	// ObservedGeneration is the spec generation this status describes.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// ManagedNodes is how many nodes this policy governs.
	ManagedNodes int32 `json:"managedNodes"`
	// HealthyNodes have a fresh GPUHealthy=True condition.
	HealthyNodes int32 `json:"healthyNodes"`
	// UnhealthyNodes have GPUHealthy=False.
	UnhealthyNodes int32 `json:"unhealthyNodes"`
	// QuarantinedNodes are currently cordoned and tainted by the controller.
	QuarantinedNodes int32 `json:"quarantinedNodes"`
	// Budget is the most nodes this policy allows to be quarantined at once.
	Budget int32 `json:"budget"`
	// RepairingNodes have a reboot in flight (attempted within the cooldown).
	RepairingNodes int32 `json:"repairingNodes"`
	// ReplacementRequestedNodes exhausted their reboots and await replacement.
	ReplacementRequestedNodes int32 `json:"replacementRequestedNodes"`

	// QuarantinedNodeNames lists up to 50 quarantined nodes.
	// +kubebuilder:validation:MaxItems=50
	// +listType=atomic
	// +optional
	QuarantinedNodeNames []string `json:"quarantinedNodeNames,omitempty"`

	// Conditions: Ready and BudgetExhausted.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// GPUNodePolicy configures remediation for a set of GPU nodes.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=gnp
// +kubebuilder:printcolumn:name="Managed",type=integer,JSONPath=`.status.managedNodes`
// +kubebuilder:printcolumn:name="Unhealthy",type=integer,JSONPath=`.status.unhealthyNodes`
// +kubebuilder:printcolumn:name="Quarantined",type=integer,JSONPath=`.status.quarantinedNodes`
// +kubebuilder:printcolumn:name="Budget",type=integer,JSONPath=`.status.budget`
// +kubebuilder:printcolumn:name="Repairing",type=integer,JSONPath=`.status.repairingNodes`
// +kubebuilder:printcolumn:name="NeedReplacement",type=integer,JSONPath=`.status.replacementRequestedNodes`
// +kubebuilder:printcolumn:name="DryRun",type=boolean,JSONPath=`.spec.dryRun`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type GPUNodePolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GPUNodePolicySpec   `json:"spec"`
	Status GPUNodePolicyStatus `json:"status,omitempty"`
}

// GPUNodePolicyList is a list of GPUNodePolicy.
//
// +kubebuilder:object:root=true
type GPUNodePolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GPUNodePolicy `json:"items"`
}

func init() {
	SchemeBuilder.Register(&GPUNodePolicy{}, &GPUNodePolicyList{})
}
