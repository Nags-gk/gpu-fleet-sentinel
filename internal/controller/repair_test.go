package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/Nags-gk/gpu-fleet-sentinel/api/v1alpha1"
	"github.com/Nags-gk/gpu-fleet-sentinel/internal/api"
)

type fakeActuator struct {
	reboots  []string // "node#attempt"
	replaced []string
	err      error
	cleaned  []string
}

func (f *fakeActuator) Reboot(_ context.Context, n *corev1.Node, attempt int) error {
	f.reboots = append(f.reboots, n.Name+"#"+string(rune('0'+attempt)))
	return f.err
}
func (f *fakeActuator) RequestReplacement(_ context.Context, n *corev1.Node) error {
	f.replaced = append(f.replaced, n.Name)
	return nil
}
func (f *fakeActuator) Cleanup(_ context.Context, n *corev1.Node) error {
	f.cleaned = append(f.cleaned, n.Name)
	return nil
}

func repairPolicy(mutate func(*v1alpha1.EscalationSpec)) *v1alpha1.GPUNodePolicy {
	return policy("repair", "a", func(s *v1alpha1.GPUNodePolicySpec) {
		s.GracePeriod = &metav1.Duration{Duration: time.Second}
		s.RecoveryPeriod = &metav1.Duration{Duration: time.Minute}
		s.MaxUnavailable, s.MaxUnavailablePercent = ptr.To[int32](5), ptr.To[int32](0)
		e := &v1alpha1.EscalationSpec{
			After:         metav1.Duration{Duration: 10 * time.Minute},
			Cooldown:      &metav1.Duration{Duration: 20 * time.Minute},
			MaxAttempts:   ptr.To[int32](2),
			MaxConcurrent: ptr.To[int32](1),
		}
		if mutate != nil {
			mutate(e)
		}
		s.Escalation = e
	})
}

func repairHarness(t *testing.T, funcs *interceptor.Funcs, objs ...client.Object) (*harness, *fakeActuator) {
	t.Helper()
	h := newHarness(t, funcs, objs...)
	h.r.UsePolicyCRD = true
	act := &fakeActuator{}
	h.r.Actuator = act
	return h, act
}

// step advances the clock, refreshes the agent heartbeat (so the node is not
// stale) and reconciles.
func (h *harness) step(t *testing.T, node string, after time.Duration) {
	t.Helper()
	*h.now = h.now.Add(after)
	n := h.node(t, node)
	gpuCondition(n).LastHeartbeatTime = metav1.NewTime(*h.now)
	if err := h.c.Status().Update(context.Background(), n); err != nil {
		t.Fatal(err)
	}
	h.reconcile(t, node)
}

func unhealthyAt(name string) *corev1.Node {
	return poolNode(name, "a", corev1.ConditionFalse, t0.Add(-time.Minute))
}

// A node that never recovers is rebooted at most MaxAttempts times, spaced by
// the cooldown, and then handed off for replacement exactly once.
func TestEscalationRebootsThenRequestsReplacement(t *testing.T) {
	h, act := repairHarness(t, nil, repairPolicy(nil), unhealthyAt("a1"))
	h.r.Policy.StaleAfter = 5 * time.Minute

	h.step(t, "a1", 0) // quarantined now
	if !h.node(t, "a1").Spec.Unschedulable {
		t.Fatal("expected quarantine")
	}
	h.step(t, "a1", 5*time.Minute)
	if len(act.reboots) != 0 {
		t.Fatalf("rebooted before the 10m escalation delay: %v", act.reboots)
	}
	h.step(t, "a1", 6*time.Minute) // 11m quarantined
	if len(act.reboots) != 1 || act.reboots[0] != "a1#1" {
		t.Fatalf("want first reboot, got %v", act.reboots)
	}
	if got := h.node(t, "a1").Annotations[api.AnnotationRepairAttempts]; got != "1" {
		t.Fatalf("attempt not recorded: %q", got)
	}
	h.step(t, "a1", 5*time.Minute) // inside the 20m cooldown
	if len(act.reboots) != 1 {
		t.Fatalf("rebooted inside the cooldown: %v", act.reboots)
	}
	h.step(t, "a1", 16*time.Minute) // 21m after attempt 1, node still unhealthy
	if len(act.reboots) != 2 || act.reboots[1] != "a1#2" {
		t.Fatalf("want second reboot, got %v", act.reboots)
	}
	h.step(t, "a1", 21*time.Minute) // reboots exhausted
	if len(act.reboots) != 2 || len(act.replaced) != 1 {
		t.Fatalf("want 2 reboots and one replacement request, got reboots=%v replaced=%v", act.reboots, act.replaced)
	}
	n := h.node(t, "a1")
	if _, ok := n.Annotations[api.AnnotationReplacementRequested]; !ok {
		t.Fatal("replacement request not recorded on the node")
	}
	h.step(t, "a1", time.Hour)
	if len(act.reboots) != 2 || len(act.replaced) != 1 {
		t.Fatalf("handed-off node must not be touched again: %v %v", act.reboots, act.replaced)
	}
	ev := h.events()
	for _, want := range []string{"RebootRequested", "ReplacementRequested"} {
		if !strings.Contains(ev, want) {
			t.Errorf("missing %s event", want)
		}
	}
}

