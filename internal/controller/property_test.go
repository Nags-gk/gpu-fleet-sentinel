package controller

import (
	"context"
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/Nags-gk/gpu-fleet-sentinel/api/v1alpha1"
)

// TestReconcilerNeverExceedsAnyPolicyBudget runs the real reconciler (fake
// client) over random fleets split across two GPUNodePolicies and the flag
// default. Whatever order health flips, clock jumps and reconciles arrive in:
//   - no policy ever has more nodes quarantined than its own budget,
//   - a node is cordoned if and only if the controller marked it quarantined,
//   - a quarantined node has no GPU pods left on it.
func TestReconcilerNeverExceedsAnyPolicyBudget(t *testing.T) {
	ctx := context.Background()
	deferred, quarantines := 0, 0
	// Guard against a vacuous run: the properties only mean something if the
	// fleets actually reached quarantines and budget contention.
	defer func() {
		t.Logf("exercised %d quarantines and %d budget deferrals", quarantines, deferred)
		if quarantines < 40 || deferred < 20 {
			t.Errorf("model too weak: only %d quarantines and %d deferrals", quarantines, deferred)
		}
	}()
	for seed := int64(0); seed < 50; seed++ {
		r := rand.New(rand.NewSource(seed))
		size := 4 + r.Intn(20)

		objs := []client.Object{
			policy("pool-a", "a", func(s *v1alpha1.GPUNodePolicySpec) {
				s.MaxUnavailable, s.MaxUnavailablePercent = ptr.To(int32(1+r.Intn(2))), ptr.To(int32(r.Intn(30)))
				s.GracePeriod = &metav1.Duration{Duration: time.Duration(r.Intn(90)) * time.Second}
				s.RecoveryPeriod = &metav1.Duration{Duration: time.Duration(r.Intn(120)) * time.Second}
			}),
			policy("pool-b", "b", func(s *v1alpha1.GPUNodePolicySpec) {
				s.MaxUnavailable, s.MaxUnavailablePercent = ptr.To(int32(r.Intn(3))), ptr.To(int32(r.Intn(60)))
				s.GracePeriod = &metav1.Duration{Duration: time.Duration(r.Intn(90)) * time.Second}
			}),
		}
		names := make([]string, size)
		for i := range names {
			names[i] = fmt.Sprintf("n%02d", i)
			pool := []string{"a", "b", "none"}[r.Intn(3)] // "none": governed by the flag defaults
			objs = append(objs, poolNode(names[i], pool, corev1.ConditionTrue, t0),
				pod("job-"+names[i], names[i], 8, "Job"))
		}
		h := newHarness(t, nil, objs...)
		h.r.UsePolicyCRD = true
		h.r.Policy.StaleAfter = 0 // heartbeats are not part of this model
		h.r.Policy.GracePeriod, h.r.Policy.RecoveryPeriod = 45*time.Second, 60*time.Second

		check := func(step int) {
			t.Helper()
			var nl corev1.NodeList
			if err := h.c.List(ctx, &nl); err != nil {
				t.Fatal(err)
			}
			ps, err := h.r.Resolver().List(ctx)
			if err != nil {
				t.Fatal(err)
			}
			fleets := Fleets(ps, nl.Items)
			for name, f := range fleets {
				for i := range ps {
					if ps[i].Name == name && f.Quarantined > ps[i].Policy.Budget(f.Total) {
						t.Fatalf("seed %d step %d: policy %s has %d quarantined, budget %d of %d nodes",
							seed, step, name, f.Quarantined, ps[i].Policy.Budget(f.Total), f.Total)
					}
				}
			}
			for i := range nl.Items {
				n := &nl.Items[i]
				if isQuarantinedByUs(n) != n.Spec.Unschedulable {
					t.Fatalf("seed %d step %d: %s annotated=%v but unschedulable=%v", seed, step, n.Name, isQuarantinedByUs(n), n.Spec.Unschedulable)
				}
				if isQuarantinedByUs(n) && h.podExists("job-"+n.Name) {
					t.Fatalf("seed %d step %d: quarantined node %s still runs its GPU pod", seed, step, n.Name)
				}
			}
		}

		for step := 0; step < 150; step++ {
			switch r.Intn(4) {
			case 0: // flip a node's health
				var n corev1.Node
				name := names[r.Intn(size)]
				if err := h.c.Get(ctx, types.NamespacedName{Name: name}, &n); err != nil {
					t.Fatal(err)
				}
				c := gpuCondition(&n)
				if r.Intn(2) == 0 {
					c.Status = corev1.ConditionFalse
				} else {
					c.Status = corev1.ConditionTrue
				}
				c.LastTransitionTime = metav1.NewTime(*h.now)
				if err := h.c.Status().Update(ctx, &n); err != nil {
					t.Fatal(err)
				}
			case 1:
				*h.now = h.now.Add(time.Duration(r.Intn(100)) * time.Second)
			default:
				h.reconcile(t, names[r.Intn(size)])
				ev := h.events()
				deferred += strings.Count(ev, "RemediationDeferred")
				quarantines += strings.Count(ev, "Warning Quarantined")
				check(step)
			}
		}
	}
}
