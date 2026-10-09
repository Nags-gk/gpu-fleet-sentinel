package remediation

import (
	"fmt"
	"math/rand"
	"testing"
	"time"
)

// These tests generate thousands of random policies, nodes and fleets and check
// the invariants that make automated remediation safe. They are deterministic:
// every case derives from a seed, which is printed on failure.

const propertyCases = 20000

func randPolicy(r *rand.Rand) Policy {
	d := func(max time.Duration) time.Duration { return time.Duration(r.Int63n(int64(max))) }
	p := Policy{
		GracePeriod:           d(5 * time.Minute),
		RecoveryPeriod:        d(15 * time.Minute),
		MaxUnavailable:        r.Intn(6),
		MaxUnavailablePercent: r.Intn(101),
		DeferRetry:            time.Second + d(2*time.Minute),
	}
	if r.Intn(4) > 0 { // 0 disables the staleness check
		p.StaleAfter = time.Second + d(10*time.Minute)
	}
	return p
}

func randNode(r *rand.Rand, now time.Time) NodeView {
	n := NodeView{Name: "n", Health: Health(r.Intn(3)), QuarantinedByUs: r.Intn(2) == 0}
	// Since may be slightly in the future: node and controller clocks disagree.
	n.Since = now.Add(time.Duration(r.Int63n(int64(time.Hour))) - 10*time.Minute)
	if r.Intn(5) > 0 {
		n.Heartbeat = now.Add(-time.Duration(r.Int63n(int64(20 * time.Minute))))
	}
	return n
}

func TestDecideInvariants(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	for seed := int64(0); seed < propertyCases; seed++ {
		r := rand.New(rand.NewSource(seed))
		p, n := randPolicy(r), randNode(r, now)
		f := FleetView{Total: r.Intn(2000)}
		f.Quarantined = r.Intn(f.Total + 1)
		d := p.Decide(n, f, now)
		fail := func(format string, a ...any) {
			t.Helper()
			t.Fatalf("seed %d: %s\n policy=%+v\n node=%+v\n fleet=%+v\n decision=%+v", seed, fmt.Sprintf(format, a...), p, n, f, d)
		}

		stale := p.StaleAfter > 0 && !n.Heartbeat.IsZero() && now.Sub(n.Heartbeat) > p.StaleAfter
		elapsed := now.Sub(n.Since)

		if stale && d.Action != ActionNone {
			fail("acted on a stale heartbeat")
		}
		if n.Health == HealthUnknown && d.Action != ActionNone {
			fail("acted on unknown health")
		}
		if (d.Action == ActionWait || d.Action == ActionDefer) && d.RequeueAfter <= 0 {
			fail("%s without a positive requeue would stall the node forever", d.Action)
		}
		if d.Action == ActionNone && d.RequeueAfter != 0 {
			fail("None must not requeue")
		}
		if d.Reason == "" {
			fail("decision without a reason")
		}
		// A skewed clock must not produce an unbounded wait.
		if d.RequeueAfter > max(p.GracePeriod, p.RecoveryPeriod, p.DeferRetry) {
			fail("requeue %v exceeds every configured period", d.RequeueAfter)
		}

		switch n.Health {
		case HealthHealthy:
			if d.Action == ActionQuarantine || d.Action == ActionDefer {
				fail("healthy node must never be quarantined or deferred")
			}
			if d.Action == ActionRelease && (!n.QuarantinedByUs || elapsed < p.RecoveryPeriod) {
				fail("released a node that was not ours or has not recovered long enough")
			}
			if !stale && n.QuarantinedByUs && elapsed >= p.RecoveryPeriod && d.Action != ActionRelease {
				fail("recovered node was not released")
			}
			if !n.QuarantinedByUs && d.Action != ActionNone {
				fail("healthy, un-quarantined node needs no action")
			}
		case HealthUnhealthy:
			if d.Action == ActionRelease || d.Action == ActionNone && !stale {
				fail("unhealthy node got %s", d.Action)
			}
			if d.Action == ActionQuarantine && !n.QuarantinedByUs {
				if elapsed < p.GracePeriod {
					fail("quarantined before the grace period")
				}
				if f.Quarantined >= p.Budget(f.Total) {
					fail("quarantined past the disruption budget")
				}
			}
			if d.Action == ActionDefer && (n.QuarantinedByUs || f.Quarantined < p.Budget(f.Total) || elapsed < p.GracePeriod) {
				fail("deferred without an exhausted budget")
			}
			if !stale && !n.QuarantinedByUs && elapsed >= p.GracePeriod && f.Quarantined < p.Budget(f.Total) && d.Action != ActionQuarantine {
				fail("eligible node was not quarantined despite a free slot")
			}
		}
	}
}

