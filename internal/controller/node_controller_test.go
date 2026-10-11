package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/Nags-gk/gpu-fleet-sentinel/internal/api"
	"github.com/Nags-gk/gpu-fleet-sentinel/internal/incident"
	"github.com/Nags-gk/gpu-fleet-sentinel/internal/remediation"
)

var t0 = time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)

func gpuNode(name string, status corev1.ConditionStatus, since time.Time) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{
			"nvidia.com/gpu.present": "true", "nvidia.com/gpu.product": "NVIDIA-H100-80GB-HBM3",
		}},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{
			Type: api.ConditionGPUHealthy, Status: status, Message: "GPU2: XID 79 (GPU has fallen off the bus)",
			LastTransitionTime: metav1.NewTime(since), LastHeartbeatTime: metav1.NewTime(t0),
		}}},
	}
}

func pod(name, node string, gpus int64, owner string) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ml"},
		Spec:       corev1.PodSpec{NodeName: node, Containers: []corev1.Container{{Name: "c"}}},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	if gpus > 0 {
		p.Spec.Containers[0].Resources.Limits = corev1.ResourceList{api.GPUResource: *resource.NewQuantity(gpus, resource.DecimalSI)}
	}
	if owner != "" {
		p.OwnerReferences = []metav1.OwnerReference{{Kind: owner, Name: "x", APIVersion: "apps/v1", UID: "u"}}
	}
	return p
}

type harness struct {
	r   *NodeReconciler
	c   client.Client
	rec *record.FakeRecorder
	now *time.Time
}

func newHarness(t *testing.T, funcs *interceptor.Funcs, objs ...client.Object) *harness {
	t.Helper()
	b := fake.NewClientBuilder().WithObjects(objs...).
		WithIndex(&corev1.Pod{}, PodNodeNameIndex, func(o client.Object) []string {
			return []string{o.(*corev1.Pod).Spec.NodeName}
		})
	if funcs != nil {
		b = b.WithInterceptorFuncs(*funcs)
	}
	now := t0
	sel, _ := labels.Parse(api.DefaultGPUNodeSelector)
	rec := record.NewFakeRecorder(100)
	h := &harness{c: b.Build(), rec: rec, now: &now}
	h.r = &NodeReconciler{
		Client: h.c, Recorder: rec, Policy: remediation.DefaultPolicy(), Selector: sel,
		DrainScope: DrainGPUPods, Summarizer: incident.Template{},
		Now: func() time.Time { return *h.now },
	}
	return h
}

func (h *harness) reconcile(t *testing.T, name string) ctrl.Result {
	t.Helper()
	res, err := h.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: name}})
	if err != nil {
		t.Fatalf("reconcile %s: %v", name, err)
	}
	return res
}

func (h *harness) node(t *testing.T, name string) *corev1.Node {
	t.Helper()
	var n corev1.Node
	if err := h.c.Get(context.Background(), types.NamespacedName{Name: name}, &n); err != nil {
		t.Fatal(err)
	}
	return &n
}

func (h *harness) podExists(name string) bool {
	var p corev1.Pod
	err := h.c.Get(context.Background(), types.NamespacedName{Namespace: "ml", Name: name}, &p)
	return err == nil
}

func (h *harness) events() string {
	var out []string
	for {
		select {
		case e := <-h.rec.Events:
			out = append(out, e)
		default:
			return strings.Join(out, "\n")
		}
	}
}

