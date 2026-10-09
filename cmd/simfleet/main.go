// Command simfleet is the scale-test driver. It registers N kwok-managed GPU
// nodes, runs one real agent per node (simulated GPUs, the production Agent
// code path), optionally injects faults, and reports how much load the fleet
// puts on the kube-apiserver and how the controller behaves under it.
//
//	hack/scale.sh        # orchestrates apiserver, kwok, controller and this tool
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"hash/fnv"
	"math/rand"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/expfmt"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Nags-gk/gpu-fleet-sentinel/internal/agent"
	"github.com/Nags-gk/gpu-fleet-sentinel/internal/api"
	"github.com/Nags-gk/gpu-fleet-sentinel/internal/health"
)

type options struct {
	kubeconfig  string
	nodes       int
	mode        string
	interval    time.Duration
	steady      time.Duration
	faultNodes  int
	budget      int
	metricsURL  string
	out         string
	pids        string
	startupWait time.Duration
}

func main() {
	var o options
	flag.StringVar(&o.kubeconfig, "cluster", "scale.kubeconfig", "cluster to test against")
	flag.IntVar(&o.nodes, "nodes", 1000, "simulated GPU nodes")
	flag.StringVar(&o.mode, "mode", "lease", "heartbeat mode: lease (Lease + change-only condition patches) or legacy (patch the node every interval)")
	flag.DurationVar(&o.interval, "interval", 15*time.Second, "agent sampling interval")
	flag.DurationVar(&o.steady, "steady", 2*time.Minute, "length of the steady-state measurement window")
	flag.IntVar(&o.faultNodes, "fault-nodes", 0, "nodes to fail at once after the steady window (0 = skip the fault phase)")
	flag.IntVar(&o.budget, "budget", 0, "expected disruption budget, to assert it is never exceeded (0 = don't assert)")
	flag.StringVar(&o.metricsURL, "controller-metrics", "http://localhost:8080/metrics", "controller /metrics URL")
	flag.StringVar(&o.out, "out", "", "write the JSON report here (default stdout)")
	flag.StringVar(&o.pids, "watch-pids", "", "name=pid,... processes whose CPU and RSS to report")
	flag.DurationVar(&o.startupWait, "startup-timeout", 5*time.Minute, "how long to wait for all agents to publish")
	flag.Parse()

	rep, err := run(o)
	if rep != nil {
		b, _ := json.MarshalIndent(rep, "", "  ")
		if o.out != "" {
			_ = os.WriteFile(o.out, b, 0o644)
		} else {
			fmt.Println(string(b))
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "simfleet:", err)
		os.Exit(1)
	}
}

type report struct {
	Mode     string `json:"mode"`
	Nodes    int    `json:"nodes"`
	Interval string `json:"interval"`

	Steady struct {
		Seconds         float64            `json:"seconds"`
		RequestsPerSec  map[string]float64 `json:"requests_per_sec"`
		TotalPerSec     float64            `json:"total_requests_per_sec"`
		NodeWritesPerS  float64            `json:"node_writes_per_sec"`
		ProcessCPUCores map[string]float64 `json:"process_cpu_cores,omitempty"`
		ProcessRSSMiB   map[string]float64 `json:"process_rss_mib,omitempty"`
	} `json:"steady_state"`

	Fault *faultReport `json:"fault,omitempty"`

	Controller map[string]float64 `json:"controller_metrics,omitempty"`
}

type faultReport struct {
	Injected            int     `json:"injected"`
	Quarantined         int     `json:"quarantined"`
	Deferred            int     `json:"deferred_by_budget"`
	PeakQuarantined     int     `json:"peak_concurrent_quarantined"`
	Budget              int     `json:"budget,omitempty"`
	DetectP50           float64 `json:"inject_to_condition_false_p50_s"`
	DetectP95           float64 `json:"inject_to_condition_false_p95_s"`
	QuarantineP50       float64 `json:"condition_false_to_quarantine_p50_s"`
	QuarantineP95       float64 `json:"condition_false_to_quarantine_p95_s"`
	QuarantineMax       float64 `json:"condition_false_to_quarantine_max_s"`
	PodsEvicted         int     `json:"gpu_pods_evicted"`
	ReleaseAllSeconds   float64 `json:"clear_to_all_released_s"`
	BudgetNeverExceeded *bool   `json:"budget_never_exceeded,omitempty"`
}

