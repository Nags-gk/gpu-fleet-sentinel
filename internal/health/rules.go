package health

import (
	"fmt"
	"sort"
	"sync"
)

// XID codes the agent treats as hardware faults that warrant draining the
// node. dcgm-exporter reports the last XID until the GPU is reset or the node
// reboots, so a critical XID keeps the node quarantined until that reset; that
// is intended, since a faulted GPU should not return to service unrepaired. Codes not listed here (e.g. 13, 31, 43, 45) are usually caused by the
// application rather than the hardware and are reported as warnings.
// Reference: NVIDIA "XID Errors" documentation.
var criticalXIDs = map[int]string{
	48:  "double-bit ECC error",
	61:  "internal micro-controller breakpoint/warning",
	62:  "internal micro-controller halt",
	64:  "ECC page retirement or row remapper recording failure",
	74:  "NVLink error",
	79:  "GPU has fallen off the bus",
	92:  "high single-bit ECC error rate",
	94:  "contained ECC error",
	95:  "uncontained ECC error",
	119: "GSP RPC timeout",
	120: "GSP error",
}

// XIDs that report a successful self-heal rather than a fault. XID 63 means a
// row remap or page retirement was recorded; the failure case is XID 64. Treating
// 63 as critical would drain a node whose memory just repaired itself.
var infoXIDs = map[int]string{
	63: "row remap/page retirement recorded; reset the GPU to apply",
}

// Thresholds configures the rule engine. Zero values are replaced by defaults.
type Thresholds struct {
	TempWarnC     float64 // warn at or above
	TempCriticalC float64 // critical at or above
	// SBE errors gained between two consecutive samples that count as a warning.
	SBEDeltaWarn uint64
	// NVLink CRC errors gained between two consecutive samples that count as a warning.
	NVLinkCRCDeltaWarn uint64
	// ExpectedGPUs is how many GPUs the node should report; 0 disables the check.
	ExpectedGPUs int
}

// DefaultThresholds are conservative values for data-center GPUs.
func DefaultThresholds() Thresholds {
	return Thresholds{
		TempWarnC:          83,
		TempCriticalC:      90,
		SBEDeltaWarn:       100,
		NVLinkCRCDeltaWarn: 100,
	}
}

func (t Thresholds) withDefaults() Thresholds {
	d := DefaultThresholds()
	if t.TempWarnC == 0 {
		t.TempWarnC = d.TempWarnC
	}
	if t.TempCriticalC == 0 {
		t.TempCriticalC = d.TempCriticalC
	}
	if t.SBEDeltaWarn == 0 {
		t.SBEDeltaWarn = d.SBEDeltaWarn
	}
	if t.NVLinkCRCDeltaWarn == 0 {
		t.NVLinkCRCDeltaWarn = d.NVLinkCRCDeltaWarn
	}
	return t
}

// Evaluator applies the rules. It keeps the previous sample per GPU so it can
// detect counters that are growing too fast. Safe for concurrent use.
type Evaluator struct {
	t    Thresholds
	mu   sync.Mutex
	prev map[string]GPUSample // keyed by UUID (falls back to index)
}

// NewEvaluator builds an Evaluator; zero-valued thresholds take defaults.
func NewEvaluator(t Thresholds) *Evaluator {
	return &Evaluator{t: t.withDefaults(), prev: map[string]GPUSample{}}
}

func key(s GPUSample) string {
	if s.UUID != "" {
		return s.UUID
	}
	return fmt.Sprintf("idx-%d", s.Index)
}

