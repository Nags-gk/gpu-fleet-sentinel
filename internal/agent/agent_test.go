package agent

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/Nags-gk/gpu-fleet-sentinel/internal/api"
	"github.com/Nags-gk/gpu-fleet-sentinel/internal/health"
)

type errSource struct{}

func (errSource) Sample(context.Context) ([]health.GPUSample, error) {
	return nil, context.DeadlineExceeded
}

func newAgent(t *testing.T, src health.Source, now *time.Time) (*Agent, client.Client, *prometheus.Registry) {
	t.Helper()
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "gpu-node-1"},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{
			{Type: corev1.NodeReady, Status: corev1.ConditionTrue, Reason: "KubeletReady"},
		}},
	}
	c := fake.NewClientBuilder().WithObjects(node).WithStatusSubresource(node).Build()
	reg := prometheus.NewRegistry()
	a := &Agent{
		NodeName:  "gpu-node-1",
		Source:    src,
		Evaluator: health.NewEvaluator(health.Thresholds{ExpectedGPUs: 4}),
		Debouncer: &health.Debouncer{FailAfter: 2, RecoverAfter: 2},
		Client:    c,
		Metrics:   NewMetrics(reg),
		Interval:  time.Second,
		Log:       logr.Discard(),
		Now:       func() time.Time { return *now },
	}
	return a, c, reg
}

func gpuCondition(t *testing.T, c client.Client) (corev1.NodeCondition, []corev1.NodeCondition) {
	t.Helper()
	var n corev1.Node
	if err := c.Get(context.Background(), types.NamespacedName{Name: "gpu-node-1"}, &n); err != nil {
		t.Fatal(err)
	}
	for _, cond := range n.Status.Conditions {
		if cond.Type == api.ConditionGPUHealthy {
			return cond, n.Status.Conditions
		}
	}
	t.Fatalf("GPUHealthy condition missing: %+v", n.Status.Conditions)
	return corev1.NodeCondition{}, nil
}

func TestTickLifecycle(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	sim := health.NewSimSource("gpu-node-1", "H100", 4, 1)
	a, c, reg := newAgent(t, sim, &now)

	if err := a.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	cond, all := gpuCondition(t, c)
	if cond.Status != corev1.ConditionTrue || cond.Reason != api.ReasonHealthy {
		t.Fatalf("want healthy, got %+v", cond)
	}
	if len(all) != 2 || all[0].Type != corev1.NodeReady {
		t.Fatalf("kubelet conditions must be preserved: %+v", all)
	}
	firstTransition := cond.LastTransitionTime

	// One critical sample is debounced; the condition stays True.
	_ = sim.Inject(2, health.FaultXID79)
	now = now.Add(time.Second)
	_ = a.Tick(ctx)
	cond, _ = gpuCondition(t, c)
	if cond.Status != corev1.ConditionTrue {
		t.Fatalf("single bad sample must not flip condition: %+v", cond)
	}
	if !cond.LastTransitionTime.Equal(&firstTransition) || !cond.LastHeartbeatTime.Time.Equal(now) {
		t.Fatalf("heartbeat should advance without a transition: %+v", cond)
	}

	// Second critical sample flips it.
	now = now.Add(time.Second)
	_ = a.Tick(ctx)
	cond, _ = gpuCondition(t, c)
	if cond.Status != corev1.ConditionFalse || cond.Reason != api.ReasonFault || !strings.Contains(cond.Message, "XID 79") {
		t.Fatalf("want unhealthy with XID 79, got %+v", cond)
	}
	if !cond.LastTransitionTime.Time.Equal(now) {
		t.Fatalf("transition time should be now, got %v", cond.LastTransitionTime)
	}
	if got := testutil.ToFloat64(a.Metrics.NodeHealthy); got != 0 {
		t.Fatalf("node healthy gauge = %v, want 0", got)
	}
	if got := testutil.ToFloat64(a.Metrics.GPUStatus.WithLabelValues("2", "GPU-sim-gpu-node-1-2")); got != 2 {
		t.Fatalf("gpu2 status gauge = %v, want 2", got)
	}

	// Clear and recover after two good samples.
	_ = sim.Inject(2, health.FaultNone)
	for i := 0; i < 2; i++ {
		now = now.Add(time.Second)
		_ = a.Tick(ctx)
	}
	cond, _ = gpuCondition(t, c)
	if cond.Status != corev1.ConditionTrue {
		t.Fatalf("want recovered, got %+v", cond)
	}
	if n, _ := testutil.GatherAndCount(reg, "sentinel_findings_total"); n == 0 {
		t.Fatal("findings counter should have series")
	}
}

func TestScrapeFailureIsUnknownNotUnhealthy(t *testing.T) {
	now := time.Now()
	a, c, _ := newAgent(t, errSource{}, &now)
	if err := a.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	cond, _ := gpuCondition(t, c)
	if cond.Status != corev1.ConditionUnknown || cond.Reason != api.ReasonScrapeFailed {
		t.Fatalf("want Unknown/%s, got %+v", api.ReasonScrapeFailed, cond)
	}
	if got := testutil.ToFloat64(a.Metrics.SampleErrors); got != 1 {
		t.Fatalf("sample errors = %v", got)
	}
}