// Never reboot a node while a PDB-protected pod is still on it, and never kill
// CPU pods a GPU-only drain would have left behind.
func TestRebootWaitsForFullDrain(t *testing.T) {
	blocked := true
	funcs := &interceptor.Funcs{
		SubResourceCreate: func(ctx context.Context, c client.Client, sub string, obj client.Object, sr client.Object, opts ...client.SubResourceCreateOption) error {
			if sub == "eviction" && obj.GetName() == "db" && blocked {
				return apierrors.NewTooManyRequests("violates PDB", 10)
			}
			return c.SubResource(sub).Create(ctx, obj, sr, opts...)
		},
	}
	h, act := repairHarness(t, funcs, repairPolicy(nil), unhealthyAt("a1"),
		pod("db", "a1", 0, "StatefulSet"), // CPU pod: outside the GPU-only drain scope
		pod("trainer", "a1", 8, "Job"),
		pod("agent", "a1", 0, "DaemonSet"))
	h.r.Policy.StaleAfter = 5 * time.Minute

	h.step(t, "a1", 0)
	h.step(t, "a1", 11*time.Minute)
	if len(act.reboots) != 0 {
		t.Fatalf("rebooted with a PDB-protected pod still running: %v", act.reboots)
	}
	if h.podExists("trainer") {
		t.Fatal("GPU pod should have been drained at quarantine")
	}
	if !strings.Contains(h.events(), "RebootDeferred") {
		t.Fatal("expected a RebootDeferred event")
	}
	blocked = false
	h.step(t, "a1", 20*time.Second)
	if len(act.reboots) != 1 {
		t.Fatalf("expected the reboot once the node was empty, got %v", act.reboots)
	}
	if h.podExists("db") {
		t.Fatal("the CPU pod should have been evicted before the reboot")
	}
	if !h.podExists("agent") {
		t.Fatal("DaemonSet pods are left alone")
	}
}

func TestRebootConcurrencyLimit(t *testing.T) {
	h, act := repairHarness(t, nil, repairPolicy(nil), unhealthyAt("a1"), unhealthyAt("a2"))
	h.r.Policy.StaleAfter = 5 * time.Minute
	h.step(t, "a1", 0)
	h.step(t, "a2", 0)
	h.step(t, "a1", 11*time.Minute)
	h.step(t, "a2", 0)
	if len(act.reboots) != 1 {
		t.Fatalf("MaxConcurrent=1 but reboots=%v", act.reboots)
	}
	if !strings.Contains(h.events(), "RebootRequested") {
		t.Fatal("first node should have been rebooted")
	}
}

// Attempts survive release, so a node that recovers and fails again does not
// get a fresh set of reboots.
func TestRepairHistorySurvivesRelease(t *testing.T) {
	h, act := repairHarness(t, nil, repairPolicy(nil), unhealthyAt("a1"))
	h.r.Policy.StaleAfter = 5 * time.Minute
	h.step(t, "a1", 0)
	h.step(t, "a1", 11*time.Minute)
	if len(act.reboots) != 1 {
		t.Fatal("setup: expected one reboot")
	}
	// The reboot worked: the agent reports healthy again.
	n := h.node(t, "a1")
	gpuCondition(n).Status, gpuCondition(n).LastTransitionTime = corev1.ConditionTrue, metav1.NewTime(*h.now)
	if err := h.c.Status().Update(context.Background(), n); err != nil {
		t.Fatal(err)
	}
	h.step(t, "a1", 2*time.Minute) // past the 1m recovery period
	n = h.node(t, "a1")
	if n.Spec.Unschedulable || n.Annotations[api.AnnotationQuarantinedAt] != "" {
		t.Fatalf("node should be released: %+v", n.Annotations)
	}
	if n.Annotations[api.AnnotationRepairAttempts] != "1" || n.Annotations[api.AnnotationLastRepairAt] == "" {
		t.Fatalf("repair history must survive release: %+v", n.Annotations)
	}
	if _, ok := n.Annotations[api.AnnotationRebootRequested]; ok {
		t.Fatal("reboot-requested should be cleared on release")
	}
	if len(act.cleaned) != 1 {
		t.Fatalf("actuator cleanup not called on release: %v", act.cleaned)
	}
}

