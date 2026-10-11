//go:build integration

// Package integration runs the real controller manager and node agents against
// a real kube-apiserver + etcd (controller-runtime envtest). Unlike the fake
// client, this exercises strategic-merge status patches, optimistic-lock
// conflicts, the Eviction API and PodDisruptionBudget enforcement.
//
//	make test-integration   (downloads the apiserver/etcd binaries once)
package integration

import (
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/Nags-gk/gpu-fleet-sentinel/internal/agent"
	"github.com/Nags-gk/gpu-fleet-sentinel/internal/api"
	"github.com/Nags-gk/gpu-fleet-sentinel/internal/controller"
	"github.com/Nags-gk/gpu-fleet-sentinel/internal/health"
	"github.com/Nags-gk/gpu-fleet-sentinel/internal/incident"
	"github.com/Nags-gk/gpu-fleet-sentinel/internal/remediation"
)

func TestEndToEndAgainstRealAPIServer(t *testing.T) {
	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))
	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start envtest (run `make envtest` first): %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	c, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	must(t, err)

	// Three GPU nodes with fake GPU capacity, plus one CPU node.
	nodes := []string{"gpu-a", "gpu-b", "gpu-c"}
	for _, n := range nodes {
		must(t, c.Create(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: n, Labels: map[string]string{
			"nvidia.com/gpu.present": "true", "nvidia.com/gpu.product": "NVIDIA-H100-80GB-HBM3"}}}))
	}
	must(t, c.Create(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "cpu-a"}}))

	// Controller manager with fast timers.
	sel, _ := labels.Parse(api.DefaultGPUNodeSelector)
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: scheme.Scheme, Metrics: metricsserver.Options{BindAddress: "0"},
		// Allows `go test -count=N`, which re-registers the controller in one process.
		Controller: config.Controller{SkipNameValidation: ptr.To(true)},
	})
	must(t, err)
	r := &controller.NodeReconciler{
		Client: mgr.GetClient(), Recorder: mgr.GetEventRecorderFor("sentinel"),
		Policy: remediation.Policy{GracePeriod: 2 * time.Second, RecoveryPeriod: 3 * time.Second,
			StaleAfter: time.Minute, MaxUnavailable: 1, DeferRetry: time.Second},
		Selector: sel, DrainScope: controller.DrainGPUPods, Summarizer: incident.Template{}, LeaseNamespace: "default",
		Evictor: controller.RESTEvictor{REST: kubernetes.NewForConfigOrDie(cfg).PolicyV1().RESTClient()},
	}
	must(t, r.SetupWithManager(mgr))
	go func() { _ = mgr.Start(ctx) }()

	// One agent per GPU node, each with its own simulated GPUs.
	sims := map[string]*health.SimSource{}
	for _, n := range nodes {
		sim := health.NewSimSource(n, "H100", 8, 1)
		sims[n] = sim
		a := &agent.Agent{
			NodeName: n, Source: sim,
			Evaluator: health.NewEvaluator(health.Thresholds{ExpectedGPUs: 8}),
			Debouncer: &health.Debouncer{FailAfter: 2, RecoverAfter: 2},
			Client:    c, Metrics: agent.NewMetrics(prometheus.NewRegistry()),
			Interval: 300 * time.Millisecond, Log: logr.Discard(),
			LeaseNamespace: "default", ConditionResync: time.Hour,
		}
		go func() { _ = a.Run(ctx) }()
	}
	for _, n := range nodes {
		eventually(t, 10*time.Second, n+" GPUHealthy=True", func() bool { return condStatus(ctx, c, n) == corev1.ConditionTrue })
	}

	// Workloads on gpu-a: a GPU job (evictable) and a GPU pod protected by a PDB.
	must(t, c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "ml"}}))
	for _, p := range []*corev1.Pod{gpuPod("job", "gpu-a", nil), gpuPod("protected", "gpu-a", map[string]string{"app": "db"})} {
		must(t, c.Create(ctx, p))
		// No kubelet in envtest: mark pods Running like a kubelet would. The
		// Eviction API skips PDB checks for Pending pods, so this matters.
		p.Status.Phase = corev1.PodRunning
		p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
		must(t, c.Status().Update(ctx, p))
	}
	minAvail := intstr.FromInt32(1)
	must(t, c.Create(ctx, &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "ml"},
		Spec: policyv1.PodDisruptionBudgetSpec{MinAvailable: &minAvail,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "db"}}},
	}))

	// --- Scenario 1: XID 79 on gpu-a.
	start := time.Now()
	must(t, sims["gpu-a"].Inject(3, health.FaultXID79))
	eventually(t, 15*time.Second, "gpu-a quarantined", func() bool { return quarantined(ctx, c, "gpu-a") })
	t.Logf("fault → quarantine: %s (condition timestamps have 1s resolution, so a 2s grace can fire ~1s early)", time.Since(start).Round(10*time.Millisecond))

	var n corev1.Node
	eventually(t, 5*time.Second, "incident summary annotation", func() bool {
		var n corev1.Node
		_ = c.Get(ctx, types.NamespacedName{Name: "gpu-a"}, &n)
		return n.Annotations[api.AnnotationIncidentSummary] != ""
	})
	must(t, c.Get(ctx, types.NamespacedName{Name: "gpu-a"}, &n))
	if len(n.Status.Conditions) == 0 {
		t.Fatal("node lost its conditions")
	}

	// Eviction is graceful: with no kubelet the pod is marked for deletion
	// rather than removed, which is exactly what the Eviction API guarantees.
	eventually(t, 10*time.Second, "GPU job evicted", func() bool { return podDeletingOrGone(ctx, c, "job") })
	// No disruption controller runs in envtest, so the PDB never reports
	// allowed disruptions and the apiserver must refuse this eviction.
	time.Sleep(time.Second)
	if podDeletingOrGone(ctx, c, "protected") {
		t.Fatal("PDB-protected pod must not be evicted")
	}

	// CPU node is never touched.
	must(t, c.Get(ctx, types.NamespacedName{Name: "cpu-a"}, &n))
	if n.Spec.Unschedulable {
		t.Fatal("non-GPU node was cordoned")
	}

	// --- Scenario 2: budget. gpu-a holds the only slot; gpu-b must be deferred.
	must(t, sims["gpu-b"].Inject(0, health.FaultECCDBE))
	eventually(t, 10*time.Second, "gpu-b reports unhealthy", func() bool { return condStatus(ctx, c, "gpu-b") == corev1.ConditionFalse })
	time.Sleep(4 * time.Second) // well past the 2s grace period
	if quarantined(ctx, c, "gpu-b") {
		t.Fatal("disruption budget violated: gpu-b quarantined while gpu-a holds the only slot")
	}
	eventually(t, 5*time.Second, "RemediationDeferred event for gpu-b", func() bool {
		var evs corev1.EventList
		_ = c.List(ctx, &evs)
		for _, e := range evs.Items {
			if e.Reason == "RemediationDeferred" && e.InvolvedObject.Name == "gpu-b" {
				return true
			}
		}
		return false
	})

	// --- Scenario 3: gpu-a recovers → released → budget frees → gpu-b quarantined.
	must(t, sims["gpu-a"].Inject(3, health.FaultNone))
	eventually(t, 15*time.Second, "gpu-a released", func() bool {
		var n corev1.Node
		_ = c.Get(ctx, types.NamespacedName{Name: "gpu-a"}, &n)
		return !quarantined(ctx, c, "gpu-a") && !n.Spec.Unschedulable && !hasTaint(&n)
	})
	eventually(t, 15*time.Second, "gpu-b quarantined once budget frees", func() bool { return quarantined(ctx, c, "gpu-b") })

	// --- Scenario 4: a GPU vanishes from gpu-c; detected, but deferred because gpu-b holds the slot.
	must(t, sims["gpu-c"].Inject(1, health.FaultDisappear))
	eventually(t, 10*time.Second, "gpu-c unhealthy (missing GPU)", func() bool { return condStatus(ctx, c, "gpu-c") == corev1.ConditionFalse })
	time.Sleep(3 * time.Second)
	if quarantined(ctx, c, "gpu-c") {
		t.Fatal("budget violated for gpu-c")
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatal(err)
	}
}