func run(o options) (*report, error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg, err := clientcmd.BuildConfigFromFlags("", o.kubeconfig)
	if err != nil {
		return nil, err
	}
	cfg.QPS, cfg.Burst = -1, 2000 // no client-side throttling: measure the server, not the limiter
	observer, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		return nil, err
	}

	// Agents get a separate, counting client so only their traffic is measured.
	ctr := &counter{counts: map[string]*atomic.Int64{}}
	acfg := *cfg
	acfg.Wrap(ctr.wrap)
	agentClient, err := client.New(&acfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		return nil, err
	}

	names := make([]string, o.nodes)
	for i := range names {
		names[i] = fmt.Sprintf("kwok-gpu-%04d", i)
	}
	if err := setup(ctx, observer, names); err != nil {
		return nil, err
	}

	leaseMode := o.mode == "lease"
	sims := make(map[string]*health.SimSource, o.nodes)
	var wg sync.WaitGroup
	for _, n := range names {
		h := fnv.New64a()
		_, _ = h.Write([]byte(n))
		sim := health.NewSimSource(n, "NVIDIA H100 80GB HBM3", 8, int64(h.Sum64()))
		sims[n] = sim
		a := &agent.Agent{
			NodeName: n, Source: sim,
			Evaluator: health.NewEvaluator(health.Thresholds{ExpectedGPUs: 8}),
			Debouncer: &health.Debouncer{FailAfter: 2, RecoverAfter: 2},
			Client:    agentClient, Metrics: agent.NewMetrics(prometheus.NewRegistry()),
			Interval: o.interval, Log: logr.Discard(),
		}
		if leaseMode {
			a.LeaseNamespace, a.ConditionResync = "default", 5*time.Minute
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			// A DaemonSet's pods are not phase-aligned; spread the first tick.
			select {
			case <-time.After(time.Duration(rand.Int63n(int64(o.interval)))):
			case <-ctx.Done():
				return
			}
			_ = a.Run(ctx)
		}()
	}
	defer func() { cancel(); wg.Wait() }()

	fmt.Fprintf(os.Stderr, "waiting for %d agents to publish GPUHealthy=True ...\n", o.nodes)
	if err := waitFor(ctx, o.startupWait, time.Second, func() (bool, error) {
		nodes, err := listNodes(ctx, observer)
		if err != nil {
			return false, err
		}
		ok := 0
		for i := range nodes {
			if c := gpuCond(&nodes[i]); c != nil && c.Status == corev1.ConditionTrue {
				ok++
			}
		}
		return ok == o.nodes, nil
	}); err != nil {
		return nil, fmt.Errorf("agents never converged: %w", err)
	}
	// Let the first-tick burst drain and the controller settle.
	time.Sleep(2 * o.interval)

	rep := &report{Mode: o.mode, Nodes: o.nodes, Interval: o.interval.String()}

	fmt.Fprintf(os.Stderr, "steady-state window: %s\n", o.steady)
	before, procBefore := ctr.snapshot(), sampleProcs(o.pids)
	t0 := time.Now()
	time.Sleep(o.steady)
	elapsed := time.Since(t0).Seconds()
	after, procAfter := ctr.snapshot(), sampleProcs(o.pids)

	rep.Steady.Seconds = elapsed
	rep.Steady.RequestsPerSec = map[string]float64{}
	for k, v := range after {
		d := float64(v-before[k]) / elapsed
		rep.Steady.RequestsPerSec[k] = round(d)
		rep.Steady.TotalPerSec += d
		if strings.HasPrefix(k, "PATCH nodes") || strings.HasPrefix(k, "PUT nodes") {
			rep.Steady.NodeWritesPerS += d
		}
	}
	rep.Steady.TotalPerSec, rep.Steady.NodeWritesPerS = round(rep.Steady.TotalPerSec), round(rep.Steady.NodeWritesPerS)
	rep.Steady.ProcessCPUCores, rep.Steady.ProcessRSSMiB = map[string]float64{}, map[string]float64{}
	for name, a := range procAfter {
		if b, ok := procBefore[name]; ok {
			rep.Steady.ProcessCPUCores[name] = round((a.cpu - b.cpu) / elapsed)
		}
		rep.Steady.ProcessRSSMiB[name] = round(a.rssKiB / 1024)
	}

	if o.faultNodes > 0 {
		fr, err := faultPhase(ctx, o, observer, names, sims)
		rep.Fault = fr
		if err != nil {
			return rep, err
		}
	}
	rep.Controller = scrapeController(o.metricsURL)
	return rep, nil
}

