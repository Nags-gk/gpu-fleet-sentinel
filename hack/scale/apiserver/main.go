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
	if err := run(*kubeconfig); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// run is separate from main so the deferred env.Stop always executes.
func run(kubeconfig string) error {
	env := &envtest.Environment{}
	// Raise the in-flight limits so the benchmark measures the controller and
	// agents rather than the apiserver's default flow-control caps.
	env.ControlPlane.GetAPIServer().Configure().
		Set("max-requests-inflight", "2000").Set("max-mutating-requests-inflight", "1000")

	cfg, err := env.Start()
	if err != nil {
		return fmt.Errorf("start control plane: %w", err)
	}
	defer func() { _ = env.Stop() }()

	user, err := env.AddUser(envtest.User{Name: "bench", Groups: []string{"system:masters"}}, cfg)
	if err != nil {
		return fmt.Errorf("add user: %w", err)
	}
	kc, err := user.KubeConfig()
	if err != nil {
		return fmt.Errorf("kubeconfig: %w", err)
	}
	if err := os.WriteFile(kubeconfig, kc, 0o600); err != nil {
		return err
	}
	fmt.Println("control plane ready:", cfg.Host)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	return nil
}
