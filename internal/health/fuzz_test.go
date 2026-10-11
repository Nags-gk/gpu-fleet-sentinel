package health

import (
	"math"
	"sort"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// FuzzParseDCGM feeds arbitrary exposition text to the parser. The exporter is
// an external process on every node, so its output must never be able to crash
// the agent or produce samples the rest of the pipeline cannot handle.
func FuzzParseDCGM(f *testing.F) {
	f.Add(dcgmFixture)
	f.Add("not { valid")
	f.Add("")
	f.Add("DCGM_FI_DEV_GPU_TEMP{gpu=\"0\"} NaN\nDCGM_FI_DEV_XID_ERRORS{gpu=\"0\"} +Inf\n")
	f.Add("DCGM_FI_DEV_ECC_DBE_VOL_TOTAL{gpu=\"0\"} -5\nDCGM_FI_DEV_ECC_SBE_VOL_TOTAL{gpu=\"0\"} 1e30\n")
	f.Add("DCGM_FI_DEV_GPU_TEMP{gpu=\"-1\"} 70\nDCGM_FI_DEV_GPU_TEMP{gpu=\"99999999999999999999\"} 70\n")
	f.Fuzz(func(t *testing.T, in string) {
		samples, err := ParseDCGM(strings.NewReader(in))
		if err != nil {
			return
		}
		seen := map[int]bool{}
		for i, s := range samples {
			if s.Index < 0 {
				t.Fatalf("negative GPU index %d", s.Index)
			}
			if seen[s.Index] {
				t.Fatalf("duplicate GPU index %d", s.Index)
			}
			seen[s.Index] = true
			if i > 0 && samples[i-1].Index >= s.Index {
				t.Fatalf("samples not sorted by index: %d then %d", samples[i-1].Index, s.Index)
			}
			for name, v := range map[string]float64{"temp": s.TempC, "power": s.PowerW, "util": s.UtilPct} {
				if math.IsNaN(v) || math.IsInf(v, 0) {
					t.Fatalf("%s is %v: non-finite readings must not reach the rule engine", name, v)
				}
			}
			if s.LastXID < 0 {
				t.Fatalf("negative XID %d", s.LastXID)
			}
		}
	})
}

// FuzzEvaluate checks the rule engine against arbitrary readings: no panic,
// stable ordering, and status always equal to the worst finding.
func FuzzEvaluate(f *testing.F) {
	f.Add(60.0, 300.0, uint64(0), uint64(0), false, uint64(0), 0, 2)
	f.Add(95.0, 0.0, uint64(5), uint64(1), true, uint64(1000), 79, 1)
	f.Add(math.NaN(), math.Inf(1), uint64(math.MaxUint64), uint64(math.MaxUint64), false, uint64(math.MaxUint64), -3, 0)
	f.Fuzz(func(t *testing.T, temp, power float64, sbe, dbe uint64, remap bool, crc uint64, xid, gpus int) {
		gpus = ((gpus % 9) + 9) % 9
		e := NewEvaluator(Thresholds{ExpectedGPUs: 4})
		for round := 0; round < 3; round++ { // rate rules need history
			var samples []GPUSample
			for i := 0; i < gpus; i++ {
				samples = append(samples, GPUSample{Index: i, UUID: "u", TempC: temp, PowerW: power,
					ECCSingleBit: sbe * uint64(round), ECCDoubleBit: dbe, RowRemapFailure: remap,
					NVLinkCRCErrors: crc * uint64(round), LastXID: xid})
			}
			r := e.Evaluate(Snapshot{Node: "n", Time: time.Unix(int64(round), 0), Samples: samples})

			worst := SeverityOK
			for _, fd := range r.Findings {
				worst = max(worst, fd.Severity)
				if fd.Message == "" || fd.Rule == "" {
					t.Fatalf("finding missing rule/message: %+v", fd)
				}
			}
			if r.Status() != worst {
				t.Fatalf("Status %v != worst finding %v", r.Status(), worst)
			}
			if !sort.SliceIsSorted(r.Findings, func(i, j int) bool {
				if r.Findings[i].GPU != r.Findings[j].GPU {
					return r.Findings[i].GPU < r.Findings[j].GPU
				}
				return r.Findings[i].Rule < r.Findings[j].Rule
			}) {
				t.Fatalf("findings not sorted: %+v", r.Findings)
			}
			if s := r.Summary(); !utf8.ValidString(s) {
				t.Fatalf("summary is not valid UTF-8: %q", s)
			}
		}
	})
}
