package agent

import (
	"encoding/json"
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/Nags-gk/gpu-fleet-sentinel/internal/health"
)

// Handler serves /metrics, /healthz, /debug/report and, when sim is non-nil,
// the fault-injection endpoints used by the e2e test and the demo.
func Handler(a *Agent, gatherer prometheus.Gatherer, sim *health.SimSource) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(gatherer, promhttp.HandlerOpts{}))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /debug/report", func(w http.ResponseWriter, _ *http.Request) {
		r, err := a.LastReport()
		out := map[string]any{"node": r.Node, "time": r.Time, "status": r.Status().String(),
			"gpus": r.GPUCount, "summary": r.Summary(), "findings": r.Findings}
		if err != nil {
			out["error"] = err.Error()
		}
		writeJSON(w, http.StatusOK, out)
	})

	if sim != nil {
		mux.HandleFunc("GET /debug/faults", func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, sim.Faults())
		})
		mux.HandleFunc("POST /debug/inject", func(w http.ResponseWriter, r *http.Request) {
			var req struct {
				GPU   int    `json:"gpu"`
				Fault string `json:"fault"`
			}
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&req); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
			if err := sim.Inject(req.GPU, req.Fault); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
			writeJSON(w, http.StatusOK, sim.Faults())
		})
	}
	return mux
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