func TestQuarantineDrainAndRelease(t *testing.T) {
	h := newHarness(t, nil,
		gpuNode("gpu-1", corev1.ConditionFalse, t0.Add(-30*time.Second)),
		gpuNode("gpu-2", corev1.ConditionTrue, t0.Add(-time.Hour)),
		pod("trainer", "gpu-1", 8, "Job"),
		pod("cpu-sidecar", "gpu-1", 0, "ReplicaSet"),
		pod("dcgm-exporter", "gpu-1", 0, "DaemonSet"),
		pod("other-node-job", "gpu-2", 8, "Job"),
	)

	// Inside the 2m grace period: wait, touch nothing.
	if res := h.reconcile(t, "gpu-1"); res.RequeueAfter != 90*time.Second {
		t.Fatalf("want requeue 90s, got %v", res.RequeueAfter)
	}
	if h.node(t, "gpu-1").Spec.Unschedulable {
		t.Fatal("must not cordon inside grace period")
	}

	// Past grace: quarantine.
	*h.now = t0.Add(2 * time.Minute)
	h.reconcile(t, "gpu-1")
	n := h.node(t, "gpu-1")
	if !n.Spec.Unschedulable || !hasTaint(n) || n.Annotations[api.AnnotationQuarantinedAt] == "" {
		t.Fatalf("node not quarantined: %+v %+v", n.Spec, n.Annotations)
	}
	if !strings.Contains(n.Annotations[api.AnnotationIncidentSummary], "PCIe bus") {
		t.Fatalf("incident summary missing: %q", n.Annotations[api.AnnotationIncidentSummary])
	}
	if h.podExists("trainer") {
		t.Fatal("GPU pod should have been evicted")
	}
	if !h.podExists("cpu-sidecar") || !h.podExists("dcgm-exporter") || !h.podExists("other-node-job") {
		t.Fatal("drain scope 'gpu' must leave non-GPU, DaemonSet and other-node pods alone")
	}
	if ev := h.events(); !strings.Contains(ev, "Quarantined") || !strings.Contains(ev, "IncidentSummary") {
		t.Fatalf("missing events: %s", ev)
	}

	// Agent reports healthy again; release only after the 10m recovery period.
	n.Status.Conditions[0].Status = corev1.ConditionTrue
	n.Status.Conditions[0].LastTransitionTime = metav1.NewTime(*h.now)
	n.Status.Conditions[0].LastHeartbeatTime = metav1.NewTime(*h.now)
	if err := h.c.Status().Update(context.Background(), n); err != nil {
		t.Fatal(err)
	}
	if res := h.reconcile(t, "gpu-1"); res.RequeueAfter != 10*time.Minute {
		t.Fatalf("want 10m recovery wait, got %v", res.RequeueAfter)
	}
	*h.now = h.now.Add(10 * time.Minute)
	n = h.node(t, "gpu-1")
	n.Status.Conditions[0].LastHeartbeatTime = metav1.NewTime(*h.now)
	_ = h.c.Status().Update(context.Background(), n)
	h.reconcile(t, "gpu-1")

	n = h.node(t, "gpu-1")
	if n.Spec.Unschedulable || hasTaint(n) || isQuarantinedByUs(n) {
		t.Fatalf("node not released: %+v %+v", n.Spec, n.Annotations)
	}
	if !strings.Contains(h.events(), "Released") {
		t.Fatal("missing Released event")
	}
}

func TestDisruptionBudgetDefers(t *testing.T) {
	already := gpuNode("gpu-a", corev1.ConditionFalse, t0.Add(-time.Hour))
	already.Annotations = map[string]string{api.AnnotationQuarantinedAt: t0.Format(time.RFC3339)}
	h := newHarness(t, nil, already, gpuNode("gpu-b", corev1.ConditionFalse, t0.Add(-time.Hour)))

	// Fleet of 2, budget max(1, 10%) = 1, already used by gpu-a.
	res := h.reconcile(t, "gpu-b")
	if res.RequeueAfter != time.Minute || h.node(t, "gpu-b").Spec.Unschedulable {
		t.Fatalf("expected deferral, got %v unschedulable=%v", res, h.node(t, "gpu-b").Spec.Unschedulable)
	}
	if !strings.Contains(h.events(), "RemediationDeferred") {
		t.Fatal("missing RemediationDeferred event")
	}
}

func TestPDBBlockedEvictionRequeues(t *testing.T) {
	funcs := &interceptor.Funcs{
		SubResourceCreate: func(ctx context.Context, c client.Client, sub string, obj client.Object, sr client.Object, opts ...client.SubResourceCreateOption) error {
			if sub == "eviction" && obj.GetName() == "protected" {
				return apierrors.NewTooManyRequests("Cannot evict pod as it would violate the pod's disruption budget.", 10)
			}
			return c.SubResource(sub).Create(ctx, obj, sr, opts...)
		},
	}
	h := newHarness(t, funcs,
		gpuNode("gpu-1", corev1.ConditionFalse, t0.Add(-time.Hour)),
		pod("protected", "gpu-1", 1, "StatefulSet"),
		pod("free", "gpu-1", 1, "Job"),
	)
	res := h.reconcile(t, "gpu-1")
	if res.RequeueAfter != 30*time.Second {
		t.Fatalf("want 30s requeue while PDB blocks, got %v", res.RequeueAfter)
	}
	if h.podExists("free") || !h.podExists("protected") {
		t.Fatal("expected free evicted and protected kept")
	}
	if ev := h.events(); !strings.Contains(ev, "EvictionBlocked") || !strings.Contains(h.node(t, "gpu-1").Annotations[api.AnnotationIncidentSummary], "1 blocked") {
		t.Fatalf("missing blocked signal: %s", ev)
	}
}

func TestReleaseKeepsHumanCordon(t *testing.T) {
	n := gpuNode("gpu-1", corev1.ConditionFalse, t0.Add(-time.Hour))
	n.Spec.Unschedulable = true // an operator cordoned it for maintenance first
	h := newHarness(t, nil, n)
	h.reconcile(t, "gpu-1")
	if h.node(t, "gpu-1").Annotations[api.AnnotationWasUnschedulable] != "true" {
		t.Fatal("prior cordon not recorded")
	}

	got := h.node(t, "gpu-1")
	got.Status.Conditions[0].Status = corev1.ConditionTrue
	got.Status.Conditions[0].LastTransitionTime = metav1.NewTime(t0.Add(-time.Hour))
	_ = h.c.Status().Update(context.Background(), got)
	h.reconcile(t, "gpu-1")

	got = h.node(t, "gpu-1")
	if !got.Spec.Unschedulable || hasTaint(got) {
		t.Fatalf("release must remove our taint but keep the human cordon: %+v", got.Spec)
	}
}

