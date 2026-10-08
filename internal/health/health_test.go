package health

import (
	"context"
	"strings"
	"testing"
	"time"
)

func snap(samples ...GPUSample) Snapshot {
	return Snapshot{Node: "n1", Time: time.Unix(0, 0), Samples: samples}
}

func healthy(i int) GPUSample {
	return GPUSample{Index: i, UUID: "u" + string(rune('0'+i)), TempC: 60}
}

func TestEvaluateRules(t *testing.T) {
	tests := []struct {
		name     string
		sample   GPUSample
		wantRule string
		wantSev  Severity
	}{
		{"healthy", healthy(0), "", SeverityOK},
		{"warm", GPUSample{Index: 0, TempC: 85}, "temperature", SeverityWarning},
		{"hot", GPUSample{Index: 0, TempC: 95}, "temperature", SeverityCritical},
		{"dbe", GPUSample{Index: 0, TempC: 60, ECCDoubleBit: 2}, "ecc-dbe", SeverityCritical},
		{"row remap", GPUSample{Index: 0, TempC: 60, RowRemapFailure: true}, "row-remap-failure", SeverityCritical},
		{"xid 79 critical", GPUSample{Index: 0, TempC: 60, LastXID: 79}, "xid", SeverityCritical},
		{"xid 63 remap recorded is not a fault", GPUSample{Index: 0, TempC: 60, LastXID: 63}, "xid", SeverityWarning},
		{"xid 64 remap failure", GPUSample{Index: 0, TempC: 60, LastXID: 64}, "xid", SeverityCritical},
		{"xid 13 app-level", GPUSample{Index: 0, TempC: 60, LastXID: 13}, "xid", SeverityWarning},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := NewEvaluator(Thresholds{}).Evaluate(snap(tc.sample))
			if got := r.Status(); got != tc.wantSev {
				t.Fatalf("status = %v, want %v (findings %+v)", got, tc.wantSev, r.Findings)
			}
			if tc.wantRule == "" {
				if len(r.Findings) != 0 {
					t.Fatalf("expected no findings, got %+v", r.Findings)
				}
				return
			}
			if r.Findings[0].Rule != tc.wantRule {
				t.Fatalf("rule = %q, want %q", r.Findings[0].Rule, tc.wantRule)
			}
		})
	}
}

func TestRateRulesNeedTwoSamplesAndIgnoreCounterReset(t *testing.T) {
	e := NewEvaluator(Thresholds{SBEDeltaWarn: 50})
	g := healthy(0)
	g.ECCSingleBit = 10
	if r := e.Evaluate(snap(g)); r.Status() != SeverityOK {
		t.Fatalf("first sample has no baseline, got %+v", r.Findings)
	}
	g.ECCSingleBit = 100 // +90
	r := e.Evaluate(snap(g))
	if r.Status() != SeverityWarning || r.Findings[0].Rule != "ecc-sbe-rate" {
		t.Fatalf("expected sbe-rate warning, got %+v", r.Findings)
	}
	g.ECCSingleBit = 0 // driver reload resets counters
	if r := e.Evaluate(snap(g)); r.Status() != SeverityOK {
		t.Fatalf("counter reset must not alert, got %+v", r.Findings)
	}
}

func TestMissingGPU(t *testing.T) {
	r := NewEvaluator(Thresholds{ExpectedGPUs: 2}).Evaluate(snap(healthy(0)))
	if r.Status() != SeverityCritical || r.Findings[0].Rule != "gpu-missing" || r.Findings[0].GPU != -1 {
		t.Fatalf("expected node-level gpu-missing, got %+v", r.Findings)
	}
}

