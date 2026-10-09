package health

import (
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
)

// Source produces the current GPU readings for this node.
type Source interface {
	Sample(ctx context.Context) ([]GPUSample, error)
}

// DCGMSource scrapes NVIDIA dcgm-exporter's Prometheus endpoint, which runs as
// a DaemonSet on GPU nodes (default http://<node>:9400/metrics).
type DCGMSource struct {
	URL    string
	Client *http.Client
}

// NewDCGMSource returns a source with a sane request timeout.
func NewDCGMSource(url string) *DCGMSource {
	return &DCGMSource{URL: url, Client: &http.Client{Timeout: 5 * time.Second}}
}

// Sample scrapes the exporter once and parses every GPU it reports.
func (d *DCGMSource) Sample(ctx context.Context) ([]GPUSample, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.URL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := d.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("scrape dcgm-exporter: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("scrape dcgm-exporter: HTTP %d", resp.StatusCode)
	}
	return ParseDCGM(resp.Body)
}

// ParseDCGM converts dcgm-exporter text exposition into per-GPU samples.
// Unknown metrics are ignored, so exporter upgrades that add fields are safe.
func ParseDCGM(r io.Reader) ([]GPUSample, error) {
	parser := expfmt.TextParser{}
	families, err := parser.TextToMetricFamilies(r)
	if err != nil {
		return nil, fmt.Errorf("parse dcgm metrics: %w", err)
	}

	byGPU := map[int]*GPUSample{}
	get := func(m *dto.Metric) (*GPUSample, bool) {
		var idx = -1
		var uuid, modelName string
		for _, l := range m.GetLabel() {
			switch l.GetName() {
			case "gpu":
				if v, err := strconv.Atoi(l.GetValue()); err == nil {
					idx = v
				}
			case "UUID":
				uuid = l.GetValue()
			case "modelName":
				modelName = l.GetValue()
			}
		}
		if idx < 0 {
			return nil, false
		}
		s, ok := byGPU[idx]
		if !ok {
			s = &GPUSample{Index: idx}
			byGPU[idx] = s
		}
		if uuid != "" {
			s.UUID = uuid
		}
		if modelName != "" {
			s.Model = modelName
		}
		return s, true
	}

	for name, fam := range families {
		for _, m := range fam.GetMetric() {
			v := value(m)
			s, ok := get(m)
			if !ok {
				continue
			}
			if err := checkValue(name, v); err != nil {
				return nil, err
			}
			switch name {
			case "DCGM_FI_DEV_GPU_TEMP":
				s.TempC = v
			case "DCGM_FI_DEV_POWER_USAGE":
				s.PowerW = v
			case "DCGM_FI_DEV_GPU_UTIL":
				s.UtilPct = v
			case "DCGM_FI_DEV_ECC_SBE_VOL_TOTAL":
				s.ECCSingleBit = uint64(v)
			case "DCGM_FI_DEV_ECC_DBE_VOL_TOTAL":
				s.ECCDoubleBit = uint64(v)
			case "DCGM_FI_DEV_ROW_REMAP_FAILURE":
				s.RowRemapFailure = v > 0
			case "DCGM_FI_DEV_NVLINK_CRC_FLIT_ERROR_COUNT_TOTAL":
				s.NVLinkCRCErrors = uint64(v)
			case "DCGM_FI_DEV_XID_ERRORS":
				s.LastXID = int(v)
			}
		}
	}

	out := make([]GPUSample, 0, len(byGPU))
	for _, s := range byGPU {
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Index < out[j].Index })
	return out, nil
}

// checkValue rejects readings the rules cannot reason about. NaN compares false
// against every threshold, so a NaN temperature would read as healthy, and
// converting a negative or huge float to an unsigned counter is
// implementation-defined in Go (amd64 and arm64 disagree). Failing the scrape
// reports the node as Unknown, which never triggers remediation.
func checkValue(name string, v float64) error {
	switch name {
	case "DCGM_FI_DEV_GPU_TEMP", "DCGM_FI_DEV_POWER_USAGE", "DCGM_FI_DEV_GPU_UTIL",
		"DCGM_FI_DEV_ECC_SBE_VOL_TOTAL", "DCGM_FI_DEV_ECC_DBE_VOL_TOTAL",
		"DCGM_FI_DEV_ROW_REMAP_FAILURE", "DCGM_FI_DEV_NVLINK_CRC_FLIT_ERROR_COUNT_TOTAL",
		"DCGM_FI_DEV_XID_ERRORS":
	default:
		return nil
	}
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return fmt.Errorf("parse dcgm metrics: %s is not a finite number (%v)", name, v)
	}
	switch name {
	case "DCGM_FI_DEV_ECC_SBE_VOL_TOTAL", "DCGM_FI_DEV_ECC_DBE_VOL_TOTAL", "DCGM_FI_DEV_NVLINK_CRC_FLIT_ERROR_COUNT_TOTAL":
		if v < 0 || v >= 1<<63 {
			return fmt.Errorf("parse dcgm metrics: counter %s out of range (%v)", name, v)
		}
	case "DCGM_FI_DEV_XID_ERRORS":
		if v < 0 || v > 65535 {
			return fmt.Errorf("parse dcgm metrics: %s is not a valid XID (%v)", name, v)
		}
	}
	return nil
}

func value(m *dto.Metric) float64 {
	switch {
	case m.Gauge != nil:
		return m.GetGauge().GetValue()
	case m.Counter != nil:
		return m.GetCounter().GetValue()
	case m.Untyped != nil:
		return m.GetUntyped().GetValue()
	}
	return 0
}