// Evaluate checks every GPU in the snapshot and returns a Report whose
// findings are sorted by GPU index then rule name for stable output.
func (e *Evaluator) Evaluate(snap Snapshot) Report {
	e.mu.Lock()
	defer e.mu.Unlock()

	r := Report{Node: snap.Node, Time: snap.Time, GPUCount: len(snap.Samples)}
	t := e.t

	if t.ExpectedGPUs > 0 && len(snap.Samples) < t.ExpectedGPUs {
		r.Findings = append(r.Findings, Finding{
			GPU: -1, Rule: "gpu-missing", Severity: SeverityCritical,
			Message: fmt.Sprintf("only %d of %d expected GPUs visible", len(snap.Samples), t.ExpectedGPUs),
		})
	}

	for _, s := range snap.Samples {
		add := func(rule string, sev Severity, format string, args ...any) {
			r.Findings = append(r.Findings, Finding{
				GPU: s.Index, Rule: rule, Severity: sev,
				Message: fmt.Sprintf("GPU%d: ", s.Index) + fmt.Sprintf(format, args...),
			})
		}

		switch {
		case s.TempC >= t.TempCriticalC:
			add("temperature", SeverityCritical, "temperature %.0fC >= %.0fC", s.TempC, t.TempCriticalC)
		case s.TempC >= t.TempWarnC:
			add("temperature", SeverityWarning, "temperature %.0fC >= %.0fC", s.TempC, t.TempWarnC)
		}

		if s.ECCDoubleBit > 0 {
			add("ecc-dbe", SeverityCritical, "%d uncorrectable double-bit ECC errors", s.ECCDoubleBit)
		}
		if s.RowRemapFailure {
			add("row-remap-failure", SeverityCritical, "row remapping failed; GPU memory needs service")
		}

		if s.LastXID != 0 {
			if desc, ok := criticalXIDs[s.LastXID]; ok {
				add("xid", SeverityCritical, "XID %d (%s)", s.LastXID, desc)
			} else if desc, ok := infoXIDs[s.LastXID]; ok {
				add("xid", SeverityWarning, "XID %d (%s)", s.LastXID, desc)
			} else {
				add("xid", SeverityWarning, "XID %d (likely application-level)", s.LastXID)
			}
		}

		if p, ok := e.prev[key(s)]; ok {
			// Counters reset when the driver reloads; only positive growth counts.
			if s.ECCSingleBit > p.ECCSingleBit && s.ECCSingleBit-p.ECCSingleBit >= t.SBEDeltaWarn {
				add("ecc-sbe-rate", SeverityWarning, "%d new single-bit ECC errors since last sample", s.ECCSingleBit-p.ECCSingleBit)
			}
			if s.NVLinkCRCErrors > p.NVLinkCRCErrors && s.NVLinkCRCErrors-p.NVLinkCRCErrors >= t.NVLinkCRCDeltaWarn {
				add("nvlink-crc-rate", SeverityWarning, "%d new NVLink CRC errors since last sample", s.NVLinkCRCErrors-p.NVLinkCRCErrors)
			}
		}
		e.prev[key(s)] = s
	}

	sort.SliceStable(r.Findings, func(i, j int) bool {
		if r.Findings[i].GPU != r.Findings[j].GPU {
			return r.Findings[i].GPU < r.Findings[j].GPU
		}
		return r.Findings[i].Rule < r.Findings[j].Rule
	})
	return r
}

// Debouncer adds hysteresis so one bad or good sample does not flap a node.
// A node turns unhealthy after FailAfter consecutive critical reports and
// healthy again only after RecoverAfter consecutive non-critical reports.
type Debouncer struct {
	FailAfter    int
	RecoverAfter int

	healthy bool
	badRun  int
	goodRun int
	started bool
}

// Seed sets the starting state before the first Observe, so a restarted agent
// resumes from the condition already published instead of briefly reporting
// healthy. It has no effect once Observe has been called.
func (d *Debouncer) Seed(healthy bool) {
	if !d.started {
		d.started, d.healthy = true, healthy
	}
}

// Observe records one report status and returns the debounced health.
func (d *Debouncer) Observe(status Severity) (healthy bool) {
	if !d.started {
		d.started, d.healthy = true, true
	}
	failAfter, recoverAfter := max(d.FailAfter, 1), max(d.RecoverAfter, 1)

	if status == SeverityCritical {
		d.badRun++
		d.goodRun = 0
		if d.healthy && d.badRun >= failAfter {
			d.healthy = false
		}
	} else {
		d.goodRun++
		d.badRun = 0
		if !d.healthy && d.goodRun >= recoverAfter {
			d.healthy = true
		}
	}
	return d.healthy
}
