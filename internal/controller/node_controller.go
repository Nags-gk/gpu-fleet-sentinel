// Package controller contains the cluster-wide remediation controller. It
// watches the GPUHealthy condition written by the node agents and quarantines
// unhealthy nodes (cordon + taint + drain) within a fleet disruption budget.
package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/Nags-gk/gpu-fleet-sentinel/internal/api"
	"github.com/Nags-gk/gpu-fleet-sentinel/internal/incident"
	"github.com/Nags-gk/gpu-fleet-sentinel/internal/remediation"
)

// PodNodeNameIndex is the field index used to list pods on one node.
const PodNodeNameIndex = "spec.nodeName"

// DrainScope selects which pods are evicted from a quarantined node.
type DrainScope string

const (
	// DrainGPUPods evicts only pods requesting nvidia.com/gpu.
	DrainGPUPods DrainScope = "gpu"
	// DrainAllPods evicts every pod except DaemonSet and mirror pods.
	DrainAllPods DrainScope = "all"
)

var (
	decisionsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "sentinel_remediation_decisions_total", Help: "Remediation decisions by action.",
	}, []string{"action"})
	quarantinedNodes = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "sentinel_quarantined_nodes", Help: "GPU nodes currently quarantined by the controller.",
	})
	managedNodes = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "sentinel_managed_nodes", Help: "GPU nodes matched by the controller's selector.",
	})
	evictionsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "sentinel_evictions_total", Help: "Pod eviction attempts by result.",
	}, []string{"result"})
	timeToQuarantine = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "sentinel_time_to_quarantine_seconds",
		Help:    "Seconds from the GPUHealthy=False transition to the node being cordoned.",
		Buckets: []float64{15, 30, 60, 120, 180, 300, 600, 1200},
	})
)

func init() {
	ctrlmetrics.Registry.MustRegister(decisionsTotal, quarantinedNodes, managedNodes, evictionsTotal, timeToQuarantine)
}

// NodeReconciler reconciles one Node at a time.
type NodeReconciler struct {
	client.Client
	Recorder   record.EventRecorder
	Policy     remediation.Policy
	Selector   labels.Selector
	DrainScope DrainScope
	DryRun     bool
	Summarizer incident.Summarizer
	// Evictor sends evictions; nil falls back to the controller-runtime client.
	Evictor Evictor
	Now     func() time.Time
}

// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods/eviction,verbs=create
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile evaluates one node and applies the policy decision.
func (r *NodeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("node", req.Name)
	now := r.now()

	var node corev1.Node
	if err := r.Get(ctx, req.NamespacedName, &node); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !r.Selector.Matches(labels.Set(node.Labels)) {
		return ctrl.Result{}, nil
	}

	fleet, err := r.fleetView(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}
	view := nodeView(&node)
	d := r.Policy.Decide(view, fleet, now)
	decisionsTotal.WithLabelValues(string(d.Action)).Inc()
	logger.V(1).Info("decision", "action", d.Action, "reason", d.Reason, "health", view.Health)

	switch d.Action {
	case remediation.ActionWait:
		return ctrl.Result{RequeueAfter: d.RequeueAfter}, nil

	case remediation.ActionDefer:
		r.Recorder.Event(&node, corev1.EventTypeWarning, "RemediationDeferred", d.Reason)
		return ctrl.Result{RequeueAfter: d.RequeueAfter}, nil

	case remediation.ActionQuarantine:
		if r.DryRun {
			r.Recorder.Event(&node, corev1.EventTypeNormal, "DryRunQuarantine", conditionMessage(&node))
			return ctrl.Result{}, nil
		}
		return r.quarantine(ctx, &node, view)

	case remediation.ActionRelease:
		if r.DryRun {
			r.Recorder.Event(&node, corev1.EventTypeNormal, "DryRunRelease", d.Reason)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, r.release(ctx, &node)
	}
	return ctrl.Result{}, nil
}

func (r *NodeReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *NodeReconciler) fleetView(ctx context.Context) (remediation.FleetView, error) {
	var nodes corev1.NodeList
	if err := r.List(ctx, &nodes, client.MatchingLabelsSelector{Selector: r.Selector}); err != nil {
		return remediation.FleetView{}, fmt.Errorf("list GPU nodes: %w", err)
	}
	f := remediation.FleetView{Total: len(nodes.Items)}
	for i := range nodes.Items {
		if isQuarantinedByUs(&nodes.Items[i]) {
			f.Quarantined++
		}
	}
	managedNodes.Set(float64(f.Total))
	quarantinedNodes.Set(float64(f.Quarantined))
	return f, nil
}

func nodeView(n *corev1.Node) remediation.NodeView {
	v := remediation.NodeView{Name: n.Name, Health: remediation.HealthUnknown, QuarantinedByUs: isQuarantinedByUs(n)}
	if c := gpuCondition(n); c != nil {
		switch c.Status {
		case corev1.ConditionTrue:
			v.Health = remediation.HealthHealthy
		case corev1.ConditionFalse:
			v.Health = remediation.HealthUnhealthy
		}
		v.Since = c.LastTransitionTime.Time
		v.Heartbeat = c.LastHeartbeatTime.Time
	}
	return v
}

func gpuCondition(n *corev1.Node) *corev1.NodeCondition {
	for i := range n.Status.Conditions {
		if n.Status.Conditions[i].Type == api.ConditionGPUHealthy {
			return &n.Status.Conditions[i]
		}
	}
	return nil
}

func conditionMessage(n *corev1.Node) string {
	if c := gpuCondition(n); c != nil {
		return c.Message
	}
	return ""
}

func isQuarantinedByUs(n *corev1.Node) bool {
	_, ok := n.Annotations[api.AnnotationQuarantinedAt]
	return ok
}

// quarantine cordons, taints and drains the node. Every step is idempotent,
// so the controller can crash at any point and safely redo the work.
func (r *NodeReconciler) quarantine(ctx context.Context, node *corev1.Node, view remediation.NodeView) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("node", node.Name)
	firstTime := !view.QuarantinedByUs

	if firstTime {
		orig := node.DeepCopy()
		if node.Annotations == nil {
			node.Annotations = map[string]string{}
		}
		if node.Spec.Unschedulable {
			node.Annotations[api.AnnotationWasUnschedulable] = "true"
		}
		node.Spec.Unschedulable = true
		if !hasTaint(node) {
			added := metav1.NewTime(r.now())
			node.Spec.Taints = append(node.Spec.Taints, corev1.Taint{
				Key: api.TaintKey, Value: "true", Effect: corev1.TaintEffectNoSchedule, TimeAdded: &added,
			})
		}
		node.Annotations[api.AnnotationQuarantinedAt] = r.now().UTC().Format(time.RFC3339)
		node.Annotations[api.AnnotationReason] = truncate(conditionMessage(node), 512)
		// Optimistic locking: if anything else changed the node since we read
		// it, the patch fails with a conflict and we retry on fresh data
		// instead of overwriting someone else's taints.
		if err := r.Patch(ctx, node, client.MergeFromWithOptions(orig, client.MergeFromWithOptimisticLock{})); err != nil {
			return ctrl.Result{}, fmt.Errorf("cordon %s: %w", node.Name, err)
		}
		if !view.Since.IsZero() {
			timeToQuarantine.Observe(r.now().Sub(view.Since).Seconds())
		}
		logger.Info("node quarantined", "reason", conditionMessage(node))
		r.Recorder.Event(node, corev1.EventTypeWarning, "Quarantined", "GPU unhealthy: "+conditionMessage(node))
	}

	evicted, blocked, err := r.drain(ctx, node)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Keyed on the annotation rather than "first pass" so a summary is still
	// written if an earlier pass failed midway through the drain.
	if r.Summarizer != nil && node.Annotations[api.AnnotationIncidentSummary] == "" {
		r.annotateIncident(ctx, node, evicted, blocked)
	}

	if blocked > 0 {
		r.Recorder.Eventf(node, corev1.EventTypeWarning, "EvictionBlocked",
			"%d pod(s) protected by PodDisruptionBudget; retrying", blocked)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	return ctrl.Result{}, nil
}

// drain evicts pods through the Eviction API so PodDisruptionBudgets are honored.
func (r *NodeReconciler) drain(ctx context.Context, node *corev1.Node) (evicted, blocked int, err error) {
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.MatchingFields{PodNodeNameIndex: node.Name}); err != nil {
		return 0, 0, fmt.Errorf("list pods on %s: %w", node.Name, err)
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		if !r.shouldEvict(p) {
			continue
		}
		switch err := r.evictor().Evict(ctx, p); {
		case err == nil || apierrors.IsNotFound(err):
			evicted++
			evictionsTotal.WithLabelValues("evicted").Inc()
		case apierrors.IsTooManyRequests(err):
			blocked++
			evictionsTotal.WithLabelValues("blocked_by_pdb").Inc()
		default:
			evictionsTotal.WithLabelValues("error").Inc()
			return evicted, blocked, fmt.Errorf("evict %s/%s: %w", p.Namespace, p.Name, err)
		}
	}
	return evicted, blocked, nil
}

func (r *NodeReconciler) evictor() Evictor {
	if r.Evictor != nil {
		return r.Evictor
	}
	return clientEvictor{c: r.Client}
}