func TestDebugHandlers(t *testing.T) {
	now := time.Now()
	sim := health.NewSimSource("gpu-node-1", "H100", 4, 1)
	a, _, reg := newAgent(t, sim, &now)
	_ = a.Tick(context.Background())
	srv := httptest.NewServer(Handler(a, reg, sim))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/debug/inject", "application/json", strings.NewReader(`{"gpu":1,"fault":"overheat"}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("inject: %v", resp.Status)
	}
	if sim.Faults()[1] != health.FaultOverheat {
		t.Fatal("fault not applied")
	}
	resp, err = http.Post(srv.URL+"/debug/inject", "application/json", strings.NewReader(`{"gpu":1,"fault":"nope"}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad fault should 400, got %s", resp.Status)
	}
	for _, path := range []string{"/metrics", "/healthz", "/debug/report", "/debug/faults"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: %v", path, resp.Status)
		}
	}
}

// A restarted agent on a node that is already unhealthy must not flip the
// condition back to True (which would reset the controller's grace timer).
func TestRestartKeepsUnhealthyCondition(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	sim := health.NewSimSource("gpu-node-1", "H100", 4, 1)
	_ = sim.Inject(0, health.FaultXID79)
	a, c, _ := newAgent(t, sim, &now)

	var node corev1.Node
	if err := c.Get(ctx, types.NamespacedName{Name: "gpu-node-1"}, &node); err != nil {
		t.Fatal(err)
	}
	since := metav1.NewTime(now.Add(-time.Minute))
	node.Status.Conditions = append(node.Status.Conditions, corev1.NodeCondition{
		Type: api.ConditionGPUHealthy, Status: corev1.ConditionFalse, Reason: api.ReasonFault,
		LastTransitionTime: since,
	})
	if err := c.Status().Update(ctx, &node); err != nil {
		t.Fatal(err)
	}

	if err := a.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	cond, _ := gpuCondition(t, c)
	if cond.Status != corev1.ConditionFalse {
		t.Fatalf("restart flipped condition to %v", cond.Status)
	}
	if !cond.LastTransitionTime.Equal(&since) {
		t.Fatalf("transition time reset to %v, want %v", cond.LastTransitionTime, since)
	}
}

func leaseAgent(t *testing.T, now *time.Time) (*Agent, client.Client) {
	t.Helper()
	sim := health.NewSimSource("gpu-node-1", "H100", 4, 1)
	a, c, _ := newAgent(t, sim, now)
	a.LeaseNamespace = "gpu-sentinel"
	a.ConditionResync = 5 * time.Minute
	return a, c
}

func getLease(t *testing.T, c client.Client) *coordinationv1.Lease {
	t.Helper()
	var l coordinationv1.Lease
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "gpu-sentinel", Name: api.LeaseName("gpu-node-1")}, &l); err != nil {
		t.Fatal(err)
	}
	return &l
}

func TestLeaseHeartbeatAvoidsRedundantConditionPatches(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	a, c := leaseAgent(t, &now)

	for i := 0; i < 10; i++ {
		if err := a.Tick(ctx); err != nil {
			t.Fatal(err)
		}
		now = now.Add(15 * time.Second)
	}
	if got := testutil.ToFloat64(a.Metrics.Patches); got != 1 {
		t.Fatalf("steady node: %v condition patches in 10 ticks, want 1", got)
	}
	l := getLease(t, c)
	if l.Spec.RenewTime == nil || !l.Spec.RenewTime.Time.Equal(now.Add(-15*time.Second)) {
		t.Fatalf("lease not renewed on every tick: %+v", l.Spec)
	}
	if len(l.OwnerReferences) != 1 || l.OwnerReferences[0].Kind != "Node" {
		t.Fatalf("lease should be owned by the node: %+v", l.OwnerReferences)
	}

	// The periodic resync still rewrites the condition.
	now = now.Add(5 * time.Minute)
	_ = a.Tick(ctx)
	if got := testutil.ToFloat64(a.Metrics.Patches); got != 2 {
		t.Fatalf("resync: %v patches, want 2", got)
	}
}

func TestLeaseModeStillPublishesTransitionsImmediately(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	sim := health.NewSimSource("gpu-node-1", "H100", 4, 1)
	a, c, _ := newAgent(t, sim, &now)
	a.LeaseNamespace, a.ConditionResync = "gpu-sentinel", time.Hour

	_ = a.Tick(ctx)
	_ = sim.Inject(1, health.FaultXID79)
	for i := 0; i < 2; i++ { // FailAfter is 2
		now = now.Add(time.Second)
		_ = a.Tick(ctx)
	}
	cond, _ := gpuCondition(t, c)
	if cond.Status != corev1.ConditionFalse || !strings.Contains(cond.Message, "XID 79") {
		t.Fatalf("fault must be published without waiting for resync: %+v", cond)
	}
}

// If the Lease cannot be renewed, the condition heartbeat must take over so the
// controller does not mistake a working agent for a dead one.
func TestLeaseFailureFallsBackToConditionHeartbeat(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	a, c := leaseAgent(t, &now)
	a.Client = interceptor.NewClient(c.(client.WithWatch), interceptor.Funcs{
		Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, p client.Patch, o ...client.PatchOption) error {
			if _, ok := obj.(*coordinationv1.Lease); ok {
				return errors.New("forbidden")
			}
			return cl.Patch(ctx, obj, p, o...)
		},
	})
	for i := 0; i < 3; i++ {
		_ = a.Tick(ctx)
		now = now.Add(15 * time.Second)
	}
	if got := testutil.ToFloat64(a.Metrics.Patches); got != 3 {
		t.Fatalf("with a broken lease the condition must carry the heartbeat: %v patches, want 3", got)
	}
	if got := testutil.ToFloat64(a.Metrics.LeaseErrors); got != 3 {
		t.Fatalf("lease errors = %v, want 3", got)
	}
}