func setup(ctx context.Context, c client.Client, names []string) error {
	for _, ns := range []string{"ml"} {
		if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
	}
	sem := make(chan struct{}, 32)
	var wg sync.WaitGroup
	var firstErr atomic.Value
	for _, n := range names {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
				Name:        n,
				Annotations: map[string]string{"kwok.x-k8s.io/node": "fake", "node.alpha.kubernetes.io/ttl": "0"},
				Labels: map[string]string{"type": "kwok", "kubernetes.io/hostname": n,
					"nvidia.com/gpu.present": "true", "nvidia.com/gpu.product": "NVIDIA-H100-80GB-HBM3"},
			}, Status: corev1.NodeStatus{
				Capacity: corev1.ResourceList{"cpu": resource.MustParse("64"), "memory": resource.MustParse("512Gi"),
					"pods": resource.MustParse("110"), api.GPUResource: resource.MustParse("8")},
				Allocatable: corev1.ResourceList{"cpu": resource.MustParse("64"), "memory": resource.MustParse("512Gi"),
					"pods": resource.MustParse("110"), api.GPUResource: resource.MustParse("8")},
			}}
			if err := c.Create(ctx, node); err != nil && !apierrors.IsAlreadyExists(err) {
				firstErr.CompareAndSwap(nil, err)
				return
			}
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "job-" + n, Namespace: "ml"},
				Spec: corev1.PodSpec{NodeName: n, Containers: []corev1.Container{{
					Name: "train", Image: "fake",
					Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{api.GPUResource: resource.MustParse("8")}},
				}}},
			}
			if err := c.Create(ctx, pod); err != nil && !apierrors.IsAlreadyExists(err) {
				firstErr.CompareAndSwap(nil, err)
			}
		}()
	}
	wg.Wait()
	if e, _ := firstErr.Load().(error); e != nil {
		return e
	}
	return nil
}

