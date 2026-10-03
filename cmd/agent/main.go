// Command agent runs as a DaemonSet on every GPU node and publishes the
// GPUHealthy node condition.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"hash/fnv"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/Nags-gk/gpu-fleet-sentinel/internal/agent"
	"github.com/Nags-gk/gpu-fleet-sentinel/internal/health"
)

func main() {
	var (
		nodeName     = flag.String("node-name", os.Getenv("NODE_NAME"), "node this agent runs on (defaults to $NODE_NAME)")
		source       = flag.String("source", "dcgm", "telemetry source: dcgm or sim")
		dcgmURL      = flag.String("dcgm-url", "http://localhost:9400/metrics", "dcgm-exporter metrics URL")
		simGPUs      = flag.Int("sim-gpus", 8, "GPUs to simulate when --source=sim")
		simModel     = flag.String("sim-model", "NVIDIA H100 80GB HBM3", "GPU model to simulate")
		interval     = flag.Duration("interval", 15*time.Second, "sampling interval")
		failAfter    = flag.Int("fail-after", 2, "consecutive critical samples before reporting unhealthy")
		recoverAfter = flag.Int("recover-after", 4, "consecutive good samples before reporting healthy")
		expectedGPUs = flag.Int("expected-gpus", 0, "GPUs the node must report (0 = don't check)")
		tempWarn     = flag.Float64("temp-warn", 83, "warning temperature (C)")
		tempCrit     = flag.Float64("temp-critical", 90, "critical temperature (C)")
		listen       = flag.String("listen", ":9500", "address for /metrics, /healthz and /debug endpoints")
	)
	opts := zap.Options{}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	log := ctrl.Log.WithName("agent")

	if err := run(log, *nodeName, *source, *dcgmURL, *simGPUs, *simModel, *interval, *failAfter,
		*recoverAfter, *expectedGPUs, *tempWarn, *tempCrit, *listen); err != nil {
		log.Error(err, "agent exited")
		os.Exit(1)
	}
}

func run(log logr.Logger, nodeName, source, dcgmURL string, simGPUs int, simModel string, interval time.Duration,
	failAfter, recoverAfter, expectedGPUs int, tempWarn, tempCrit float64, listen string) error {

	if nodeName == "" {
		return errors.New("--node-name or $NODE_NAME is required")
	}

	var (
		src health.Source
		sim *health.SimSource
	)
	switch source {
	case "dcgm":
		src = health.NewDCGMSource(dcgmURL)
	case "sim":
		h := fnv.New64a()
		_, _ = h.Write([]byte(nodeName))
		sim = health.NewSimSource(nodeName, simModel, simGPUs, int64(h.Sum64()))
		src = sim
		if expectedGPUs == 0 {
			expectedGPUs = simGPUs
		}
	default:
		return fmt.Errorf("unknown --source %q", source)
	}

	cfg, err := ctrl.GetConfig()
	if err != nil {
		return fmt.Errorf("kubeconfig: %w", err)
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		return fmt.Errorf("kube client: %w", err)
	}

	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))

	a := &agent.Agent{
		NodeName: nodeName,
		Source:   src,
		Evaluator: health.NewEvaluator(health.Thresholds{
			TempWarnC: tempWarn, TempCriticalC: tempCrit, ExpectedGPUs: expectedGPUs,
		}),
		Debouncer: &health.Debouncer{FailAfter: failAfter, RecoverAfter: recoverAfter},
		Client:    c,
		Metrics:   agent.NewMetrics(reg),
		Interval:  interval,
		Log:       log,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	srv := &http.Server{Addr: listen, Handler: agent.Handler(a, reg, sim), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error(err, "http server failed")
			stop()
		}
	}()
	log.Info("agent started", "node", nodeName, "source", source, "interval", interval.String(), "listen", listen)

	err = a.Run(ctx)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	return err
}
