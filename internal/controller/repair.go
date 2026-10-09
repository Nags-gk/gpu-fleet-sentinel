package controller

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/Nags-gk/gpu-fleet-sentinel/internal/api"
	"github.com/Nags-gk/gpu-fleet-sentinel/internal/escalation"
)

var repairActionsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "sentinel_repair_actions_total", Help: "Escalation steps by outcome.",
}, []string{"action"})

func init() { ctrlmetrics.Registry.MustRegister(repairActionsTotal) }

// RepairActuator carries out an escalation step. The controller decides when;
// an actuator decides how, so a cluster can plug in its own reboot or
// replacement mechanism (Cluster API, a cloud API, a vendor tool).
type RepairActuator interface {
	// Reboot reboots the node. It must be safe to call again for the same attempt.
	Reboot(ctx context.Context, node *corev1.Node, attempt int) error
	// RequestReplacement hands the node off for replacement. The controller has
	// already recorded the request on the node; this is the place for a webhook
	// or a call to an external system.
	RequestReplacement(ctx context.Context, node *corev1.Node) error
}

// repairCleaner is optionally implemented by actuators that leave objects
// behind (reboot pods), so release can tidy up.
type repairCleaner interface {
	Cleanup(ctx context.Context, node *corev1.Node) error
}

// AnnotationActuator asks for repair without touching the host: it sets
// gpu-sentinel.io/reboot-requested on the node for external automation (a
// reboot daemon, a Cluster API remediation, an operator's runbook) to act on.
// It needs no extra privileges, so it is the default.
type AnnotationActuator struct {
	Client client.Client
	Now    func() time.Time
}

// Reboot records the request on the node.
func (a AnnotationActuator) Reboot(ctx context.Context, node *corev1.Node, attempt int) error {
	orig := node.DeepCopy()
	if node.Annotations == nil {
		node.Annotations = map[string]string{}
	}
	node.Annotations[api.AnnotationRebootRequested] = fmt.Sprintf("attempt-%d@%s", attempt, a.Now().UTC().Format(time.RFC3339))
	return a.Client.Patch(ctx, node, client.MergeFrom(orig))
}

// RequestReplacement needs nothing beyond the annotation the controller sets.
func (AnnotationActuator) RequestReplacement(context.Context, *corev1.Node) error { return nil }

// PodActuator reboots the host from a short-lived privileged pod pinned to the
// node. It is opt-in: it lets the controller create privileged pods (in one
// namespace), which the default annotation actuator does not.
type PodActuator struct {
	Client    client.Client
	Namespace string
	Image     string
	// Command runs in the host namespaces. The default enters PID 1's
	// namespaces and runs the host's own `systemctl reboot`.
	Command []string
	Now     func() time.Time
}

// DefaultRebootCommand enters the host's namespaces from a hostPID pod.
var DefaultRebootCommand = []string{"nsenter", "--target", "1", "--mount", "--uts", "--ipc", "--net", "--pid", "--", "systemctl", "reboot"}

func (a PodActuator) podName(node string, attempt int) string {
	return fmt.Sprintf("gpu-sentinel-reboot-%s-%d", node, attempt)
}

// Reboot creates the reboot pod; AlreadyExists means this attempt already ran.
func (a PodActuator) Reboot(ctx context.Context, node *corev1.Node, attempt int) error {
	cmd := a.Command
	if len(cmd) == 0 {
		cmd = DefaultRebootCommand
	}
	privileged := true
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: a.podName(node.Name, attempt), Namespace: a.Namespace,
			Labels: map[string]string{api.LabelRebootNode: node.Name, "app.kubernetes.io/name": "gpu-fleet-sentinel-reboot"},
			// Owned by the Node, so it is garbage collected with it.
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "v1", Kind: "Node", Name: node.Name, UID: node.UID}},
		},
		Spec: corev1.PodSpec{
			NodeName:          node.Name, // bypasses the scheduler: the node is cordoned and tainted
			HostPID:           true,
			RestartPolicy:     corev1.RestartPolicyNever,
			PriorityClassName: "system-node-critical",
			Tolerations:       []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
			Containers: []corev1.Container{{
				Name: "reboot", Image: a.Image, Command: cmd,
				SecurityContext: &corev1.SecurityContext{Privileged: &privileged},
			}},
		},
	}
	if err := a.Client.Create(ctx, pod); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create reboot pod: %w", err)
	}
	return nil
}

// RequestReplacement needs nothing beyond the annotation the controller sets.
func (PodActuator) RequestReplacement(context.Context, *corev1.Node) error { return nil }

// Cleanup deletes this node's reboot pods.
func (a PodActuator) Cleanup(ctx context.Context, node *corev1.Node) error {
	return a.Client.DeleteAllOf(ctx, &corev1.Pod{}, client.InNamespace(a.Namespace),
		client.MatchingLabels{api.LabelRebootNode: node.Name})
}

// repairState reads the persisted repair history from node annotations.
func repairState(n *corev1.Node) escalation.NodeState {
	st := escalation.NodeState{}
	if t, err := time.Parse(time.RFC3339, n.Annotations[api.AnnotationQuarantinedAt]); err == nil {
		st.QuarantinedAt = t
	}
	if v, err := strconv.Atoi(n.Annotations[api.AnnotationRepairAttempts]); err == nil && v > 0 {
		st.Attempts = v
	}
	if t, err := time.Parse(time.RFC3339, n.Annotations[api.AnnotationLastRepairAt]); err == nil {
		st.LastAttempt = t
	}
	_, st.ReplacementRequested = n.Annotations[api.AnnotationReplacementRequested]
	return st
}