func faultPhase(ctx context.Context, o options, c client.Client, names []string, sims map[string]*health.SimSource) (*faultReport, error) {
	if o.faultNodes > len(names) {
		return nil, errors.New("--fault-nodes exceeds --nodes")
	}
	victims := append([]string(nil), names...)
	rand.Shuffle(len(victims), func(i, j int) { victims[i], victims[j] = victims[j], victims[i] })
	victims = victims[:o.faultNodes]
	isVictim := map[string]bool{}
	for _, v := range victims {
		isVictim[v] = true
	}

	fmt.Fprintf(os.Stderr, "injecting XID 79 on %d nodes\n", len(victims))
	injectAt := time.Now()
	for _, v := range victims {
		_ = sims[v].Inject(0, health.FaultXID79)
	}

	condFalseAt, quarantinedAt := map[string]time.Time{}, map[string]time.Time{}
	peak := 0
	var lastChange = time.Now()
	deadline := time.Now().Add(10 * time.Minute)
	for time.Now().Before(deadline) {
		nodes, err := listNodes(ctx, c)
		if err != nil {
			return nil, err
		}
		now := time.Now()
		q := 0
		for i := range nodes {
			n := &nodes[i]
			if _, ok := n.Annotations[api.AnnotationQuarantinedAt]; ok {
				q++
				if _, seen := quarantinedAt[n.Name]; !seen && isVictim[n.Name] {
					quarantinedAt[n.Name], lastChange = now, now
				}
			}
			if cnd := gpuCond(n); cnd != nil && cnd.Status == corev1.ConditionFalse && isVictim[n.Name] {
				if _, seen := condFalseAt[n.Name]; !seen {
					condFalseAt[n.Name] = cnd.LastTransitionTime.Time
				}
			}
		}
		peak = max(peak, q)
		// Done when the quarantine count has been stable for a full grace+resync cycle.
		if len(quarantinedAt) > 0 && now.Sub(lastChange) > 3*o.interval {
			break
		}
		time.Sleep(time.Second)
	}

	fr := &faultReport{Injected: len(victims), Quarantined: len(quarantinedAt), PeakQuarantined: peak, Budget: o.budget}
	fr.Deferred = fr.Injected - fr.Quarantined
	var det, qua []float64
	for n, qt := range quarantinedAt {
		cf := condFalseAt[n]
		det = append(det, cf.Sub(injectAt).Seconds())
		qua = append(qua, qt.Sub(cf).Seconds())
	}
	fr.DetectP50, fr.DetectP95 = round(pct(det, 50)), round(pct(det, 95))
	fr.QuarantineP50, fr.QuarantineP95, fr.QuarantineMax = round(pct(qua, 50)), round(pct(qua, 95)), round(pct(qua, 100))
	if o.budget > 0 {
		ok := peak <= o.budget
		fr.BudgetNeverExceeded = &ok
	}
	for n := range quarantinedAt {
		var p corev1.Pod
		err := c.Get(ctx, client.ObjectKey{Namespace: "ml", Name: "job-" + n}, &p)
		if apierrors.IsNotFound(err) || (err == nil && p.DeletionTimestamp != nil) {
			fr.PodsEvicted++
		}
	}

	fmt.Fprintln(os.Stderr, "clearing faults")
	clearAt := time.Now()
	for _, v := range victims {
		_ = sims[v].Inject(0, health.FaultNone)
	}
	if err := waitFor(ctx, 15*time.Minute, time.Second, func() (bool, error) {
		nodes, err := listNodes(ctx, c)
		if err != nil {
			return false, err
		}
		for i := range nodes {
			if _, ok := nodes[i].Annotations[api.AnnotationQuarantinedAt]; ok {
				return false, nil
			}
		}
		return true, nil
	}); err != nil {
		return fr, fmt.Errorf("nodes never released: %w", err)
	}
	fr.ReleaseAllSeconds = round(time.Since(clearAt).Seconds())
	return fr, nil
}

// --- request counting -------------------------------------------------------

type counter struct {
	mu     sync.Mutex
	counts map[string]*atomic.Int64
}

func (c *counter) wrap(rt http.RoundTripper) http.RoundTripper {
	return roundTripFunc(func(r *http.Request) (*http.Response, error) {
		k := r.Method + " " + classify(r.URL.Path)
		c.mu.Lock()
		v, ok := c.counts[k]
		if !ok {
			v = new(atomic.Int64)
			c.counts[k] = v
		}
		c.mu.Unlock()
		v.Add(1)
		return rt.RoundTrip(r)
	})
}

