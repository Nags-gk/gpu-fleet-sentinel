// Command apiserver starts a real kube-apiserver + etcd (the binaries used by
// controller-runtime's envtest) and writes an admin kubeconfig, so the scale
// benchmark can run on a laptop without Docker. It blocks until interrupted.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

func main() {
	kubeconfig := flag.String("write-kubeconfig", "scale.kubeconfig", "where to write the admin kubeconfig")
	flag.Parse()

	env := &envtest.Environment{}
	// Raise the in-flight limits so the benchmark measures the controller and
	// agents rather than the apiserver's default flow-control caps.
	env.ControlPlane.GetAPIServer().Configure().
		Set("max-requests-inflight", "2000").Set("max-mutating-requests-inflight", "1000")

	cfg, err := env.Start()
	if err != nil {
		fmt.Fprintln(os.Stderr, "start control plane:", err)
		os.Exit(1)
	}
	defer func() { _ = env.Stop() }()

	user, err := env.AddUser(envtest.User{Name: "bench", Groups: []string{"system:masters"}}, cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "add user:", err)
		os.Exit(1)
	}
	kc, err := user.KubeConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "kubeconfig:", err)
		os.Exit(1)
	}
	if err := os.WriteFile(*kubeconfig, kc, 0o600); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("control plane ready:", cfg.Host)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
}
