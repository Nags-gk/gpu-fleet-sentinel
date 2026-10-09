package controller

import (
	"context"
	"fmt"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/Nags-gk/gpu-fleet-sentinel/api/v1alpha1"
	"github.com/Nags-gk/gpu-fleet-sentinel/internal/remediation"
)

// DefaultPolicyName names the implicit policy built from the controller's
// flags. It is always last in precedence, so any GPUNodePolicy wins over it.
const DefaultPolicyName = "(flags)"

// Defaults are the flag-derived settings. Unset GPUNodePolicy fields inherit them.
type Defaults struct {
	Policy     remediation.Policy
	Selector   labels.Selector
	DrainScope DrainScope
	DryRun     bool
}

// ResolvedPolicy is one policy ready to apply: a GPUNodePolicy merged over the
// defaults, or the defaults themselves.
type ResolvedPolicy struct {
	Name       string
	CRD        bool
	Generation int64
	Policy     remediation.Policy
	Selector   labels.Selector // nil when the policy's selector could not be parsed
	DrainScope DrainScope
	DryRun     bool
	// Err is set when the policy is invalid. Nodes it matches are left alone
	// rather than handled with settings nobody asked for.
	Err error
}

// Matches reports whether the node is governed by this policy.
func (p *ResolvedPolicy) Matches(n *corev1.Node) bool {
	return p.Selector != nil && p.Selector.Matches(labels.Set(n.Labels))
}

// Resolver turns GPUNodePolicy objects into ResolvedPolicies.
type Resolver struct {
	Reader   client.Reader
	Defaults Defaults
	// UseCRD is false when the GPUNodePolicy CRD is not installed.
	UseCRD bool
}

// List returns every policy in precedence order: GPUNodePolicies by name, then
// the flag defaults.
func (rs Resolver) List(ctx context.Context) ([]ResolvedPolicy, error) {
	var out []ResolvedPolicy
	if rs.UseCRD {
		var list v1alpha1.GPUNodePolicyList
		if err := rs.Reader.List(ctx, &list); err != nil {
			return nil, fmt.Errorf("list GPUNodePolicies: %w", err)
		}
		sort.Slice(list.Items, func(i, j int) bool { return list.Items[i].Name < list.Items[j].Name })
		for i := range list.Items {
			out = append(out, fromSpec(&list.Items[i], rs.Defaults))
		}
	}
	d := rs.Defaults
	return append(out, ResolvedPolicy{
		Name: DefaultPolicyName, Policy: d.Policy, Selector: d.Selector, DrainScope: d.DrainScope, DryRun: d.DryRun,
	}), nil
}

// Resolve returns the first policy that matches the node, or nil.
func Resolve(ps []ResolvedPolicy, n *corev1.Node) *ResolvedPolicy {
	for i := range ps {
		if ps[i].Matches(n) {
			return &ps[i]
		}
	}
	return nil
}

// Fleets counts, per policy, the nodes it governs and how many it has
// quarantined. A node counts toward the first matching policy only, so
// overlapping selectors cannot double-spend a budget.
func Fleets(ps []ResolvedPolicy, nodes []corev1.Node) map[string]remediation.FleetView {
	out := make(map[string]remediation.FleetView, len(ps))
	for i := range nodes {
		rp := Resolve(ps, &nodes[i])
		if rp == nil {
			continue
		}
		f := out[rp.Name]
		f.Total++
		if isQuarantinedByUs(&nodes[i]) {
			f.Quarantined++
		}
		out[rp.Name] = f
	}
	return out
}

func fromSpec(p *v1alpha1.GPUNodePolicy, d Defaults) ResolvedPolicy {
	rp := ResolvedPolicy{
		Name: p.Name, CRD: true, Generation: p.Generation,
		Policy: d.Policy, DrainScope: d.DrainScope, DryRun: d.DryRun,
	}
	s := p.Spec
	set := func(dst *time.Duration, src *metav1.Duration) {
		if src != nil {
			*dst = src.Duration
		}
	}
	set(&rp.Policy.GracePeriod, s.GracePeriod)
	set(&rp.Policy.RecoveryPeriod, s.RecoveryPeriod)
	set(&rp.Policy.StaleAfter, s.StaleAfter)
	if s.MaxUnavailable != nil {
		rp.Policy.MaxUnavailable = int(*s.MaxUnavailable)
	}
	if s.MaxUnavailablePercent != nil {
		rp.Policy.MaxUnavailablePercent = int(*s.MaxUnavailablePercent)
	}
	if s.DrainScope != nil {
		rp.DrainScope = DrainScope(*s.DrainScope)
	}
	if s.DryRun != nil {
		rp.DryRun = *s.DryRun
	}

	sel, err := metav1.LabelSelectorAsSelector(&s.NodeSelector)
	switch {
	case err != nil:
		rp.Err = fmt.Errorf("nodeSelector: %w", err)
		return rp
	case sel.Empty():
		// Defense in depth: the CRD's CEL rule rejects this at admission.
		rp.Err = fmt.Errorf("nodeSelector is empty and would match every node")
		return rp
	}
	rp.Selector = sel

	switch {
	case rp.Policy.GracePeriod < 0 || rp.Policy.RecoveryPeriod < 0:
		rp.Err = fmt.Errorf("gracePeriod and recoveryPeriod must not be negative")
	case rp.Policy.StaleAfter <= 0:
		rp.Err = fmt.Errorf("staleAfter must be positive")
	case rp.Policy.MaxUnavailable < 0 || rp.Policy.MaxUnavailablePercent < 0 || rp.Policy.MaxUnavailablePercent > 100:
		rp.Err = fmt.Errorf("maxUnavailable must be >= 0 and maxUnavailablePercent within 0-100")
	case rp.DrainScope != DrainGPUPods && rp.DrainScope != DrainAllPods:
		rp.Err = fmt.Errorf("drainScope must be gpu or all, got %q", rp.DrainScope)
	}
	return rp
}
