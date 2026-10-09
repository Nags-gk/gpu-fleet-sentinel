// Package agent runs on every GPU node. Each interval it samples GPU telemetry,
// evaluates the health rules, and publishes the result as the GPUHealthy node
// condition plus Prometheus metrics.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
	// Patches counts condition writes; with Lease heartbeats it should stay
	// near zero on a steady node.
	Patches     prometheus.Counter
	LeaseErrors prometheus.Counter
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
	m.Patches = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "sentinel_condition_patches_total", Help: "Node condition writes issued.",
	})
	m.LeaseErrors = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "sentinel_lease_renew_errors_total", Help: "Failed heartbeat Lease renewals.",
	})
	reg.MustRegister(m.Temperature, m.Power, m.GPUStatus, m.NodeHealthy, m.Findings,
		m.SampleLatency, m.SampleErrors, m.PatchErrors, m.Patches, m.LeaseErrors)
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

	// LeaseNamespace enables Lease heartbeats: the agent renews a Lease there
	// every tick and rewrites the node condition only when it changes (or every
	// ConditionResync). Empty keeps the legacy behavior of patching the
	// condition, with a fresh heartbeat timestamp, on every tick.
	LeaseNamespace string
	// ConditionResync bounds how stale the condition may get while nothing
	// changes, which also repairs a condition deleted out from under the agent.
	// Only used with LeaseNamespace.
	ConditionResync time.Duration

	published  publishedState
	seeded     bool
	mu         sync.RWMutex
	lastReport health.Report
	lastErr    error
}

type publishedState struct {
	ok          bool
	status      corev1.ConditionStatus
	reason, msg string
	at          time.Time
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
		return a.report(ctx, corev1.ConditionUnknown, api.ReasonScrapeFailed, err.Error(), now)
	}

	report := a.Evaluator.Evaluate(health.Snapshot{Node: a.NodeName, Time: now, Samples: samples})
	if !a.seeded {
		a.seedDebouncer(ctx)
	}
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
	return a.report(ctx, status, reason, report.Summary(), now)
}

// report renews the heartbeat Lease and writes the condition when needed.
func (a *Agent) report(ctx context.Context, status corev1.ConditionStatus, reason, msg string, now time.Time) error {
	leaseOK := true
	if a.LeaseNamespace != "" {
		if err := a.renewLease(ctx, now); err != nil {
			a.Metrics.LeaseErrors.Inc()
			a.Log.Error(err, "lease heartbeat failed; falling back to the condition heartbeat")
			leaseOK = false
		}
	}
	p := a.published
	changed := !p.ok || p.status != status || p.reason != reason || p.msg != msg
	due := a.LeaseNamespace == "" || !leaseOK || a.ConditionResync <= 0 || now.Sub(p.at) >= a.ConditionResync
	if !changed && !due {
		return nil
	}
	if err := a.publish(ctx, status, reason, msg, now); err != nil {
		return err
	}
	a.published = publishedState{ok: true, status: status, reason: reason, msg: msg, at: now}
	return nil
}

// renewLease bumps spec.renewTime with a merge patch (no read), creating the
// Lease on first use. The Lease is owned by the Node so it is garbage collected
// with it.
func (a *Agent) renewLease(ctx context.Context, now time.Time) error {
	lease := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: api.LeaseName(a.NodeName), Namespace: a.LeaseNamespace}}
	renew := metav1.NewMicroTime(now)
	body, err := json.Marshal(map[string]any{"spec": map[string]any{"renewTime": renew}})
	if err != nil {
		return err
	}
	err = a.Client.Patch(ctx, lease, client.RawPatch(types.MergePatchType, body))
	if !apierrors.IsNotFound(err) {
		return err
	}
	var node corev1.Node
	if err := a.Client.Get(ctx, types.NamespacedName{Name: a.NodeName}, &node); err != nil {
		return fmt.Errorf("get node for lease owner: %w", err)
	}
	dur := int32(max(3*a.Interval, 15*time.Second) / time.Second)
	lease.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: "v1", Kind: "Node", Name: node.Name, UID: node.UID,
	}}
	lease.Spec = coordinationv1.LeaseSpec{HolderIdentity: &a.NodeName, LeaseDurationSeconds: &dur, RenewTime: &renew}
	if err := a.Client.Create(ctx, lease); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

// seedDebouncer resumes from the condition already on the node. Without it a
// restarted agent on a faulty node would publish True until FailAfter samples
// passed, resetting the transition time and the controller's grace period.
func (a *Agent) seedDebouncer(ctx context.Context) {
	var node corev1.Node
	if err := a.Client.Get(ctx, types.NamespacedName{Name: a.NodeName}, &node); err != nil {
		return // try again next tick
	}
	a.seeded = true
	for _, c := range node.Status.Conditions {
		if c.Type == api.ConditionGPUHealthy && c.Status == corev1.ConditionFalse {
			a.Debouncer.Seed(false)
		}
	}
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

	a.Metrics.Patches.Inc()
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
