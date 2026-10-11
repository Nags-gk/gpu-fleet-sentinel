// Command controller runs the cluster-wide GPU node remediation controller.
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	v1alpha1 "github.com/Nags-gk/gpu-fleet-sentinel/api/v1alpha1"
	"github.com/Nags-gk/gpu-fleet-sentinel/internal/api"
	"github.com/Nags-gk/gpu-fleet-sentinel/internal/controller"
	"github.com/Nags-gk/gpu-fleet-sentinel/internal/incident"
	"github.com/Nags-gk/gpu-fleet-sentinel/internal/remediation"
)

func main() {
	def := remediation.DefaultPolicy()
	var (
		metricsAddr    = flag.String("metrics-bind-address", ":8080", "Prometheus metrics address")
		probeAddr      = flag.String("health-probe-bind-address", ":8081", "liveness/readiness probe address")
		leaderElect    = flag.Bool("leader-elect", true, "enable leader election so only one replica acts")
		selector       = flag.String("node-selector", api.DefaultGPUNodeSelector, "label selector for managed GPU nodes")
		grace          = flag.Duration("grace-period", def.GracePeriod, "how long a node must stay unhealthy before quarantine")
		recovery       = flag.Duration("recovery-period", def.RecoveryPeriod, "how long a node must stay healthy before release")
		staleAfter     = flag.Duration("stale-after", def.StaleAfter, "ignore conditions whose heartbeat is older than this")
		maxUnavailable = flag.Int("max-unavailable", def.MaxUnavailable, "max nodes quarantined at once (absolute)")
		maxUnavailPct  = flag.Int("max-unavailable-percent", def.MaxUnavailablePercent, "max nodes quarantined at once (percent of fleet)")
		drainScope     = flag.String("drain-scope", string(controller.DrainGPUPods), "pods to evict: gpu or all")
		dryRun         = flag.Bool("dry-run", false, "log and emit events but never cordon or evict")
		llmURL         = flag.String("llm-base-url", "", "OpenAI-compatible base URL for incident summaries (e.g. http://ollama:11434/v1); empty uses the built-in template")
		llmModel       = flag.String("llm-model", "llama3.2", "model name for OpenAI-compatible endpoints")
		leaseNS        = flag.String("lease-namespace", os.Getenv("POD_NAMESPACE"), "namespace where agents renew heartbeat Leases (defaults to $POD_NAMESPACE); empty disables Lease heartbeats")
		enablePolicies = flag.Bool("enable-policy-crd", true, "honor GPUNodePolicy objects when the CRD is installed; the other flags become the default policy")
		azureDeploy    = flag.String("azure-openai-deployment", "", "Azure OpenAI deployment name; when set, --llm-base-url is the Azure resource endpoint")
	)
	opts := zap.Options{}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	log := ctrl.Log.WithName("setup")

	if err := run(*metricsAddr, *probeAddr, *leaderElect, *selector, *drainScope, *dryRun,
		remediation.Policy{
			GracePeriod: *grace, RecoveryPeriod: *recovery, StaleAfter: *staleAfter,
			MaxUnavailable: *maxUnavailable, MaxUnavailablePercent: *maxUnavailPct, DeferRetry: time.Minute,
		}, *llmURL, *llmModel, *azureDeploy, *leaseNS, *enablePolicies); err != nil {
		log.Error(err, "controller exited")
		os.Exit(1)
	}
}

func run(metricsAddr, probeAddr string, leaderElect bool, selector, drainScope string, dryRun bool,
	policy remediation.Policy, llmURL, llmModel, azureDeploy, leaseNS string, enablePolicies bool) error {

	sel, err := labels.Parse(selector)
	if err != nil {
		return fmt.Errorf("--node-selector: %w", err)
	}
	scope := controller.DrainScope(drainScope)
	if scope != controller.DrainGPUPods && scope != controller.DrainAllPods {
		return fmt.Errorf("--drain-scope must be gpu or all, got %q", drainScope)
	}

	var summarizer incident.Summarizer = incident.Template{}
	if llmURL != "" {
		summarizer = incident.ChatCompletions{
			BaseURL: llmURL, Model: llmModel, AzureDeployment: azureDeploy,
			APIKey: os.Getenv("LLM_API_KEY"), Fallback: incident.Template{},
		}
	}

	cfg := ctrl.GetConfigOrDie()
	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("clientset: %w", err)
	}

	cacheOpts := cache.Options{}
	if leaseNS != "" {
		// Cache only the agents' Leases. An unscoped Lease informer would also
		// hold every kubelet heartbeat Lease in the cluster, and needs
		// cluster-wide RBAC.
		cacheOpts.ByObject = map[client.Object]cache.ByObject{
			&coordinationv1.Lease{}: {Namespaces: map[string]cache.Config{leaseNS: {}}},
		}
	}
	if err := v1alpha1.AddToScheme(clientgoscheme.Scheme); err != nil {
		return err
	}
	usePolicies := enablePolicies && policyCRDInstalled(cfg)
	if enablePolicies && !usePolicies {
		ctrl.Log.WithName("setup").Info("GPUNodePolicy CRD not installed; using flags only")
	}

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Cache:                  cacheOpts,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         leaderElect,
		LeaderElectionID:       "gpu-fleet-sentinel.gpu-sentinel.io",
	})
	if err != nil {
		return fmt.Errorf("manager: %w", err)
	}

	r := &controller.NodeReconciler{
		Client:     mgr.GetClient(),
		Recorder:   mgr.GetEventRecorderFor("gpu-fleet-sentinel"),
		Policy:     policy,
		Selector:   sel,
		DrainScope: scope,
		DryRun:     dryRun,
		Summarizer: summarizer,
		Evictor:    controller.RESTEvictor{REST: clientset.PolicyV1().RESTClient()},

		LeaseNamespace: leaseNS,
		UsePolicyCRD:   usePolicies,
	}
	if err := r.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setup controller: %w", err)
	}
	if usePolicies {
		if err := (&controller.PolicyStatusReconciler{Client: mgr.GetClient(), Resolver: r.Resolver()}).SetupWithManager(mgr); err != nil {
			return fmt.Errorf("setup policy status controller: %w", err)
		}
	}
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return err
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return err
	}

	ctrl.Log.WithName("setup").Info("starting controller", "selector", selector, "dryRun", dryRun,
		"grace", policy.GracePeriod.String(), "recovery", policy.RecoveryPeriod.String(), "drainScope", drainScope)
	return mgr.Start(ctrl.SetupSignalHandler())
}

// policyCRDInstalled reports whether the API server serves GPUNodePolicy, so a
// controller upgraded without the CRD keeps working from flags alone.
func policyCRDInstalled(cfg *rest.Config) bool {
	dc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return false
	}
	rl, err := dc.ServerResourcesForGroupVersion(v1alpha1.GroupVersion.String())
	if err != nil {
		return false
	}
	for _, r := range rl.APIResources {
		if r.Kind == "GPUNodePolicy" {
			return true
		}
	}
	return false
}