func TestSummaryAndGPUStatus(t *testing.T) {
	r := NewEvaluator(Thresholds{}).Evaluate(snap(
		healthy(0),
		GPUSample{Index: 1, TempC: 85},
		GPUSample{Index: 2, TempC: 60, LastXID: 79},
	))
	if r.GPUStatus(0) != SeverityOK || r.GPUStatus(1) != SeverityWarning || r.GPUStatus(2) != SeverityCritical {
		t.Fatalf("per-GPU status wrong: %+v", r.Findings)
	}
	if s := r.Summary(); !strings.Contains(s, "XID 79") || strings.Contains(s, "temperature") {
		t.Fatalf("summary should list only the worst severity, got %q", s)
	}
	ok := NewEvaluator(Thresholds{}).Evaluate(snap(healthy(0), healthy(1)))
	if ok.Summary() != "all 2 GPUs healthy" {
		t.Fatalf("got %q", ok.Summary())
	}
}

func TestDebouncer(t *testing.T) {
	d := Debouncer{FailAfter: 2, RecoverAfter: 3}
	seq := []struct {
		in   Severity
		want bool
	}{
		{SeverityCritical, true},  // 1 bad: still healthy
		{SeverityOK, true},        // run reset
		{SeverityCritical, true},  // 1 bad
		{SeverityCritical, false}, // 2 bad: unhealthy
		{SeverityWarning, false},  // warnings count as good, 1
		{SeverityOK, false},       // 2
		{SeverityCritical, false}, // reset good run
		{SeverityOK, false},
		{SeverityOK, false},
		{SeverityOK, true}, // 3 good: recovered
	}
	for i, s := range seq {
		if got := d.Observe(s.in); got != s.want {
			t.Fatalf("step %d: Observe(%v) = %v, want %v", i, s.in, got, s.want)
		}
	}
}

func TestSimSourceFaults(t *testing.T) {
	ctx := context.Background()
	sim := NewSimSource("n1", "H100", 4, 1)
	e := NewEvaluator(Thresholds{ExpectedGPUs: 4})

	samples, _ := sim.Sample(ctx)
	if r := e.Evaluate(Snapshot{Samples: samples}); r.Status() != SeverityOK {
		t.Fatalf("fresh sim should be healthy: %+v", r.Findings)
	}

	cases := map[string]Severity{
		FaultXID79: SeverityCritical, FaultXID13: SeverityWarning, FaultOverheat: SeverityCritical,
		FaultECCDBE: SeverityCritical, FaultRowRemap: SeverityCritical, FaultDisappear: SeverityCritical,
	}
	for fault, want := range cases {
		if err := sim.Inject(1, fault); err != nil {
			t.Fatal(err)
		}
		samples, _ := sim.Sample(ctx)
		if got := e.Evaluate(Snapshot{Samples: samples}).Status(); got != want {
			t.Errorf("fault %s: status %v, want %v", fault, got, want)
		}
		_ = sim.Inject(1, FaultNone)
		samples, _ = sim.Sample(ctx)
		e.Evaluate(Snapshot{Samples: samples}) // refresh baselines
	}

	// Rate faults only show up from the second sample onward.
	_ = sim.Inject(2, FaultSBEStorm)
	s1, _ := sim.Sample(ctx)
	e.Evaluate(Snapshot{Samples: s1})
	s2, _ := sim.Sample(ctx)
	if got := e.Evaluate(Snapshot{Samples: s2}).Status(); got != SeverityWarning {
		t.Errorf("sbe-storm: status %v, want warning", got)
	}

	if err := sim.Inject(9, FaultXID79); err == nil {
		t.Error("expected out-of-range error")
	}
	if err := sim.Inject(0, "bogus"); err == nil {
		t.Error("expected unknown-fault error")
	}
}

