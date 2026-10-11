//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	v1alpha1 "github.com/Nags-gk/gpu-fleet-sentinel/api/v1alpha1"
	"github.com/Nags-gk/gpu-fleet-sentinel/internal/api"
	"github.com/Nags-gk/gpu-fleet-sentinel/internal/controller"
	"github.com/Nags-gk/gpu-fleet-sentinel/internal/incident"
	"github.com/Nags-gk/gpu-fleet-sentinel/internal/remediation"
)

// Registered once at init: mutating the global scheme inside a test races with
// the previous test's manager, which is still shutting down.
func init() { utilruntime.Must(v1alpha1.AddToScheme(scheme.Scheme)) }

// The CRD shipped in the Helm chart is loaded into a real apiserver, so the
// CEL admission rules and the status subresource are the real thing.
func TestGPUNodePolicyAgainstRealAPIServer(t *testing.T) {
	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{"../../deploy/helm/gpu-fleet-sentinel/crds"},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start envtest (run `make envtest` first): %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	c, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	must(t, err)

	t.Run("admission", func(t *testing.T) {
		sel := metav1.LabelSelector{MatchLabels: map[string]string{"pool": "x"}}
		mk := func(name string, spec v1alpha1.GPUNodePolicySpec) error {
			return c.Create(ctx, &v1alpha1.GPUNodePolicy{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: spec})
		}
		rejected := map[string]struct {
			spec v1alpha1.GPUNodePolicySpec
			want string
		}{
			"empty-selector": {v1alpha1.GPUNodePolicySpec{}, "empty selector"},
			"negative-grace": {v1alpha1.GPUNodePolicySpec{NodeSelector: sel, GracePeriod: &metav1.Duration{Duration: -time.Second}}, "non-negative"},
			"zero-stale":     {v1alpha1.GPUNodePolicySpec{NodeSelector: sel, StaleAfter: &metav1.Duration{}}, "positive"},
			"percent-101":    {v1alpha1.GPUNodePolicySpec{NodeSelector: sel, MaxUnavailablePercent: ptr.To[int32](101)}, "100"},
			"bad-scope":      {v1alpha1.GPUNodePolicySpec{NodeSelector: sel, DrainScope: ptr.To(v1alpha1.DrainScope("everything"))}, "Unsupported value"},
		}
		for name, tc := range rejected {
			err := mk(name, tc.spec)
			if err == nil {
				t.Errorf("%s: apiserver accepted an invalid policy", name)
			} else if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("%s: error %q does not mention %q", name, err, tc.want)
			}
		}
		if err := mk("ok", v1alpha1.GPUNodePolicySpec{NodeSelector: sel, GracePeriod: &metav1.Duration{Duration: time.Minute}}); err != nil {
			t.Errorf("valid policy rejected: %v", err)
		}
	})

	t.Run("enforcement", func(t *testing.T) {
		sel, _ := labels.Parse(api.DefaultGPUNodeSelector)
		mgr, err := ctrl.NewManager(cfg, ctrl.Options{
			Scheme: scheme.Scheme, Metrics: metricsserver.Options{BindAddress: "0"},
			Controller: config.Controller{SkipNameValidation: ptr.To(true)},
		})
		must(t, err)
		r := &controller.NodeReconciler{
			Client: mgr.GetClient(), Recorder: mgr.GetEventRecorderFor("sentinel"),
			// The flag defaults would wait an hour; only the policy makes this fast.
			Policy: remediation.Policy{GracePeriod: time.Hour, RecoveryPeriod: time.Hour,
				StaleAfter: time.Minute, MaxUnavailable: 1, DeferRetry: time.Second},
			Selector: sel, DrainScope: controller.DrainGPUPods, Summarizer: incident.Template{},
			UsePolicyCRD: true,
			Evictor:      controller.RESTEvictor{REST: kubernetes.NewForConfigOrDie(cfg).PolicyV1().RESTClient()},
		}
		must(t, r.SetupWithManager(mgr))
		must(t, (&controller.PolicyStatusReconciler{Client: mgr.GetClient(), Resolver: r.Resolver()}).SetupWithManager(mgr))
		go func() { _ = mgr.Start(ctx) }()

		pol := &v1alpha1.GPUNodePolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "pool-a"},
			Spec: v1alpha1.GPUNodePolicySpec{
				NodeSelector:   metav1.LabelSelector{MatchLabels: map[string]string{"pool": "a"}},
				GracePeriod:    &metav1.Duration{Duration: 2 * time.Second},
				MaxUnavailable: ptr.To[int32](1),
			},
		}
		must(t, c.Create(ctx, pol))

		for _, n := range []struct{ name, pool string }{{"in-policy", "a"}, {"outside", "b"}} {
			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: n.name, Labels: map[string]string{
				"nvidia.com/gpu.present": "true", "pool": n.pool}}}
			must(t, c.Create(ctx, node))
			now := metav1.Now()
			node.Status.Conditions = []corev1.NodeCondition{{
				Type: api.ConditionGPUHealthy, Status: corev1.ConditionFalse, Message: "GPU0: XID 79",
				LastTransitionTime: now, LastHeartbeatTime: now,
			}}
			must(t, c.Status().Update(ctx, node))
		}

		eventually(t, 20*time.Second, "policy grace quarantines in-policy node", func() bool {
			var n corev1.Node
			return c.Get(ctx, types.NamespacedName{Name: "in-policy"}, &n) == nil && n.Spec.Unschedulable
		})
		var out corev1.Node
		must(t, c.Get(ctx, types.NamespacedName{Name: "outside"}, &out))
		if out.Spec.Unschedulable {
			t.Fatal("node outside the policy followed the 1h flag default and must stay schedulable")
		}

		eventually(t, 20*time.Second, "status reflects the quarantine", func() bool {
			var p v1alpha1.GPUNodePolicy
			if c.Get(ctx, types.NamespacedName{Name: "pool-a"}, &p) != nil {
				return false
			}
			return p.Status.ManagedNodes == 1 && p.Status.QuarantinedNodes == 1 && p.Status.UnhealthyNodes == 1 &&
				p.Status.Budget == 1 && meta.IsStatusConditionTrue(p.Status.Conditions, v1alpha1.ConditionReady) &&
				len(p.Status.QuarantinedNodeNames) == 1 && p.Status.QuarantinedNodeNames[0] == "in-policy"
		})
	})
}
