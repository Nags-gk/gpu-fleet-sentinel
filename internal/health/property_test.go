package health

import (
	"fmt"
	"math/rand"
	"reflect"
	"testing"
	"time"
)

// A debounced node turns unhealthy only after FailAfter consecutive critical
// reports, recovers only after RecoverAfter consecutive non-critical ones, and
// must change state as soon as those runs are complete.
func TestDebouncerProperty(t *testing.T) {
	for seed := int64(0); seed < 2000; seed++ {
		r := rand.New(rand.NewSource(seed))
		fail, rec := 1+r.Intn(5), 1+r.Intn(5)
		d := Debouncer{FailAfter: fail, RecoverAfter: rec}
		if r.Intn(2) == 0 {
			d.Seed(r.Intn(2) == 0)
		}

		var hist []Severity
		prev := d.healthy || !d.started // an unseeded debouncer starts healthy
		for i := 0; i < 80; i++ {
			s := Severity(r.Intn(3))
			// Bias toward long runs so state changes actually happen.
			if len(hist) > 0 && r.Intn(3) > 0 {
				s = hist[len(hist)-1]
			}
			hist = append(hist, s)
			got := d.Observe(s)

			allLast := func(n int, crit bool) bool {
				if len(hist) < n {
					return false
				}
				for _, h := range hist[len(hist)-n:] {
					if (h == SeverityCritical) != crit {
						return false
					}
				}
				return true
			}
			ctx := fmt.Sprintf("seed %d step %d (fail=%d recover=%d) history=%v", seed, i, fail, rec, hist)
			if prev && !got && !allLast(fail, true) {
				t.Fatalf("%s: turned unhealthy without %d consecutive critical reports", ctx, fail)
			}
			if !prev && got && !allLast(rec, false) {
				t.Fatalf("%s: recovered without %d consecutive good reports", ctx, rec)
			}
			if prev && got && allLast(fail, true) {
				t.Fatalf("%s: stayed healthy through %d consecutive critical reports", ctx, fail)
			}
			if !prev && !got && allLast(rec, false) {
				t.Fatalf("%s: stayed unhealthy through %d consecutive good reports", ctx, rec)
			}
			prev = got
		}
	}
}

func randSample(r *rand.Rand, i int) GPUSample {
	s := GPUSample{Index: i, UUID: fmt.Sprintf("GPU-%d", i), TempC: float64(40 + r.Intn(70))}
	if r.Intn(6) == 0 {
		s.LastXID = []int{13, 31, 48, 63, 64, 79, 94, 119}[r.Intn(8)]
	}
	if r.Intn(8) == 0 {
		s.ECCDoubleBit = uint64(r.Intn(4))
	}
	s.RowRemapFailure = r.Intn(10) == 0
	s.ECCSingleBit = uint64(r.Intn(500))
	s.NVLinkCRCErrors = uint64(r.Intn(500))
	return s
}

// The result must not depend on the order GPUs are listed in.
func TestEvaluateIsOrderIndependent(t *testing.T) {
	for seed := int64(0); seed < 1000; seed++ {
		r := rand.New(rand.NewSource(seed))
		n := 1 + r.Intn(8)
		samples := make([]GPUSample, n)
		for i := range samples {
			samples[i] = randSample(r, i)
		}
		shuffled := append([]GPUSample(nil), samples...)
		r.Shuffle(n, func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })

		th := Thresholds{ExpectedGPUs: n + r.Intn(2)}
		a, b := NewEvaluator(th), NewEvaluator(th)
		now := time.Unix(0, 0)
		for round := 0; round < 3; round++ {
			for i := range samples { // counters grow between rounds
				samples[i].ECCSingleBit += uint64(r.Intn(300))
			}
			copy(shuffled, samples)
			r.Shuffle(n, func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
			ra := a.Evaluate(Snapshot{Time: now, Samples: samples})
			rb := b.Evaluate(Snapshot{Time: now, Samples: shuffled})
			if !reflect.DeepEqual(ra.Findings, rb.Findings) || ra.Status() != rb.Status() {
				t.Fatalf("seed %d round %d: GPU order changed the result\n%+v\n%+v", seed, round, ra.Findings, rb.Findings)
			}
		}
	}
}

// Slow growth of rate counters, or a counter reset, never raises a finding.
func TestRateRulesQuietBelowThreshold(t *testing.T) {
	for seed := int64(0); seed < 1000; seed++ {
		r := rand.New(rand.NewSource(seed))
		e := NewEvaluator(Thresholds{SBEDeltaWarn: 100, NVLinkCRCDeltaWarn: 100})
		g := GPUSample{Index: 0, UUID: "u", TempC: 60, ECCSingleBit: uint64(r.Intn(1e6)), NVLinkCRCErrors: uint64(r.Intn(1e6))}
		e.Evaluate(Snapshot{Samples: []GPUSample{g}})
		for i := 0; i < 50; i++ {
			if r.Intn(10) == 0 { // driver reload resets the counters
				g.ECCSingleBit, g.NVLinkCRCErrors = 0, 0
			} else {
				g.ECCSingleBit += uint64(r.Intn(100)) // < threshold
				g.NVLinkCRCErrors += uint64(r.Intn(100))
			}
			if rep := e.Evaluate(Snapshot{Samples: []GPUSample{g}}); rep.Status() != SeverityOK {
				t.Fatalf("seed %d step %d: sub-threshold growth raised %+v", seed, i, rep.Findings)
			}
		}
	}
}