// escalate runs after a drain left nothing blocked. It returns when to look at
// the node again (0 = no timer needed).
func (r *NodeReconciler) escalate(ctx context.Context, node *corev1.Node, rp *ResolvedPolicy) (time.Duration, error) {
	if rp.Escalation == nil || r.Actuator == nil {
		return 0, nil
	}
	logger := log.FromContext(ctx).WithValues("node", node.Name, "policy", rp.Name)
	now := r.now()
	spec := *rp.Escalation

	d := spec.Decide(repairState(node), 0, now)
	switch d.Action {
	case escalation.Wait:
		return d.RequeueAfter, nil
	case escalation.None:
		return 0, nil

	case escalation.Replace:
		orig := node.DeepCopy()
		node.Annotations[api.AnnotationReplacementRequested] = now.UTC().Format(time.RFC3339)
		if err := r.Patch(ctx, node, client.MergeFromWithOptions(orig, client.MergeFromWithOptimisticLock{})); err != nil {
			return 0, fmt.Errorf("record replacement request on %s: %w", node.Name, err)
		}
		if err := r.Actuator.RequestReplacement(ctx, node); err != nil {
			repairActionsTotal.WithLabelValues("error").Inc()
			return 0, fmt.Errorf("request replacement of %s: %w", node.Name, err)
		}
		repairActionsTotal.WithLabelValues("replacement_requested").Inc()
		logger.Info("replacement requested", "reason", d.Reason)
		r.Recorder.Eventf(node, corev1.EventTypeWarning, "ReplacementRequested", "Node needs replacement: %s", d.Reason)
		return 0, nil
	}

	// Reboot: every pod must already be gone. A GPU-only drain leaves CPU pods
	// running, and a reboot would kill them without a PDB check, so evict
	// everything first and wait for it to finish terminating.
	_, blocked, err := r.drain(ctx, node, DrainAllPods)
	if err != nil {
		return 0, err
	}
	remaining, err := r.workloadPods(ctx, node)
	if err != nil {
		return 0, err
	}
	if blocked > 0 || remaining > 0 {
		repairActionsTotal.WithLabelValues("reboot_deferred").Inc()
		r.Recorder.Eventf(node, corev1.EventTypeNormal, "RebootDeferred",
			"%d pod(s) still on the node (%d blocked by a PodDisruptionBudget); not rebooting yet", remaining, blocked)
		return 15 * time.Second, nil
	}

	inFlight, err := r.rebootsInFlight(ctx, node, rp)
	if err != nil {
		return 0, err
	}
	if d = spec.Decide(repairState(node), inFlight, now); d.Action != escalation.Reboot {
		return d.RequeueAfter, nil
	}

	// Record the attempt before acting. If the controller dies in between, the
	// attempt is lost rather than repeated, which keeps reboots bounded.
	attempt := spec.EffectiveAttempts(repairState(node), now) + 1
	orig := node.DeepCopy()
	node.Annotations[api.AnnotationRepairAttempts] = strconv.Itoa(attempt)
	node.Annotations[api.AnnotationLastRepairAt] = now.UTC().Format(time.RFC3339)
	if err := r.Patch(ctx, node, client.MergeFromWithOptions(orig, client.MergeFromWithOptimisticLock{})); err != nil {
		return 0, fmt.Errorf("record reboot attempt on %s: %w", node.Name, err)
	}
	if err := r.Actuator.Reboot(ctx, node, attempt); err != nil {
		repairActionsTotal.WithLabelValues("error").Inc()
		r.Recorder.Eventf(node, corev1.EventTypeWarning, "RebootFailed", "Reboot attempt %d failed: %v", attempt, err)
		return spec.Cooldown, nil // the attempt counts; retry after the cooldown
	}
	repairActionsTotal.WithLabelValues("reboot").Inc()
	logger.Info("reboot requested", "attempt", attempt, "of", spec.MaxAttempts)
	r.Recorder.Eventf(node, corev1.EventTypeWarning, "RebootRequested", "Reboot attempt %d of %d: %s", attempt, spec.MaxAttempts, d.Reason)
	return spec.Cooldown, nil
}

// workloadPods counts pods a reboot would disturb: everything except
// DaemonSet and mirror pods and pods that already finished. Terminating pods
// count; they are still running until their grace period ends.
func (r *NodeReconciler) workloadPods(ctx context.Context, node *corev1.Node) (int, error) {
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.MatchingFields{PodNodeNameIndex: node.Name}); err != nil {
		return 0, fmt.Errorf("list pods on %s: %w", node.Name, err)
	}
	n := 0
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		if _, mirror := p.Annotations[corev1.MirrorPodAnnotationKey]; mirror {
			continue
		}
		daemon := false
		for _, ref := range p.OwnerReferences {
			daemon = daemon || ref.Kind == "DaemonSet"
		}
		if !daemon && p.Labels[api.LabelRebootNode] == "" {
			n++
		}
	}
	return n, nil
}

// rebootsInFlight counts the policy's other nodes that are mid-reboot.
func (r *NodeReconciler) rebootsInFlight(ctx context.Context, self *corev1.Node, rp *ResolvedPolicy) (int, error) {
	policies, err := r.Resolver().List(ctx)
	if err != nil {
		return 0, err
	}
	var nodes corev1.NodeList
	if err := r.List(ctx, &nodes); err != nil {
		return 0, fmt.Errorf("list nodes: %w", err)
	}
	now, n := r.now(), 0
	for i := range nodes.Items {
		o := &nodes.Items[i]
		if o.Name == self.Name {
			continue
		}
		if got := Resolve(policies, o); got != nil && got.Name == rp.Name && rp.Escalation.InFlight(repairState(o), now) {
			n++
		}
	}
	return n, nil
}
