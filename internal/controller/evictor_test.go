package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// Regression test: a PDB-blocked eviction (429 + Retry-After) must return
// immediately instead of being retried by client-go for ~10s per attempt.
func TestRESTEvictorDoesNotRetryPDB429(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/api/v1/namespaces/ml/pods/db-0/eviction" || r.Method != http.MethodPost {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "10")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure",` +
			`"message":"Cannot evict pod as it would violate the pod's disruption budget.",` +
			`"reason":"TooManyRequests","code":429}`))
	}))
	defer srv.Close()

	cs, err := kubernetes.NewForConfig(&rest.Config{Host: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	e := RESTEvictor{REST: cs.PolicyV1().RESTClient()}

	start := time.Now()
	err = e.Evict(context.Background(), &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "db-0", Namespace: "ml"}})
	if !apierrors.IsTooManyRequests(err) {
		t.Fatalf("want 429 error, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("eviction blocked for %s; client-side retries are not disabled", elapsed)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("server called %d times, want exactly 1", n)
	}
}