func TestEscalationDisabledByDefault(t *testing.T) {
	// A policy without an escalation block never reboots anything.
	plain := policy("plain", "a", func(s *v1alpha1.GPUNodePolicySpec) { s.GracePeriod = &metav1.Duration{Duration: time.Second} })
	h, act := repairHarness(t, nil, plain, unhealthyAt("a1"))
	h.r.Policy.StaleAfter = 5 * time.Minute
	h.step(t, "a1", 0)
	h.step(t, "a1", 48*time.Hour)
	if len(act.reboots)+len(act.replaced) != 0 {
		t.Fatalf("no escalation configured, yet %v %v", act.reboots, act.replaced)
	}
	// And a policy with escalation but no actuator is inert too.
	h2 := policyHarness(t, repairPolicy(nil), unhealthyAt("a1"))
	h2.r.Policy.StaleAfter = 5 * time.Minute
	h2.step(t, "a1", 0)
	h2.step(t, "a1", 48*time.Hour)
	if _, ok := h2.node(t, "a1").Annotations[api.AnnotationRepairAttempts]; ok {
		t.Fatal("repair attempted without an actuator")
	}
}

func TestRebootFailureStillConsumesTheAttempt(t *testing.T) {
	h, act := repairHarness(t, nil, repairPolicy(nil), unhealthyAt("a1"))
	h.r.Policy.StaleAfter = 5 * time.Minute
	act.err = errors.New("boom")
	h.step(t, "a1", 0)
	h.step(t, "a1", 11*time.Minute)
	if got := h.node(t, "a1").Annotations[api.AnnotationRepairAttempts]; got != "1" {
		t.Fatalf("a failed actuator must still count the attempt (bounded retries), got %q", got)
	}
	if !strings.Contains(h.events(), "RebootFailed") {
		t.Fatal("expected a RebootFailed event")
	}
}

func TestInvalidEscalationMakesPolicyInvalid(t *testing.T) {
	d := Defaults{DrainScope: DrainGPUPods}
	d.Policy.StaleAfter = time.Minute
	rp := fromSpec(repairPolicy(func(e *v1alpha1.EscalationSpec) { e.After = metav1.Duration{} }), d)
	if rp.Err == nil {
		t.Fatal("zero escalation.after must be invalid")
	}
}

func TestPodActuator(t *testing.T) {
	h := newHarness(t, nil, unhealthyAt("a1"))
	node := h.node(t, "a1")
	a := PodActuator{Client: h.c, Namespace: "gpu-sentinel", Image: "busybox:1.37"}
	for i := 0; i < 2; i++ { // second call must be a no-op, not an error
		if err := a.Reboot(context.Background(), node, 1); err != nil {
			t.Fatal(err)
		}
	}
	var p corev1.Pod
	if err := h.c.Get(context.Background(), types.NamespacedName{Namespace: "gpu-sentinel", Name: "gpu-sentinel-reboot-a1-1"}, &p); err != nil {
		t.Fatal(err)
	}
	c := p.Spec.Containers[0]
	if p.Spec.NodeName != "a1" || !p.Spec.HostPID || c.SecurityContext == nil || !*c.SecurityContext.Privileged ||
		len(c.Command) < 2 || c.Command[0] != "nsenter" || c.Command[len(c.Command)-1] != "reboot" {
		t.Fatalf("reboot pod misconfigured: %+v", p.Spec)
	}
	if err := a.Cleanup(context.Background(), node); err != nil {
		t.Fatal(err)
	}
	if err := h.c.Get(context.Background(), client.ObjectKeyFromObject(&p), &p); !apierrors.IsNotFound(err) {
		t.Fatalf("cleanup should delete the reboot pod, got %v", err)
	}
}
