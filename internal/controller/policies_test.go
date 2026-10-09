package controller

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
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/Nags-gk/gpu-fleet-sentinel/api/v1alpha1"
	"github.com/Nags-gk/gpu-fleet-sentinel/internal/api"
	"github.com/Nags-gk/gpu-fleet-sentinel/internal/remediation"
)

func init() { _ = v1alpha1.AddToScheme(clientgoscheme.Scheme) }

func policy(name string, pool string, mutate func(*v1alpha1.GPUNodePolicySpec)) *v1alpha1.GPUNodePolicy {
	p := &v1alpha1.GPUNodePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name, Generation: 1},
		Spec: v1alpha1.GPUNodePolicySpec{
			NodeSelector: metav1.LabelSelector{MatchLabels: map[string]string{"pool": pool}},
		},
	}
	if mutate != nil {
		mutate(&p.Spec)
	}
	return p
}

func poolNode(name, pool string, status corev1.ConditionStatus, since time.Time) *corev1.Node {
	n := gpuNode(name, status, since)
	n.Labels["pool"] = pool
	return n
}

func policyHarness(t *testing.T, objs ...client.Object) *harness {
	t.Helper()
	h := newHarness(t, nil, objs...)
	h.r.UsePolicyCRD = true
	return h
}

func TestPolicyOverridesAndInheritsDefaults(t *testing.T) {
	d := Defaults{Policy: remediation.DefaultPolicy(), DrainScope: DrainGPUPods, DryRun: false}
	rp := fromSpec(policy("p", "a", func(s *v1alpha1.GPUNodePolicySpec) {
		s.GracePeriod = &metav1.Duration{Duration: 30 * time.Second}
		s.DryRun = ptr.To(true)
		s.MaxUnavailable = ptr.To[int32](3)
	}), d)
	if rp.Err != nil {
		t.Fatal(rp.Err)
	}
	if rp.Policy.GracePeriod != 30*time.Second || rp.Policy.MaxUnavailable != 3 || !rp.DryRun {
		t.Fatalf("overrides not applied: %+v", rp)
	}
	if rp.Policy.RecoveryPeriod != d.Policy.RecoveryPeriod || rp.Policy.StaleAfter != d.Policy.StaleAfter ||
		rp.Policy.MaxUnavailablePercent != d.Policy.MaxUnavailablePercent || rp.DrainScope != DrainGPUPods {
		t.Fatalf("unset fields must inherit defaults: %+v", rp)
	}
}

func TestInvalidPolicies(t *testing.T) {
	d := Defaults{Policy: remediation.DefaultPolicy(), DrainScope: DrainGPUPods}
	neg := &metav1.Duration{Duration: -time.Second}
	bad := ptr.To(v1alpha1.DrainScope("everything"))
	cases := map[string]func(*v1alpha1.GPUNodePolicySpec){
		"empty selector":   func(s *v1alpha1.GPUNodePolicySpec) { s.NodeSelector = metav1.LabelSelector{} },
		"negative grace":   func(s *v1alpha1.GPUNodePolicySpec) { s.GracePeriod = neg },
		"zero staleAfter":  func(s *v1alpha1.GPUNodePolicySpec) { s.StaleAfter = &metav1.Duration{} },
		"percent over 100": func(s *v1alpha1.GPUNodePolicySpec) { s.MaxUnavailablePercent = ptr.To[int32](101) },
		"unknown scope":    func(s *v1alpha1.GPUNodePolicySpec) { s.DrainScope = bad },
		"bad selector expr": func(s *v1alpha1.GPUNodePolicySpec) {
			s.NodeSelector = metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "k", Operator: "Bogus"}}}
		},
	}
	for name, mutate := range cases {
		if rp := fromSpec(policy("p", "a", mutate), d); rp.Err == nil {
			t.Errorf("%s: expected a validation error", name)
		}
	}
}