func eventually(t *testing.T, timeout time.Duration, what string, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if f() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for: %s", timeout, what)
}

func condStatus(ctx context.Context, c client.Client, node string) corev1.ConditionStatus {
	var n corev1.Node
	if err := c.Get(ctx, types.NamespacedName{Name: node}, &n); err != nil {
		return ""
	}
	for _, cond := range n.Status.Conditions {
		if cond.Type == api.ConditionGPUHealthy {
			return cond.Status
		}
	}
	return ""
}

func quarantined(ctx context.Context, c client.Client, node string) bool {
	var n corev1.Node
	if err := c.Get(ctx, types.NamespacedName{Name: node}, &n); err != nil {
		return false
	}
	_, ok := n.Annotations[api.AnnotationQuarantinedAt]
	return ok && n.Spec.Unschedulable && hasTaint(&n)
}

func hasTaint(n *corev1.Node) bool {
	for _, t := range n.Spec.Taints {
		if t.Key == api.TaintKey {
			return true
		}
	}
	return false
}

func podDeletingOrGone(ctx context.Context, c client.Client, name string) bool {
	var p corev1.Pod
	err := c.Get(ctx, types.NamespacedName{Namespace: "ml", Name: name}, &p)
	return apierrors.IsNotFound(err) || (err == nil && p.DeletionTimestamp != nil)
}

func gpuPod(name, node string, lbls map[string]string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ml", Labels: lbls},
		Spec: corev1.PodSpec{NodeName: node, Containers: []corev1.Container{{
			Name: "c", Image: "registry.k8s.io/pause:3.10",
			Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{
				api.GPUResource: resource.MustParse("1")}},
		}}},
	}
}