func TestDryRunAndSelector(t *testing.T) {
	cpu := gpuNode("cpu-1", corev1.ConditionFalse, t0.Add(-time.Hour))
	cpu.Labels = map[string]string{}
	h := newHarness(t, nil, gpuNode("gpu-1", corev1.ConditionFalse, t0.Add(-time.Hour)), cpu,
		pod("trainer", "gpu-1", 1, "Job"))
	h.r.DryRun = true

	h.reconcile(t, "gpu-1")
	h.reconcile(t, "cpu-1")
	if h.node(t, "gpu-1").Spec.Unschedulable || !h.podExists("trainer") {
		t.Fatal("dry run must not mutate anything")
	}
	if !strings.Contains(h.events(), "DryRunQuarantine") {
		t.Fatal("dry run should emit an event")
	}
	if h.node(t, "cpu-1").Spec.Unschedulable {
		t.Fatal("nodes outside the selector are ignored")
	}
}

func TestStaleAgentIsIgnored(t *testing.T) {
	n := gpuNode("gpu-1", corev1.ConditionFalse, t0.Add(-time.Hour))
	n.Status.Conditions[0].LastHeartbeatTime = metav1.NewTime(t0.Add(-time.Hour))
	h := newHarness(t, nil, n)
	h.reconcile(t, "gpu-1")
	if h.node(t, "gpu-1").Spec.Unschedulable {
		t.Fatal("must not act on a stale condition")
	}
}

func TestShouldEvict(t *testing.T) {
	r := &NodeReconciler{DrainScope: DrainGPUPods}
	done := pod("done", "n", 1, "Job")
	done.Status.Phase = corev1.PodSucceeded
	mirror := pod("mirror", "n", 1, "")
	mirror.Annotations = map[string]string{corev1.MirrorPodAnnotationKey: "x"}
	init := pod("init", "n", 0, "Job")
	init.Spec.InitContainers = []corev1.Container{{Name: "i", Resources: corev1.ResourceRequirements{
		Requests: corev1.ResourceList{api.GPUResource: resource.MustParse("1")}}}}

	cases := map[*corev1.Pod]bool{
		pod("gpu", "n", 1, "Job"): true, pod("cpu", "n", 0, "Job"): false,
		pod("ds", "n", 1, "DaemonSet"): false, done: false, mirror: false, init: true,
	}
	for p, want := range cases {
		if got := r.shouldEvict(p, r.DrainScope); got != want {
			t.Errorf("%s: shouldEvict = %v, want %v", p.Name, got, want)
		}
	}
	r.DrainScope = DrainAllPods
	if !r.shouldEvict(pod("cpu", "n", 0, "Job"), r.DrainScope) {
		t.Error("drain scope 'all' should evict CPU pods")
	}
}

func heartbeatLease(node string, renewed time.Time) *coordinationv1.Lease {
	mt := metav1.NewMicroTime(renewed)
	return &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: api.LeaseName(node), Namespace: "gpu-sentinel"},
		Spec:       coordinationv1.LeaseSpec{RenewTime: &mt},
	}
}

// With Lease heartbeats the condition's own heartbeat is only rewritten on
// change, so a fresh Lease must keep an old condition from looking stale.
func TestFreshLeaseKeepsOldConditionActionable(t *testing.T) {
	n := gpuNode("gpu-1", corev1.ConditionFalse, t0.Add(-time.Hour))
	n.Status.Conditions[0].LastHeartbeatTime = metav1.NewTime(t0.Add(-time.Hour))
	h := newHarness(t, nil, n, heartbeatLease("gpu-1", t0.Add(-10*time.Second)))
	h.r.LeaseNamespace = "gpu-sentinel"
	h.reconcile(t, "gpu-1")
	if !h.node(t, "gpu-1").Spec.Unschedulable {
		t.Fatal("fresh lease should make the unhealthy condition actionable")
	}
}

func TestStaleLeaseIsIgnored(t *testing.T) {
	n := gpuNode("gpu-1", corev1.ConditionFalse, t0.Add(-time.Hour))
	n.Status.Conditions[0].LastHeartbeatTime = metav1.NewTime(t0.Add(-time.Hour))
	h := newHarness(t, nil, n, heartbeatLease("gpu-1", t0.Add(-time.Hour)))
	h.r.LeaseNamespace = "gpu-sentinel"
	h.reconcile(t, "gpu-1")
	if h.node(t, "gpu-1").Spec.Unschedulable {
		t.Fatal("must not act when both heartbeats are stale")
	}
}

// Agents that predate Lease support keep working through the condition.
func TestMissingLeaseFallsBackToConditionHeartbeat(t *testing.T) {
	h := newHarness(t, nil, gpuNode("gpu-1", corev1.ConditionFalse, t0.Add(-time.Hour)))
	h.r.LeaseNamespace = "gpu-sentinel"
	h.reconcile(t, "gpu-1")
	if !h.node(t, "gpu-1").Spec.Unschedulable {
		t.Fatal("fresh condition heartbeat should still be honored without a lease")
	}
}