func (r *NodeReconciler) shouldEvict(p *corev1.Pod) bool {
	if p.DeletionTimestamp != nil || p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
		return false
	}
	if _, mirror := p.Annotations[corev1.MirrorPodAnnotationKey]; mirror {
		return false
	}
	for _, ref := range p.OwnerReferences {
		if ref.Kind == "DaemonSet" {
			return false // DaemonSet pods (incl. this agent) are recreated on the node anyway
		}
	}
	if r.DrainScope == DrainAllPods {
		return true
	}
	return requestsGPU(p)
}

func requestsGPU(p *corev1.Pod) bool {
	check := func(cs []corev1.Container) bool {
		for _, c := range cs {
			if q, ok := c.Resources.Limits[api.GPUResource]; ok && !q.IsZero() {
				return true
			}
			if q, ok := c.Resources.Requests[api.GPUResource]; ok && !q.IsZero() {
				return true
			}
		}
		return false
	}
	return check(p.Spec.Containers) || check(p.Spec.InitContainers)
}

func (r *NodeReconciler) annotateIncident(ctx context.Context, node *corev1.Node, evicted, blocked int) {
	logger := log.FromContext(ctx).WithValues("node", node.Name)
	sctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	summary, err := r.Summarizer.Summarize(sctx, incident.Incident{
		Node: node.Name, GPUModel: node.Labels["nvidia.com/gpu.product"],
		Message: conditionMessage(node), Evicted: evicted, Blocked: blocked,
	})
	if err != nil {
		logger.Error(err, "incident summary failed; continuing without it")
		return
	}
	orig := node.DeepCopy()
	node.Annotations[api.AnnotationIncidentSummary] = truncate(summary, 1000)
	if err := r.Patch(ctx, node, client.MergeFrom(orig)); err != nil {
		logger.Error(err, "could not store incident summary")
		return
	}
	r.Recorder.Event(node, corev1.EventTypeNormal, "IncidentSummary", truncate(summary, 1000))
}

// release undoes only what quarantine did; a node a human had already cordoned
// stays cordoned.
func (r *NodeReconciler) release(ctx context.Context, node *corev1.Node) error {
	orig := node.DeepCopy()
	taints := node.Spec.Taints[:0:0]
	for _, t := range node.Spec.Taints {
		if t.Key != api.TaintKey {
			taints = append(taints, t)
		}
	}
	node.Spec.Taints = taints
	if node.Annotations[api.AnnotationWasUnschedulable] != "true" {
		node.Spec.Unschedulable = false
	}
	for _, k := range []string{api.AnnotationQuarantinedAt, api.AnnotationReason,
		api.AnnotationWasUnschedulable, api.AnnotationIncidentSummary} {
		delete(node.Annotations, k)
	}
	if err := r.Patch(ctx, node, client.MergeFromWithOptions(orig, client.MergeFromWithOptimisticLock{})); err != nil {
		return fmt.Errorf("release %s: %w", node.Name, err)
	}
	log.FromContext(ctx).Info("node released", "node", node.Name)
	r.Recorder.Event(node, corev1.EventTypeNormal, "Released", "GPU healthy for the full recovery period")
	return nil
}

func hasTaint(n *corev1.Node) bool {
	for _, t := range n.Spec.Taints {
		if t.Key == api.TaintKey {
			return true
		}
	}
	return false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-3] + "..."
}

// SetupWithManager wires the controller. MaxConcurrentReconciles is 1 on
// purpose: decisions read fleet-wide state (the disruption budget), so
// serializing them prevents two nodes from both claiming the last slot.
func (r *NodeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &corev1.Pod{}, PodNodeNameIndex,
		func(o client.Object) []string { return []string{o.(*corev1.Pod).Spec.NodeName} }); err != nil {
		return err
	}
	selectorPred := predicate.NewPredicateFuncs(func(o client.Object) bool {
		return r.Selector.Matches(labels.Set(o.GetLabels()))
	})
	return ctrl.NewControllerManagedBy(mgr).
		Named("gpu-node-remediation").
		For(&corev1.Node{}, builder.WithPredicates(selectorPred, gpuStateChanged())).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Complete(r)
}

// gpuStateChanged skips the constant stream of heartbeat-only node updates;
// timers are handled with RequeueAfter instead.
func gpuStateChanged() predicate.Predicate {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldN, ok1 := e.ObjectOld.(*corev1.Node)
			newN, ok2 := e.ObjectNew.(*corev1.Node)
			if !ok1 || !ok2 {
				return true
			}
			oc, nc := gpuCondition(oldN), gpuCondition(newN)
			if (oc == nil) != (nc == nil) {
				return true
			}
			if oc != nil && (oc.Status != nc.Status || oc.Message != nc.Message) {
				return true
			}
			return oldN.Spec.Unschedulable != newN.Spec.Unschedulable ||
				isQuarantinedByUs(oldN) != isQuarantinedByUs(newN)
		},
	}
}
