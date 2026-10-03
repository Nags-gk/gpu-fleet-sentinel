package remediation

import (
	"testing"
	"time"
)

func TestBudget(t *testing.T) {
	p := Policy{MaxUnavailable: 1, MaxUnavailablePercent: 10}
	for total, want := range map[int]int{0: 1, 5: 1, 10: 1, 25: 2, 100: 10, 1000: 100} {
		if got := p.Budget(total); got != want {
			t.Errorf("Budget(%d) = %d, want %d", total, got, want)
		}
	}
}

func TestDecide(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	p := DefaultPolicy()
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	fleet := FleetView{Total: 10}

	tests := []struct {
		name    string
		node    NodeView
		fleet   FleetView
		want    Action
		requeue time.Duration
	}{
		{"healthy untouched", NodeView{Health: HealthHealthy, Since: ago(time.Hour), Heartbeat: now}, fleet, ActionNone, 0},
		{"unknown does nothing", NodeView{Health: HealthUnknown}, fleet, ActionNone, 0},
		{"unhealthy inside grace waits", NodeView{Health: HealthUnhealthy, Since: ago(30 * time.Second), Heartbeat: now}, fleet, ActionWait, 90 * time.Second},
		{"unhealthy past grace quarantines", NodeView{Health: HealthUnhealthy, Since: ago(3 * time.Minute), Heartbeat: now}, fleet, ActionQuarantine, 0},
		{"budget exhausted defers", NodeView{Health: HealthUnhealthy, Since: ago(3 * time.Minute), Heartbeat: now}, FleetView{Total: 10, Quarantined: 1}, ActionDefer, time.Minute},
		{"already quarantined re-drains", NodeView{Health: HealthUnhealthy, Since: ago(time.Hour), Heartbeat: now, QuarantinedByUs: true}, FleetView{Total: 10, Quarantined: 5}, ActionQuarantine, 0},
		{"recovering waits", NodeView{Health: HealthHealthy, Since: ago(4 * time.Minute), Heartbeat: now, QuarantinedByUs: true}, fleet, ActionWait, 6 * time.Minute},
		{"recovered releases", NodeView{Health: HealthHealthy, Since: ago(11 * time.Minute), Heartbeat: now, QuarantinedByUs: true}, fleet, ActionRelease, 0},
		{"stale heartbeat ignored even if unhealthy", NodeView{Health: HealthUnhealthy, Since: ago(time.Hour), Heartbeat: ago(10 * time.Minute)}, fleet, ActionNone, 0},
		{"stale heartbeat never releases", NodeView{Health: HealthHealthy, Since: ago(time.Hour), Heartbeat: ago(10 * time.Minute), QuarantinedByUs: true}, fleet, ActionNone, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := p.Decide(tc.node, tc.fleet, now)
			if d.Action != tc.want {
				t.Fatalf("action = %s, want %s (reason: %s)", d.Action, tc.want, d.Reason)
			}
			if d.RequeueAfter != tc.requeue {
				t.Fatalf("requeue = %s, want %s", d.RequeueAfter, tc.requeue)
			}
			if d.Reason == "" {
				t.Fatal("every decision must carry a reason for events/logs")
			}
		})
	}
}
