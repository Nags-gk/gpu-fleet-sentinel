// Package agent runs on every GPU node. Each interval it samples GPU telemetry,
// evaluates the health rules, and publishes the result as the GPUHealthy node
// condition plus Prometheus metrics.
package agent

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Nags-gk/gpu-fleet-sentinel/internal/api"
	"github.com/Nags-gk/gpu-fleet-sentinel/internal/health"
)

// Metrics are the agent's Prometheus series.
type Metrics struct {
	Temperature   *prometheus.GaugeVec
	Power         *prometheus.GaugeVec
	GPUStatus     *prometheus.GaugeVec
	NodeHealthy   prometheus.Gauge
	Findings      *prometheus.CounterVec
	SampleLatency prometheus.Histogram
	SampleErrors  prometheus.Counter
	PatchErrors   prometheus.Counter
}

// NewMetrics registers the agent metrics on reg.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		Temperature: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "sentinel_gpu_temperature_celsius", Help: "GPU temperature.",
		}, []string{"gpu", "uuid"}),
		Power: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "sentinel_gpu_power_watts", Help: "GPU power draw.",
		}, []string{"gpu", "uuid"}),
		GPUStatus: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "sentinel_gpu_health_status", Help: "Per-GPU health: 0 ok, 1 warning, 2 critical.",
		}, []string{"gpu", "uuid"}),
		NodeHealthy: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "sentinel_node_gpu_healthy", Help: "Debounced node GPU health: 1 healthy, 0 unhealthy.",
		}),
		Findings: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sentinel_findings_total", Help: "Health rule violations observed.",
		}, []string{"rule", "severity"}),
		SampleLatency: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "sentinel_sample_duration_seconds", Help: "Time to collect one telemetry sample.",
			Buckets: prometheus.ExponentialBuckets(0.001, 2, 12),
		}),
		SampleErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "sentinel_sample_errors_total", Help: "Failed telemetry collections.",
		}),
		PatchErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "sentinel_condition_patch_errors_total", Help: "Failed node condition updates.",
		}),
	}
	reg.MustRegister(m.Temperature, m.Power, m.GPUStatus, m.NodeHealthy, m.Findings,
		m.SampleLatency, m.SampleErrors, m.PatchErrors)
	return m
}

// Agent ties a telemetry source to the node condition.
type Agent struct {
	NodeName  string
	Source    health.Source
	Evaluator *health.Evaluator
	Debouncer *health.Debouncer
	Client    client.Client
	Metrics   *Metrics
	Interval  time.Duration
	Log       logr.Logger
	Now       func() time.Time

	mu         sync.RWMutex
	lastReport health.Report
	lastErr    error
}

// LastReport returns the most recent evaluation, for the /debug/report endpoint.
func (a *Agent) LastReport() (health.Report, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.lastReport, a.lastErr
}

// Run samples until ctx is cancelled.
func (a *Agent) Run(ctx context.Context) error {
	if a.Now == nil {
		a.Now = time.Now
	}
	t := time.NewTicker(a.Interval)
	defer t.Stop()
	for {
		if err := a.Tick(ctx); err != nil {
			a.Log.Error(err, "tick failed")
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// Tick performs one sample → evaluate → publish cycle.
func (a *Agent) Tick(ctx context.Context) error {
	if a.Now == nil {
		a.Now = time.Now
	}
	now := a.Now()

	start := time.Now()
	sampleCtx, cancel := context.WithTimeout(ctx, max(a.Interval, time.Second))
	samples, err := a.Source.Sample(sampleCtx)
	cancel()
	a.Metrics.SampleLatency.Observe(time.Since(start).Seconds())

	if err != nil {
		a.Metrics.SampleErrors.Inc()
		a.mu.Lock()
		a.lastErr = err
		a.mu.Unlock()
		// Telemetry loss is reported as Unknown, never as unhealthy: the
		// controller must not drain a node just because an exporter crashed.
		return a.publish(ctx, corev1.ConditionUnknown, api.ReasonScrapeFailed, err.Error(), now)
	}

	report := a.Evaluator.Evaluate(health.Snapshot{Node: a.NodeName, Time: now, Samples: samples})
	healthy := a.Debouncer.Observe(report.Status())

	a.Metrics.Temperature.Reset()
	a.Metrics.Power.Reset()
	a.Metrics.GPUStatus.Reset()
	for _, s := range samples {
		idx := strconv.Itoa(s.Index)
		a.Metrics.Temperature.WithLabelValues(idx, s.UUID).Set(s.TempC)
		a.Metrics.Power.WithLabelValues(idx, s.UUID).Set(s.PowerW)
		a.Metrics.GPUStatus.WithLabelValues(idx, s.UUID).Set(float64(report.GPUStatus(s.Index)))
	}
	for _, f := range report.Findings {
		a.Metrics.Findings.WithLabelValues(f.Rule, f.Severity.String()).Inc()
	}

	a.mu.Lock()
	a.lastReport, a.lastErr = report, nil
	a.mu.Unlock()

	status, reason := corev1.ConditionTrue, api.ReasonHealthy
	if healthy {
		a.Metrics.NodeHealthy.Set(1)
	} else {
		a.Metrics.NodeHealthy.Set(0)
		status, reason = corev1.ConditionFalse, api.ReasonFault
	}
	return a.publish(ctx, status, reason, report.Summary(), now)
}

// publish writes the GPUHealthy condition with a strategic merge patch on the
// node status subresource. Strategic merge keys conditions by "type", so this
// cannot clobber the kubelet's own conditions (Ready, MemoryPressure, ...).
func (a *Agent) publish(ctx context.Context, status corev1.ConditionStatus, reason, msg string, now time.Time) error {
	var node corev1.Node
	if err := a.Client.Get(ctx, types.NamespacedName{Name: a.NodeName}, &node); err != nil {
		a.Metrics.PatchErrors.Inc()
		return fmt.Errorf("get node %s: %w", a.NodeName, err)
	}
	orig := node.DeepCopy()

	cond := corev1.NodeCondition{
		Type:              api.ConditionGPUHealthy,
		Status:            status,
		Reason:            reason,
		Message:           truncate(msg, 1024),
		LastHeartbeatTime: metav1.NewTime(now),
	}
	found := false
	for i, c := range node.Status.Conditions {
		if c.Type != api.ConditionGPUHealthy {
			continue
		}
		found = true
		cond.LastTransitionTime = c.LastTransitionTime
		if c.Status != status {
			cond.LastTransitionTime = metav1.NewTime(now)
			a.Log.Info("GPU health transition", "node", a.NodeName, "from", c.Status, "to", status, "message", msg)
		}
		node.Status.Conditions[i] = cond
	}
	if !found {
		cond.LastTransitionTime = metav1.NewTime(now)
		node.Status.Conditions = append(node.Status.Conditions, cond)
	}

	if err := a.Client.Status().Patch(ctx, &node, client.StrategicMergeFrom(orig)); err != nil {
		a.Metrics.PatchErrors.Inc()
		return fmt.Errorf("patch node %s status: %w", a.NodeName, err)
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-3] + "..."
}