func (c *counter) snapshot() map[string]int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]int64, len(c.counts))
	for k, v := range c.counts {
		out[k] = v.Load()
	}
	return out
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// classify reduces /api/v1/nodes/<name>/status to "nodes/status" etc.
func classify(path string) string {
	segs := strings.Split(strings.Trim(path, "/"), "/")
	for i, s := range segs {
		switch s {
		case "nodes", "leases", "pods":
			if i+2 < len(segs) {
				return s + "/" + segs[i+2]
			}
			return s
		}
	}
	return "other"
}

// --- helpers ----------------------------------------------------------------

func listNodes(ctx context.Context, c client.Client) ([]corev1.Node, error) {
	var l corev1.NodeList
	if err := c.List(ctx, &l); err != nil {
		return nil, err
	}
	return l.Items, nil
}

func gpuCond(n *corev1.Node) *corev1.NodeCondition {
	for i := range n.Status.Conditions {
		if n.Status.Conditions[i].Type == api.ConditionGPUHealthy {
			return &n.Status.Conditions[i]
		}
	}
	return nil
}

func waitFor(ctx context.Context, timeout, every time.Duration, f func() (bool, error)) error {
	deadline := time.Now().Add(timeout)
	for {
		ok, err := f()
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("timed out")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(every):
		}
	}
}

func pct(xs []float64, p float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	i := int(float64(len(s)-1) * p / 100)
	return s[i]
}

func round(f float64) float64 { return float64(int64(f*100+0.5)) / 100 }

type procStat struct{ cpu, rssKiB float64 }

// sampleProcs reads cumulative CPU seconds and RSS from ps.
func sampleProcs(spec string) map[string]procStat {
	out := map[string]procStat{}
	for _, kv := range strings.Split(spec, ",") {
		name, pid, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		b, err := exec.Command("ps", "-o", "cputime=,rss=", "-p", pid).Output()
		if err != nil {
			continue
		}
		f := strings.Fields(string(b))
		if len(f) != 2 {
			continue
		}
		rss, _ := strconv.ParseFloat(f[1], 64)
		out[name] = procStat{cpu: parseCPU(f[0]), rssKiB: rss}
	}
	return out
}

// parseCPU turns ps's [[h:]m:]s.cc into seconds.
func parseCPU(s string) float64 {
	t := 0.0
	for _, part := range strings.Split(s, ":") {
		v, _ := strconv.ParseFloat(part, 64)
		t = t*60 + v
	}
	return t
}

// scrapeController pulls the series worth reporting from the controller.
func scrapeController(url string) map[string]float64 {
	resp, err := http.Get(url)
	if err != nil {
		return nil
	}
	defer func() { _ = resp.Body.Close() }()
	fams, err := (&expfmt.TextParser{}).TextToMetricFamilies(resp.Body)
	if err != nil {
		return nil
	}
	out := map[string]float64{}
	if f := fams["controller_runtime_reconcile_time_seconds"]; f != nil {
		for _, m := range f.Metric {
			h := m.GetHistogram()
			out["reconcile_count"] += float64(h.GetSampleCount())
			out["reconcile_seconds_sum"] += h.GetSampleSum()
		}
		if out["reconcile_count"] > 0 {
			out["reconcile_mean_ms"] = round(out["reconcile_seconds_sum"] / out["reconcile_count"] * 1000)
		}
		delete(out, "reconcile_seconds_sum")
	}
	if f := fams["sentinel_quarantined_nodes"]; f != nil && len(f.Metric) > 0 {
		out["quarantined_nodes_now"] = f.Metric[0].GetGauge().GetValue()
	}
	if f := fams["sentinel_remediation_decisions_total"]; f != nil {
		for _, m := range f.Metric {
			for _, l := range m.Label {
				if l.GetName() == "action" {
					out["decisions_"+strings.ToLower(l.GetValue())] = m.GetCounter().GetValue()
				}
			}
		}
	}
	if f := fams["process_resident_memory_bytes"]; f != nil && len(f.Metric) > 0 {
		out["controller_rss_mib"] = round(f.Metric[0].GetGauge().GetValue() / (1 << 20))
	}
	return out
}
