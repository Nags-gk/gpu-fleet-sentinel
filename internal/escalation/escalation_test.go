package escalation

import (
	"math/rand"
	"testing"
	"time"
)

var now = time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)

func TestDecide(t *testing.T) {
	s := DefaultSpec()
	q := func(ago time.Duration) NodeState { return NodeState{QuarantinedAt: now.Add(-ago)} }
	for _, tc := range []struct {
		name     string
		n        NodeState
		inFlight int
		want     Action
		requeue  time.Duration
	}{
		{"not quarantined", NodeState{}, 0, None, 0},
		{"too early", q(5 * time.Minute), 0, Wait, 10 * time.Minute},
		{"first reboot", q(20 * time.Minute), 0, Reboot, 0},
		{"concurrency limit", q(20 * time.Minute), 1, Wait, time.Minute},
		{"cooldown after attempt", NodeState{QuarantinedAt: now.Add(-time.Hour), Attempts: 1, LastAttempt: now.Add(-5 * time.Minute)}, 0, Wait, 15 * time.Minute},
		{"second reboot", NodeState{QuarantinedAt: now.Add(-time.Hour), Attempts: 1, LastAttempt: now.Add(-25 * time.Minute)}, 0, Reboot, 0},
		{"exhausted -> replace", NodeState{QuarantinedAt: now.Add(-time.Hour), Attempts: 2, LastAttempt: now.Add(-25 * time.Minute)}, 0, Replace, 0},
		{"already handed off", NodeState{QuarantinedAt: now.Add(-time.Hour), Attempts: 2, ReplacementRequested: true}, 0, None, 0},
		{"old attempts expire", NodeState{QuarantinedAt: now.Add(-time.Hour), Attempts: 2, LastAttempt: now.Add(-48 * time.Hour)}, 0, Reboot, 0},
	} {
		d := s.Decide(tc.n, tc.inFlight, now)
		if d.Action != tc.want || d.RequeueAfter != tc.requeue {
			t.Errorf("%s: got %+v, want %s requeue %v", tc.name, d, tc.want, tc.requeue)
		}
	}
	rep := s
	rep.Replace = true
	if d := rep.Decide(q(time.Hour), 0, now); d.Action != Replace {
		t.Errorf("replace-only spec should never reboot, got %+v", d)
	}
}

func randSpec(r *rand.Rand) Spec {
	d := func(max time.Duration) time.Duration { return time.Second + time.Duration(r.Int63n(int64(max))) }
	return Spec{
		After: d(30 * time.Minute), Replace: r.Intn(5) == 0, MaxAttempts: 1 + r.Intn(4),
		Cooldown: d(40 * time.Minute), MaxConcurrent: 1 + r.Intn(3),
		HistoryWindow: d(48 * time.Hour), ConcurrencyRetry: d(5 * time.Minute),
	}
}