func TestResolvePrecedenceAndSingleCounting(t *testing.T) {
	sel, _ := labels.Parse(api.DefaultGPUNodeSelector)
	rs := Resolver{UseCRD: false, Defaults: Defaults{Policy: remediation.DefaultPolicy(), Selector: sel, DrainScope: DrainGPUPods}}
	zb := fromSpec(policy("b-pool", "a", nil), rs.Defaults)
	za := fromSpec(policy("a-first", "a", nil), rs.Defaults)
	ps := []ResolvedPolicy{za, zb, {Name: DefaultPolicyName, Selector: sel}}

	n := poolNode("n1", "a", corev1.ConditionTrue, t0)
	if got := Resolve(ps, n); got == nil || got.Name != "a-first" {
		t.Fatalf("first policy by order should win, got %+v", got)
	}
	nodes := []corev1.Node{*n, *poolNode("n2", "other", corev1.ConditionTrue, t0)}
	f := Fleets(ps, nodes)
	if f["a-first"].Total != 1 || f["b-pool"].Total != 0 || f[DefaultPolicyName].Total != 1 {
		t.Fatalf("a node must count toward exactly one policy: %+v", f)
	}
}

// A policy with a shorter grace period acts sooner than the flag default.
func TestPolicyGraceOverrideTakesEffect(t *testing.T) {
	short := policy("fast", "a", func(s *v1alpha1.GPUNodePolicySpec) {
		s.GracePeriod = &metav1.Duration{Duration: 10 * time.Second}
	})
	h := policyHarness(t, short,
		poolNode("in-policy", "a", corev1.ConditionFalse, t0.Add(-30*time.Second)),
		poolNode("flags-only", "b", corev1.ConditionFalse, t0.Add(-30*time.Second)))
	h.reconcile(t, "in-policy")
	h.reconcile(t, "flags-only")
	if !h.node(t, "in-policy").Spec.Unschedulable {
		t.Fatal("policy grace of 10s should have quarantined after 30s")
	}
	if h.node(t, "flags-only").Spec.Unschedulable {
		t.Fatal("default 2m grace must still apply to nodes outside the policy")
	}
}

// Each policy has its own disruption budget.
func TestBudgetsAreIndependentPerPolicy(t *testing.T) {
	one := func(s *v1alpha1.GPUNodePolicySpec) {
		s.MaxUnavailable, s.MaxUnavailablePercent = ptr.To[int32](1), ptr.To[int32](0)
		s.GracePeriod = &metav1.Duration{Duration: time.Second}
	}
	old := t0.Add(-time.Minute)
	h := policyHarness(t, policy("pool-a", "a", one), policy("pool-b", "b", one),
		poolNode("a1", "a", corev1.ConditionFalse, old), poolNode("a2", "a", corev1.ConditionFalse, old),
		poolNode("b1", "b", corev1.ConditionFalse, old))

	h.reconcile(t, "a1")
	h.reconcile(t, "a2") // pool-a budget is full
	h.reconcile(t, "b1") // pool-b has its own slot
	if !h.node(t, "a1").Spec.Unschedulable || h.node(t, "a2").Spec.Unschedulable {
		t.Fatal("pool-a must quarantine exactly one of its two unhealthy nodes")
	}
	if !h.node(t, "b1").Spec.Unschedulable {
		t.Fatal("pool-a's full budget must not block pool-b")
	}
	if !strings.Contains(h.events(), "RemediationDeferred") {
		t.Fatal("expected a RemediationDeferred event for a2")
	}
}

func TestInvalidPolicyTakesNoAction(t *testing.T) {
	bad := policy("bad", "a", func(s *v1alpha1.GPUNodePolicySpec) {
		s.StaleAfter = &metav1.Duration{} // invalid: must be positive
	})
	h := policyHarness(t, bad, poolNode("a1", "a", corev1.ConditionFalse, t0.Add(-time.Hour)))
	h.reconcile(t, "a1")
	if h.node(t, "a1").Spec.Unschedulable {
		t.Fatal("a node under an invalid policy must be left alone")
	}
	if !strings.Contains(h.events(), "InvalidPolicy") {
		t.Fatal("expected an InvalidPolicy warning event")
	}
}

