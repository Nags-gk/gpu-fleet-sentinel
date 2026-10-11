package controller

import (
	"context"
	"fmt"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1alpha1 "github.com/Nags-gk/gpu-fleet-sentinel/api/v1alpha1"
	"github.com/Nags-gk/gpu-fleet-sentinel/internal/remediation"
)

const maxStatusNodeNames = 50

// PolicyStatusReconciler keeps GPUNodePolicy.status current so that
// `kubectl get gpunodepolicies` shows fleet health and budget use. It only
// reads nodes; remediation stays in NodeReconciler.
type PolicyStatusReconciler struct {
	client.Client
	Resolver Resolver
}

// +kubebuilder:rbac:groups=gpu-sentinel.io,resources=gpunodepolicies/status,verbs=get;update;patch

// Reconcile recomputes one policy's status and writes it only if it changed.
func (r *PolicyStatusReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var pol v1alpha1.GPUNodePolicy
	if err := r.Get(ctx, req.NamespacedName, &pol); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	policies, err := r.Resolver.List(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}
	var rp *ResolvedPolicy
	for i := range policies {
		if policies[i].Name == pol.Name && policies[i].CRD {
			rp = &policies[i]
		}
	}
	if rp == nil {
		return ctrl.Result{}, nil
	}
	var nodes corev1.NodeList
	if err := r.List(ctx, &nodes); err != nil {
		return ctrl.Result{}, fmt.Errorf("list nodes: %w", err)
	}

	orig := pol.DeepCopy()
	pol.Status = computeStatus(&pol, rp, policies, nodes.Items, time.Now())
	if !apiequality.Semantic.DeepEqual(orig.Status, pol.Status) {
		// Update, not a merge patch: a patch omits zero-valued counts, which the
		// CRD requires, so the first write of an idle policy would be rejected.
		if err := r.Status().Update(ctx, &pol); err != nil {
			return ctrl.Result{}, fmt.Errorf("update status of %s: %w", pol.Name, err)
		}
	}
	// Node events drive updates; this covers anything they miss.
	return ctrl.Result{RequeueAfter: 2 * time.Minute}, nil
}

func computeStatus(pol *v1alpha1.GPUNodePolicy, rp *ResolvedPolicy, all []ResolvedPolicy, nodes []corev1.Node, now time.Time) v1alpha1.GPUNodePolicyStatus {
	st := v1alpha1.GPUNodePolicyStatus{ObservedGeneration: pol.Generation, Conditions: pol.Status.Conditions}
	var waiting int32
	for i := range nodes {
		n := &nodes[i]
		if got := Resolve(all, n); got == nil || got.Name != rp.Name {
			continue
		}
		st.ManagedNodes++
		v := nodeView(n)
		switch v.Health {
		case remediation.HealthHealthy:
			st.HealthyNodes++
		case remediation.HealthUnhealthy:
			st.UnhealthyNodes++
			if !v.QuarantinedByUs {
				waiting++
			}
		}
		if rs := repairState(n); rs.ReplacementRequested {
			st.ReplacementRequestedNodes++
		} else if rp.Escalation != nil && rp.Escalation.InFlight(rs, now) {
			st.RepairingNodes++
		}
		if v.QuarantinedByUs {
			st.QuarantinedNodes++
			st.QuarantinedNodeNames = append(st.QuarantinedNodeNames, n.Name)
		}
	}
	sort.Strings(st.QuarantinedNodeNames)
	if len(st.QuarantinedNodeNames) > maxStatusNodeNames {
		st.QuarantinedNodeNames = st.QuarantinedNodeNames[:maxStatusNodeNames]
	}
	st.Budget = int32(rp.Policy.Budget(int(st.ManagedNodes)))

	ready := metav1.Condition{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "Enforcing",
		Message: "policy is valid and enforced", ObservedGeneration: pol.Generation}
	switch {
	case rp.Err != nil:
		ready.Status, ready.Reason, ready.Message = metav1.ConditionFalse, "Invalid", rp.Err.Error()
	case rp.DryRun:
		ready.Reason, ready.Message = "DryRun", "policy is valid; remediation is only logged, never applied"
	}
	meta.SetStatusCondition(&st.Conditions, ready)

	budget := metav1.Condition{Type: v1alpha1.ConditionBudgetExhausted, Status: metav1.ConditionFalse, Reason: "WithinBudget",
		Message: fmt.Sprintf("%d of %d allowed quarantines in use", st.QuarantinedNodes, st.Budget), ObservedGeneration: pol.Generation}
	if rp.Err == nil && st.QuarantinedNodes >= st.Budget && waiting > 0 {
		budget.Status, budget.Reason = metav1.ConditionTrue, "BudgetExhausted"
		budget.Message = fmt.Sprintf("%d unhealthy node(s) are waiting; %d of %d allowed quarantines in use", waiting, st.QuarantinedNodes, st.Budget)
	}
	meta.SetStatusCondition(&st.Conditions, budget)
	return st
}

// SetupWithManager wires the status controller.
func (r *PolicyStatusReconciler) SetupWithManager(mgr ctrl.Manager) error {
	allPolicies := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, _ client.Object) []reconcile.Request {
		var l v1alpha1.GPUNodePolicyList
		if err := r.List(ctx, &l); err != nil {
			return nil
		}
		reqs := make([]reconcile.Request, 0, len(l.Items))
		for i := range l.Items {
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&l.Items[i])})
		}
		return reqs
	})
	return ctrl.NewControllerManagedBy(mgr).
		Named("gpu-policy-status").
		// Spec changes only; our own status writes must not retrigger us.
		For(&v1alpha1.GPUNodePolicy{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&corev1.Node{}, allPolicies, builder.WithPredicates(gpuStateChanged())).
		Complete(r)
}