func TestDecideInvariants(t *testing.T) {
	for seed := int64(0); seed < 20000; seed++ {
		r := rand.New(rand.NewSource(seed))
		s := randSpec(r)
		n := NodeState{QuarantinedAt: now.Add(time.Duration(r.Int63n(int64(3*time.Hour))) - 10*time.Minute),
			Attempts: r.Intn(6), ReplacementRequested: r.Intn(6) == 0}
		if r.Intn(3) > 0 {
			n.LastAttempt = now.Add(-time.Duration(r.Int63n(int64(72 * time.Hour))))
		}
		inFlight := r.Intn(5)
		d := s.Decide(n, inFlight, now)
		fail := func(msg string) {
			t.Helper()
			t.Fatalf("seed %d: %s\n spec=%+v\n node=%+v inFlight=%d\n decision=%+v", seed, msg, s, n, inFlight, d)
		}
		attempts := s.EffectiveAttempts(n, now)
		q := max(now.Sub(n.QuarantinedAt), 0)

		if d.Reason == "" {
			fail("no reason")
		}
		if d.Action == Wait && (d.RequeueAfter <= 0 || d.RequeueAfter > max(s.After, s.Cooldown, s.ConcurrencyRetry)) {
			fail("Wait needs a positive requeue within a configured period")
		}
		if d.Action != Wait && d.RequeueAfter != 0 {
			fail("only Wait may requeue")
		}
		if (n.ReplacementRequested || n.QuarantinedAt.IsZero()) && d.Action != None {
			fail("acted on a handed-off or unquarantined node")
		}
		if (d.Action == Reboot || d.Action == Replace) && q < s.After {
			fail("escalated before the quarantine duration")
		}
		if d.Action == Reboot {
			if s.Replace {
				fail("replace-only spec rebooted")
			}
			if attempts >= s.MaxAttempts {
				fail("rebooted past MaxAttempts")
			}
			if inFlight >= s.MaxConcurrent {
				fail("rebooted past the concurrency cap")
			}
			if attempts > 0 && max(now.Sub(n.LastAttempt), 0) < s.Cooldown {
				fail("rebooted inside the cooldown")
			}
		}
		if d.Action == Replace && !s.Replace && attempts < s.MaxAttempts {
			fail("requested replacement while reboot attempts remain")
		}
	}
}

// Simulate a node that never recovers: reboots must stop at MaxAttempts per
// history window, spaced by at least the cooldown, then hand off exactly once.
func TestPersistentFaultIsBoundedAndHandedOff(t *testing.T) {
	for seed := int64(0); seed < 2000; seed++ {
		r := rand.New(rand.NewSource(seed))
		s := randSpec(r)
		s.Replace = false
		s.HistoryWindow = max(s.HistoryWindow, 2*s.Cooldown*time.Duration(s.MaxAttempts+1)) // window outlives the whole sequence
		n := NodeState{QuarantinedAt: now}
		t0 := now
		clock := now
		var reboots []time.Time
		replaced := 0
		for step := 0; step < 400; step++ {
			clock = clock.Add(time.Duration(1 + r.Int63n(int64(10*time.Minute))))
			switch d := s.Decide(n, 0, clock); d.Action {
			case Reboot:
				reboots = append(reboots, clock)
				n.Attempts = s.EffectiveAttempts(n, clock) + 1
				n.LastAttempt = clock
			case Replace:
				replaced++
				n.ReplacementRequested = true
			}
		}
		if len(reboots) != s.MaxAttempts {
			t.Fatalf("seed %d: %d reboots, want exactly MaxAttempts=%d (spec %+v)", seed, len(reboots), s.MaxAttempts, s)
		}
		for i := 1; i < len(reboots); i++ {
			if reboots[i].Sub(reboots[i-1]) < s.Cooldown {
				t.Fatalf("seed %d: reboots %d and %d only %v apart, cooldown %v", seed, i-1, i, reboots[i].Sub(reboots[i-1]), s.Cooldown)
			}
		}
		if reboots[0].Sub(t0) < s.After {
			t.Fatalf("seed %d: first reboot after %v, before After=%v", seed, reboots[0].Sub(t0), s.After)
		}
		if replaced != 1 {
			t.Fatalf("seed %d: replacement requested %d times, want once", seed, replaced)
		}
	}
}

func TestInFlight(t *testing.T) {
	s := DefaultSpec()
	if s.InFlight(NodeState{}, now) {
		t.Error("no attempt is not in flight")
	}
	if !s.InFlight(NodeState{LastAttempt: now.Add(-time.Minute)}, now) {
		t.Error("recent attempt is in flight")
	}
	if s.InFlight(NodeState{LastAttempt: now.Add(-time.Hour)}, now) {
		t.Error("attempt older than cooldown is finished")
	}
	if s.InFlight(NodeState{LastAttempt: now.Add(-time.Minute), ReplacementRequested: true}, now) {
		t.Error("handed-off node is not in flight")
	}
}
