package controller

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Evictor issues a single eviction request through the Eviction API.
type Evictor interface {
	Evict(ctx context.Context, pod *corev1.Pod) error
}

// RESTEvictor posts evictions with client-side retries disabled.
//
// Why this exists: when a PodDisruptionBudget blocks an eviction the apiserver
// answers 429 with "Retry-After: 10", and client-go transparently retries such
// responses up to 10 times. Because the reconciler runs a single worker (to
// keep the disruption budget race-free), one PDB-protected pod would stall
// remediation for the whole fleet. With MaxRetries(0) the 429 returns at once,
// the pod is counted as blocked, and the node is requeued on its own.
type RESTEvictor struct {
	// REST is a policy/v1 REST client, e.g. clientset.PolicyV1().RESTClient().
	REST rest.Interface
}

// Evict posts one eviction and returns the apiserver response unchanged.
func (e RESTEvictor) Evict(ctx context.Context, pod *corev1.Pod) error {
	ev := &policyv1.Eviction{
		TypeMeta:   metav1.TypeMeta{APIVersion: "policy/v1", Kind: "Eviction"},
		ObjectMeta: metav1.ObjectMeta{Name: pod.Name, Namespace: pod.Namespace},
	}
	return e.REST.Post().
		AbsPath("/api/v1").Namespace(pod.Namespace).Resource("pods").Name(pod.Name).SubResource("eviction").
		MaxRetries(0).
		Body(ev).
		Do(ctx).
		Error()
}

// clientEvictor uses the controller-runtime client; used when no RESTEvictor is
// configured (unit tests with the fake client).
type clientEvictor struct{ c client.Client }

func (e clientEvictor) Evict(ctx context.Context, pod *corev1.Pod) error {
	ev := &policyv1.Eviction{ObjectMeta: metav1.ObjectMeta{Name: pod.Name, Namespace: pod.Namespace}}
	return e.c.SubResource("eviction").Create(ctx, pod, ev)
}