func TestPolicyDryRunDoesNotCordon(t *testing.T) {
	dry := policy("dry", "a", func(s *v1alpha1.GPUNodePolicySpec) {
		s.DryRun, s.GracePeriod = ptr.To(true), &metav1.Duration{Duration: time.Second}
	})
	h := policyHarness(t, dry, poolNode("a1", "a", corev1.ConditionFalse, t0.Add(-time.Hour)))
	h.reconcile(t, "a1")
	if h.node(t, "a1").Spec.Unschedulable || !strings.Contains(h.events(), "DryRunQuarantine") {
		t.Fatalf("dry-run policy must only emit an event; events: %s", h.events())
	}
}

func TestPolicyDrainScopeAll(t *testing.T) {
	all := policy("all", "a", func(s *v1alpha1.GPUNodePolicySpec) {
		s.DrainScope = ptr.To(v1alpha1.DrainAllPods)
		s.GracePeriod = &metav1.Duration{Duration: time.Second}
	})
	h := policyHarness(t, all, poolNode("a1", "a", corev1.ConditionFalse, t0.Add(-time.Hour)),
		pod("web", "a1", 0, "ReplicaSet"), pod("trainer", "a1", 8, "Job"))
	h.reconcile(t, "a1")
	if h.podExists("web") || h.podExists("trainer") {
		t.Fatal("drainScope=all should evict CPU-only pods too")
	}
}

func TestStatusReconciler(t *testing.T) {
	pol := policy("pool-a", "a", func(s *v1alpha1.GPUNodePolicySpec) {
		s.MaxUnavailable, s.MaxUnavailablePercent = ptr.To[int32](1), ptr.To[int32](0)
	})
	q := poolNode("a1", "a", corev1.ConditionFalse, t0)
	q.Annotations = map[string]string{api.AnnotationQuarantinedAt: "x"}
	objs := []client.Object{pol, q,
		poolNode("a2", "a", corev1.ConditionFalse, t0), // unhealthy, waiting for budget
		poolNode("a3", "a", corev1.ConditionTrue, t0),
		poolNode("b1", "b", corev1.ConditionTrue, t0)}
	c := fake.NewClientBuilder().WithObjects(objs...).WithStatusSubresource(&v1alpha1.GPUNodePolicy{}).Build()
	sel, _ := labels.Parse(api.DefaultGPUNodeSelector)
	r := &PolicyStatusReconciler{Client: c, Resolver: Resolver{
		Reader: c, UseCRD: true,
		Defaults: Defaults{Policy: remediation.DefaultPolicy(), Selector: sel, DrainScope: DrainGPUPods},
	}}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "pool-a"}}); err != nil {
		t.Fatal(err)
	}
	var got v1alpha1.GPUNodePolicy
	if err := c.Get(context.Background(), types.NamespacedName{Name: "pool-a"}, &got); err != nil {
		t.Fatal(err)
	}
	st := got.Status
	if st.ManagedNodes != 3 || st.HealthyNodes != 1 || st.UnhealthyNodes != 2 || st.QuarantinedNodes != 1 || st.Budget != 1 {
		t.Fatalf("wrong counts: %+v", st)
	}
	if len(st.QuarantinedNodeNames) != 1 || st.QuarantinedNodeNames[0] != "a1" || st.ObservedGeneration != 1 {
		t.Fatalf("wrong names/generation: %+v", st)
	}
	if !meta.IsStatusConditionTrue(st.Conditions, v1alpha1.ConditionReady) ||
		!meta.IsStatusConditionTrue(st.Conditions, v1alpha1.ConditionBudgetExhausted) {
		t.Fatalf("want Ready and BudgetExhausted true: %+v", st.Conditions)
	}
}
