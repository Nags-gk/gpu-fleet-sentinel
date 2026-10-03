package health

import (
	"context"
	"fmt"
	"math/rand"
	"strings"
	"sync"
)

// Fault names accepted by SimSource.Inject.
const (
	FaultNone      = "none"
	FaultXID79     = "xid79"      // GPU fell off the bus
	FaultXID48     = "xid48"      // double-bit ECC
	FaultXID13     = "xid13"      // application-level, warning only
	FaultOverheat  = "overheat"   // sustained high temperature
	FaultECCDBE    = "ecc-dbe"    // uncorrectable memory errors
	FaultRowRemap  = "row-remap"  // row remapping failure
	FaultSBEStorm  = "sbe-storm"  // fast-growing single-bit ECC count
	FaultNVLinkCRC = "nvlink-crc" // fast-growing NVLink CRC errors
	FaultDisappear = "disappear"  // GPU missing from the snapshot
)

// AllFaults lists every injectable fault, for validation and help text.
var AllFaults = []string{
	FaultNone, FaultXID79, FaultXID48, FaultXID13, FaultOverheat, FaultECCDBE,
	FaultRowRemap, FaultSBEStorm, FaultNVLinkCRC, FaultDisappear,
}

// SimSource fakes a node's GPUs so the whole system can run on a laptop or in
// CI without NVIDIA hardware. Faults are injected per GPU at runtime.
type SimSource struct {
	mu     sync.Mutex
	model  string
	gpus   int
	faults map[int]string
	sbe    map[int]uint64
	crc    map[int]uint64
	rng    *rand.Rand
	node   string
}

// NewSimSource simulates n healthy GPUs of the given model. The node name
// seeds the UUIDs so they are stable across restarts.
func NewSimSource(node, model string, n int, seed int64) *SimSource {
	return &SimSource{
		model: model, gpus: n, node: node,
		faults: map[int]string{}, sbe: map[int]uint64{}, crc: map[int]uint64{},
		rng: rand.New(rand.NewSource(seed)),
	}
}

// Inject sets (or with FaultNone, clears) a fault on one GPU.
func (s *SimSource) Inject(gpu int, fault string) error {
	fault = strings.ToLower(strings.TrimSpace(fault))
	if gpu < 0 || gpu >= s.gpus {
		return fmt.Errorf("gpu %d out of range [0,%d)", gpu, s.gpus)
	}
	valid := false
	for _, f := range AllFaults {
		if f == fault {
			valid = true
			break
		}
	}
	if !valid {
		return fmt.Errorf("unknown fault %q (valid: %s)", fault, strings.Join(AllFaults, ", "))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if fault == FaultNone {
		delete(s.faults, gpu)
	} else {
		s.faults[gpu] = fault
	}
	return nil
}

// Faults returns a copy of the active faults.
func (s *SimSource) Faults() map[int]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[int]string, len(s.faults))
	for k, v := range s.faults {
		out[k] = v
	}
	return out
}

// Sample returns the simulated readings, applying any injected faults.
func (s *SimSource) Sample(ctx context.Context) ([]GPUSample, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]GPUSample, 0, s.gpus)
	for i := 0; i < s.gpus; i++ {
		fault := s.faults[i]
		if fault == FaultDisappear {
			continue
		}
		g := GPUSample{
			Index:   i,
			UUID:    fmt.Sprintf("GPU-sim-%s-%d", s.node, i),
			Model:   s.model,
			TempC:   55 + s.rng.Float64()*10,
			PowerW:  250 + s.rng.Float64()*150,
			UtilPct: 60 + s.rng.Float64()*40,
		}
		switch fault {
		case FaultXID79:
			g.LastXID = 79
		case FaultXID48:
			g.LastXID = 48
			g.ECCDoubleBit = 1
		case FaultXID13:
			g.LastXID = 13
		case FaultOverheat:
			g.TempC = 93 + s.rng.Float64()*4
		case FaultECCDBE:
			g.ECCDoubleBit = 3
		case FaultRowRemap:
			g.RowRemapFailure = true
		case FaultSBEStorm:
			s.sbe[i] += 500
		case FaultNVLinkCRC:
			s.crc[i] += 500
		}
		g.ECCSingleBit = s.sbe[i]
		g.NVLinkCRCErrors = s.crc[i]
		out = append(out, g)
	}
	return out, nil
}
