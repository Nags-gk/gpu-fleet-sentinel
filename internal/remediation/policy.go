// Package remediation decides what to do with a GPU node given its health and
// the state of the rest of the fleet. It is pure logic with no Kubernetes
// client so every branch can be unit-tested deterministically.
package remediation

import (
	"fmt"
	"math"
	"time"
)

// Action is what the controller should do next for a node.
type Action string

const (
	// ActionNone means leave the node alone.
	ActionNone Action = "None"
	// ActionWait means re-check after RequeueAfter (grace/recovery timers).
	ActionWait Action = "Wait"
	// ActionQuarantine means cordon, taint and drain GPU workloads. Idempotent.
	ActionQuarantine Action = "Quarantine"
	// ActionDefer means the node should be quarantined but the fleet-wide
	// disruption budget is exhausted; retry later.
	ActionDefer Action = "Defer"
	// ActionRelease means undo a quarantine this controller applied.
	ActionRelease Action = "Release"
)

// Policy holds the knobs that make automated remediation safe.
type Policy struct {
	// GracePeriod an unhealthy condition must persist before quarantine.
	GracePeriod time.Duration
	// RecoveryPeriod a healthy condition must persist before release.
	RecoveryPeriod time.Duration
	// StaleAfter: if the agent has not refreshed the condition for this long,
	// health is treated as unknown and no action is taken.
	StaleAfter time.Duration
	// MaxUnavailable caps how many nodes may be quarantined at once.
	// The larger of the absolute count and the percentage is used.
	MaxUnavailable        int
	MaxUnavailablePercent int
	// DeferRetry is how long to wait before re-checking a deferred node.
	DeferRetry time.Duration
}

// DefaultPolicy is a conservative starting point.
func DefaultPolicy() Policy {
	return Policy{
		GracePeriod:           2 * time.Minute,
		RecoveryPeriod:        10 * time.Minute,
		StaleAfter:            5 * time.Minute,
		MaxUnavailable:        1,
		MaxUnavailablePercent: 10,
		DeferRetry:            time.Minute,
	}
}

// Health is the agent-reported GPU health of a node.
type Health int

const (
	// HealthUnknown means no fresh condition is available.
	HealthUnknown Health = iota
	// HealthHealthy means the agent reports GPUHealthy=True.
	HealthHealthy
	// HealthUnhealthy means the agent reports GPUHealthy=False.
	HealthUnhealthy
)

func (h Health) String() string {
	return [...]string{"Unknown", "Healthy", "Unhealthy"}[h]
}

// NodeView is the slice of node state the policy needs.
type NodeView struct {
	Name            string
	Health          Health
	Since           time.Time // when Health last changed
	Heartbeat       time.Time // when the agent last refreshed the condition
	QuarantinedByUs bool
}

// FleetView summarizes the GPU nodes this controller manages.
type FleetView struct {
	Total       int
	Quarantined int // nodes currently quarantined by this controller
}

// Decision is the policy output.
type Decision struct {
	Action       Action
	RequeueAfter time.Duration
	Reason       string
}

// Budget returns how many nodes may be quarantined at once for a fleet size.
func (p Policy) Budget(total int) int {
	pct := int(math.Floor(float64(total) * float64(p.MaxUnavailablePercent) / 100))
	return max(p.MaxUnavailable, pct)
}

// since is the time elapsed since t, never negative. The transition time is
// written with the node's clock, so skew can place it in the future; without
// the clamp a timer would wait longer than its configured period.
func since(now, t time.Time) time.Duration { return max(now.Sub(t), 0) }

// Decide is the single source of truth for remediation behavior.
func (p Policy) Decide(n NodeView, f FleetView, now time.Time) Decision {
	if p.StaleAfter > 0 && !n.Heartbeat.IsZero() && now.Sub(n.Heartbeat) > p.StaleAfter {
		// A dead agent is not evidence of a bad GPU; never act on stale data.
		return Decision{Action: ActionNone, Reason: fmt.Sprintf("agent heartbeat stale for %s", now.Sub(n.Heartbeat).Round(time.Second))}
	}

	switch n.Health {
	case HealthUnhealthy:
		if n.QuarantinedByUs {
			return Decision{Action: ActionQuarantine, Reason: "already quarantined; ensuring drain is complete"}
		}
		if elapsed := since(now, n.Since); elapsed < p.GracePeriod {
			return Decision{Action: ActionWait, RequeueAfter: p.GracePeriod - elapsed,
				Reason: fmt.Sprintf("unhealthy for %s, grace period %s", elapsed.Round(time.Second), p.GracePeriod)}
		}
		if budget := p.Budget(f.Total); f.Quarantined >= budget {
			return Decision{Action: ActionDefer, RequeueAfter: p.DeferRetry,
				Reason: fmt.Sprintf("disruption budget exhausted (%d/%d nodes quarantined)", f.Quarantined, budget)}
		}
		return Decision{Action: ActionQuarantine, Reason: "GPU unhealthy beyond grace period"}

	case HealthHealthy:
		if !n.QuarantinedByUs {
			return Decision{Action: ActionNone, Reason: "healthy"}
		}
		if elapsed := since(now, n.Since); elapsed < p.RecoveryPeriod {
			return Decision{Action: ActionWait, RequeueAfter: p.RecoveryPeriod - elapsed,
				Reason: fmt.Sprintf("healthy for %s, recovery period %s", elapsed.Round(time.Second), p.RecoveryPeriod)}
		}
		return Decision{Action: ActionRelease, Reason: "GPU healthy beyond recovery period"}

	default:
		return Decision{Action: ActionNone, Reason: "health unknown"}
	}
}