func TestBudgetProperties(t *testing.T) {
	for seed := int64(0); seed < propertyCases; seed++ {
		r := rand.New(rand.NewSource(seed))
		p := randPolicy(r)
		prev := 0
		for total := 0; total <= 300; total++ {
			b := p.Budget(total)
			if b < p.MaxUnavailable {
				t.Fatalf("seed %d: budget(%d)=%d below the absolute floor %d", seed, total, b, p.MaxUnavailable)
			}
			if b < prev {
				t.Fatalf("seed %d: budget shrank from %d to %d as the fleet grew to %d", seed, prev, b, total)
			}
			if pct := total * p.MaxUnavailablePercent / 100; b > max(p.MaxUnavailable, pct) {
				t.Fatalf("seed %d: budget(%d)=%d exceeds max(abs, pct)=%d", seed, total, b, max(p.MaxUnavailable, pct))
			}
			prev = b
		}
	}
}

// TestFleetNeverExceedsBudget drives a whole fleet through random health flips,
// clock jumps and serial reconciles using the real Decide, and checks the
// safety property the controller exists to guarantee: no more nodes are ever
// quarantined than the budget allows, and nothing is left stuck at the end.
func TestFleetNeverExceedsBudget(t *testing.T) {
	for seed := int64(0); seed < 1000; seed++ {
		r := rand.New(rand.NewSource(seed))
		p := randPolicy(r)
		p.StaleAfter = 0 // heartbeats are always fresh in this model
		size := 1 + r.Intn(60)
		now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)

		type node struct {
			health Health
			since  time.Time
			q      bool
		}
		nodes := make([]node, size)
		for i := range nodes {
			nodes[i] = node{health: HealthHealthy, since: now}
		}
		count := func() (q int) {
			for _, n := range nodes {
				if n.q {
					q++
				}
			}
			return
		}
		reconcile := func() {
			for _, i := range r.Perm(size) {
				n := &nodes[i]
				d := p.Decide(NodeView{Health: n.health, Since: n.since, Heartbeat: now, QuarantinedByUs: n.q},
					FleetView{Total: size, Quarantined: count()}, now)
				switch d.Action {
				case ActionQuarantine:
					n.q = true
				case ActionRelease:
					n.q = false
				}
				if q := count(); q > p.Budget(size) {
					t.Fatalf("seed %d: %d nodes quarantined, budget %d (fleet %d, policy %+v)", seed, q, p.Budget(size), size, p)
				}
			}
		}

		for step := 0; step < 200; step++ {
			switch r.Intn(3) {
			case 0: // a node changes health
				i := r.Intn(size)
				h := HealthHealthy
				if r.Intn(2) == 0 {
					h = HealthUnhealthy
				}
				if nodes[i].health != h {
					nodes[i].health, nodes[i].since = h, now
				}
			case 1:
				now = now.Add(time.Duration(r.Int63n(int64(3 * time.Minute))))
			case 2:
				reconcile()
			}
		}

		// Liveness: with no further changes, enough time and reconciles must
		// quarantine up to the budget and release every recovered node.
		for i := 0; i < 4; i++ {
			now = now.Add(p.GracePeriod + p.RecoveryPeriod + p.DeferRetry + time.Minute)
			reconcile()
		}
		unhealthy, quarantinedHealthy := 0, 0
		for _, n := range nodes {
			if n.health == HealthUnhealthy {
				unhealthy++
			} else if n.q {
				quarantinedHealthy++
			}
		}
		if quarantinedHealthy > 0 {
			t.Fatalf("seed %d: %d recovered node(s) never released", seed, quarantinedHealthy)
		}
		if want := min(unhealthy, p.Budget(size)); count() < want {
			t.Fatalf("seed %d: only %d quarantined; %d unhealthy and budget %d should have allowed %d", seed, count(), unhealthy, p.Budget(size), want)
		}
	}
}
