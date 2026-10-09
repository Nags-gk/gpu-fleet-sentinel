// Package escalation decides when a quarantined node that has not recovered on
// its own should be rebooted, and when to give up and ask for a replacement. It
// is pure logic, like package remediation, so every branch is unit- and
// property-tested without a cluster.
//
// A reboot is the first-line fix for most GPU faults because it resets the
// driver state that a critical XID or uncorrectable-ECC counter latches until
// reset. Reboots are deliberately rare: bounded attempts per history window, a
// cooldown between them, and a cap on how many run at once.
package escalation

import (
	"fmt"
	"time"
)

// Action is what to do next for one quarantined, still-unhealthy node.
type Action string

const (
	// None means no escalation is configured or the node is already handed off.
	None Action = "None"
	// Wait means come back after RequeueAfter (timers, cooldown, concurrency).
	Wait Action = "Wait"
	// Reboot means reboot the node now.
	Reboot Action = "Reboot"
	// Replace means reboots are exhausted (or not wanted): ask for a replacement.
	Replace Action = "Replace"
)

// Spec configures escalation for one policy.
type Spec struct {
	// After is how long a node must stay quarantined before the first action.
	After time.Duration
	// Replace skips reboots and goes straight to requesting a replacement.
	Replace bool
	// MaxAttempts is how many reboots to try inside HistoryWindow before asking
	// for a replacement.
	MaxAttempts int
	// Cooldown is the minimum time between two attempts, long enough for the
	// node to come back and the agent to report.
	Cooldown time.Duration
	// MaxConcurrent caps reboots in flight across the policy's nodes.
	MaxConcurrent int
	// HistoryWindow: attempts older than this no longer count against MaxAttempts.
	HistoryWindow time.Duration
	// ConcurrencyRetry is how long to wait when MaxConcurrent is reached.
	ConcurrencyRetry time.Duration
}

// DefaultSpec returns conservative values for fields a policy leaves unset.
func DefaultSpec() Spec {
	return Spec{
		After: 15 * time.Minute, MaxAttempts: 2, Cooldown: 20 * time.Minute,
		MaxConcurrent: 1, HistoryWindow: 24 * time.Hour, ConcurrencyRetry: time.Minute,
	}
}

// NodeState is the repair history persisted on the node.
type NodeState struct {
	QuarantinedAt        time.Time
	Attempts             int       // reboots recorded inside the history window
	LastAttempt          time.Time // zero if none
	ReplacementRequested bool
}

// Decision is the output of Decide.
type Decision struct {
	Action       Action
	RequeueAfter time.Duration
	Reason       string
}

// EffectiveAttempts is the attempt count that still matters: if the last
// attempt is older than the history window the node gets a fresh budget.
func (s Spec) EffectiveAttempts(n NodeState, now time.Time) int {
	if n.LastAttempt.IsZero() || now.Sub(n.LastAttempt) > s.HistoryWindow {
		return 0
	}
	return n.Attempts
}

// Decide chooses the next step. inFlight is how many other nodes of the same
// policy are mid-reboot (an attempt newer than the cooldown).
func (s Spec) Decide(n NodeState, inFlight int, now time.Time) Decision {
	if n.ReplacementRequested {
		return Decision{Action: None, Reason: "replacement already requested"}
	}
	if n.QuarantinedAt.IsZero() {
		return Decision{Action: None, Reason: "node is not quarantined"}
	}
	// The annotation is written with this controller's clock, but a restored
	// backup or a manual edit could put it in the future; never wait longer
	// than configured.
	if q := max(now.Sub(n.QuarantinedAt), 0); q < s.After {
		return Decision{Action: Wait, RequeueAfter: s.After - q,
			Reason: fmt.Sprintf("quarantined for %s, escalating after %s", q.Round(time.Second), s.After)}
	}
	attempts := s.EffectiveAttempts(n, now)
	if attempts > 0 {
		if since := max(now.Sub(n.LastAttempt), 0); since < s.Cooldown {
			return Decision{Action: Wait, RequeueAfter: s.Cooldown - since,
				Reason: fmt.Sprintf("attempt %d was %s ago, cooldown %s", attempts, since.Round(time.Second), s.Cooldown)}
		}
	}
	if s.Replace || attempts >= s.MaxAttempts {
		return Decision{Action: Replace, Reason: fmt.Sprintf("%d reboot attempt(s) did not recover the node", attempts)}
	}
	if inFlight >= s.MaxConcurrent {
		return Decision{Action: Wait, RequeueAfter: s.ConcurrencyRetry,
			Reason: fmt.Sprintf("%d of %d allowed reboots already in flight", inFlight, s.MaxConcurrent)}
	}
	return Decision{Action: Reboot, Reason: fmt.Sprintf("still unhealthy after quarantine; reboot attempt %d of %d", attempts+1, s.MaxAttempts)}
}

// InFlight reports whether a node counts as mid-reboot: it has an attempt
// recorded within the cooldown and has not been handed off for replacement.
func (s Spec) InFlight(n NodeState, now time.Time) bool {
	return !n.ReplacementRequested && !n.LastAttempt.IsZero() && max(now.Sub(n.LastAttempt), 0) < s.Cooldown
}