const dcgmFixture = `# HELP DCGM_FI_DEV_GPU_TEMP GPU temperature (in C).
# TYPE DCGM_FI_DEV_GPU_TEMP gauge
DCGM_FI_DEV_GPU_TEMP{gpu="0",UUID="GPU-aaa",device="nvidia0",modelName="NVIDIA H100 80GB HBM3",Hostname="node-a"} 61
DCGM_FI_DEV_GPU_TEMP{gpu="1",UUID="GPU-bbb",device="nvidia1",modelName="NVIDIA H100 80GB HBM3",Hostname="node-a"} 92
# HELP DCGM_FI_DEV_XID_ERRORS Value of the last XID error encountered.
# TYPE DCGM_FI_DEV_XID_ERRORS gauge
DCGM_FI_DEV_XID_ERRORS{gpu="0",UUID="GPU-aaa",device="nvidia0",modelName="NVIDIA H100 80GB HBM3",Hostname="node-a"} 79
DCGM_FI_DEV_XID_ERRORS{gpu="1",UUID="GPU-bbb",device="nvidia1",modelName="NVIDIA H100 80GB HBM3",Hostname="node-a"} 0
# HELP DCGM_FI_DEV_ECC_DBE_VOL_TOTAL Total number of double-bit volatile ECC errors.
# TYPE DCGM_FI_DEV_ECC_DBE_VOL_TOTAL counter
DCGM_FI_DEV_ECC_DBE_VOL_TOTAL{gpu="0",UUID="GPU-aaa",device="nvidia0",modelName="NVIDIA H100 80GB HBM3",Hostname="node-a"} 0
DCGM_FI_DEV_ECC_DBE_VOL_TOTAL{gpu="1",UUID="GPU-bbb",device="nvidia1",modelName="NVIDIA H100 80GB HBM3",Hostname="node-a"} 4
# HELP DCGM_FI_DEV_POWER_USAGE Power draw (in W).
# TYPE DCGM_FI_DEV_POWER_USAGE gauge
DCGM_FI_DEV_POWER_USAGE{gpu="0",UUID="GPU-aaa",device="nvidia0",modelName="NVIDIA H100 80GB HBM3",Hostname="node-a"} 312.5
# HELP DCGM_FI_DEV_FB_USED Framebuffer memory used (in MiB).
# TYPE DCGM_FI_DEV_FB_USED gauge
DCGM_FI_DEV_FB_USED{gpu="0",UUID="GPU-aaa",device="nvidia0",modelName="NVIDIA H100 80GB HBM3",Hostname="node-a"} 1024
`

func TestParseDCGM(t *testing.T) {
	got, err := ParseDCGM(strings.NewReader(dcgmFixture))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 GPUs, got %d", len(got))
	}
	g0, g1 := got[0], got[1]
	if g0.UUID != "GPU-aaa" || g0.Model != "NVIDIA H100 80GB HBM3" || g0.LastXID != 79 || g0.PowerW != 312.5 {
		t.Fatalf("gpu0 parsed wrong: %+v", g0)
	}
	if g1.TempC != 92 || g1.ECCDoubleBit != 4 {
		t.Fatalf("gpu1 parsed wrong: %+v", g1)
	}
	r := NewEvaluator(Thresholds{}).Evaluate(Snapshot{Samples: got})
	if r.GPUStatus(0) != SeverityCritical || r.GPUStatus(1) != SeverityCritical {
		t.Fatalf("expected both GPUs critical: %+v", r.Findings)
	}
}

func TestParseDCGMRejectsGarbage(t *testing.T) {
	if _, err := ParseDCGM(strings.NewReader("not { valid")); err == nil {
		t.Fatal("expected parse error")
	}
}

func TestDebouncerSeed(t *testing.T) {
	d := Debouncer{FailAfter: 2, RecoverAfter: 2}
	d.Seed(false)
	if d.Observe(SeverityCritical) {
		t.Fatal("seeded-unhealthy debouncer must not report healthy on a critical sample")
	}
	if d.Observe(SeverityOK) {
		t.Fatal("one good sample is not enough to recover")
	}
	if !d.Observe(SeverityOK) {
		t.Fatal("two good samples should recover")
	}
	d.Seed(false) // ignored once started
	if !d.Observe(SeverityOK) {
		t.Fatal("Seed after Observe must be a no-op")
	}
}
