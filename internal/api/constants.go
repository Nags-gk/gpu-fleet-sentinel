// Package api holds the names shared by the agent and the controller: the node
// condition the agent writes and the taint/annotations the controller owns.
package api

const (
	// Domain prefixes every key this project owns.
	Domain = "gpu-sentinel.io"

	// ConditionGPUHealthy is the node condition the agent maintains.
	// Status True means every GPU on the node is healthy.
	ConditionGPUHealthy = "GPUHealthy"

	// ReasonHealthy is the condition reason when every GPU passes the rules.
	ReasonHealthy = "GPUsHealthy"
	// ReasonFault is the condition reason when a critical finding persists.
	ReasonFault = "GPUFault"
	// ReasonScrapeFailed is the condition reason when telemetry can't be read.
	ReasonScrapeFailed = "TelemetryUnavailable"

	// TaintKey is applied (NoSchedule) to quarantined nodes.
	TaintKey = Domain + "/unhealthy"

	// AnnotationQuarantinedAt marks a node quarantined by this controller (RFC3339).
	AnnotationQuarantinedAt = Domain + "/quarantined-at"
	// AnnotationReason stores the health message that triggered quarantine.
	AnnotationReason = Domain + "/reason"
	// AnnotationWasUnschedulable records that a human had already cordoned the
	// node, so release must not uncordon it.
	AnnotationWasUnschedulable = Domain + "/was-unschedulable"
	// AnnotationIncidentSummary holds the incident summary written on quarantine.
	AnnotationIncidentSummary = Domain + "/incident-summary"

	// DefaultGPUNodeSelector matches nodes labeled by NVIDIA GPU Feature Discovery.
	DefaultGPUNodeSelector = "nvidia.com/gpu.present=true"

	// GPUResource is the extended resource name exposed by the NVIDIA device plugin.
	GPUResource = "nvidia.com/gpu"
)
