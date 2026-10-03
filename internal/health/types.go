// Package health turns raw per-GPU telemetry into health findings.
//
// It is deliberately free of Kubernetes dependencies so the rules can be
// unit-tested in isolation and reused by both the node agent and offline tools.
package health

import (
	"fmt"
	"sort"
	"time"
)

// Severity orders how bad a finding is. Higher is worse.
type Severity int

const (
	// SeverityOK means no rule fired.
	SeverityOK Severity = iota
	// SeverityWarning is worth a metric and a log line but no remediation.
	SeverityWarning
	// SeverityCritical means the GPU should not run workloads.
	SeverityCritical
)

func (s Severity) String() string {
	switch s {
	case SeverityOK:
		return "ok"
	case SeverityWarning:
		return "warning"
	case SeverityCritical:
		return "critical"
	default:
		return fmt.Sprintf("severity(%d)", int(s))
	}
}

// GPUSample is one point-in-time reading for a single GPU. Field names follow
// the DCGM field they are sourced from (see source_dcgm.go).
type GPUSample struct {
	Index int
	UUID  string
	Model string

	TempC   float64 // DCGM_FI_DEV_GPU_TEMP
	PowerW  float64 // DCGM_FI_DEV_POWER_USAGE
	UtilPct float64 // DCGM_FI_DEV_GPU_UTIL

	// Volatile (since driver load) ECC error counters.
	ECCSingleBit uint64 // DCGM_FI_DEV_ECC_SBE_VOL_TOTAL
	ECCDoubleBit uint64 // DCGM_FI_DEV_ECC_DBE_VOL_TOTAL

	RowRemapFailure bool   // DCGM_FI_DEV_ROW_REMAP_FAILURE
	NVLinkCRCErrors uint64 // DCGM_FI_DEV_NVLINK_CRC_FLIT_ERROR_COUNT_TOTAL

	// LastXID is the most recent NVIDIA driver XID event code; 0 means none.
	LastXID int // DCGM_FI_DEV_XID_ERRORS
}

// Snapshot is every GPU reading taken from one node at one moment.
type Snapshot struct {
	Node    string
	Time    time.Time
	Samples []GPUSample
}

// Finding is one rule violation on one GPU. GPU is -1 for node-level findings
// (for example, a GPU that disappeared from the PCIe bus).
type Finding struct {
	GPU      int
	Rule     string
	Severity Severity
	Message  string
}

// Report is the evaluated result of a Snapshot.
type Report struct {
	Node     string
	Time     time.Time
	Findings []Finding
	GPUCount int
}

// Status is the worst severity across all findings.
func (r Report) Status() Severity {
	worst := SeverityOK
	for _, f := range r.Findings {
		if f.Severity > worst {
			worst = f.Severity
		}
	}
	return worst
}

// GPUStatus returns the worst severity for one GPU index.
func (r Report) GPUStatus(gpu int) Severity {
	worst := SeverityOK
	for _, f := range r.Findings {
		if f.GPU == gpu && f.Severity > worst {
			worst = f.Severity
		}
	}
	return worst
}

// Summary renders the critical (or, failing that, warning) findings as one
// line suitable for a Kubernetes condition message.
func (r Report) Summary() string {
	status := r.Status()
	if status == SeverityOK {
		return fmt.Sprintf("all %d GPUs healthy", r.GPUCount)
	}
	var msgs []string
	for _, f := range r.Findings {
		if f.Severity == status {
			msgs = append(msgs, f.Message)
		}
	}
	sort.Strings(msgs)
	out := ""
	for i, m := range msgs {
		if i == 3 {
			out += fmt.Sprintf("; +%d more", len(msgs)-3)
			break
		}
		if i > 0 {
			out += "; "
		}
		out += m
	}
	return out
}
