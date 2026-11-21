// Command tender runs the operator.
package main

import (
	"flag"
	"os"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	cloudv1alpha1 "github.com/paguos/tender-operator/api/v1alpha1"
	"github.com/paguos/tender-operator/internal/cloud/gcp"
	"github.com/paguos/tender-operator/internal/controller"
)

// Leader election (--leader-elect) needs leases.
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch;delete

// version is set at build time (-ldflags "-X main.version=...").
var version = "dev"

func main() {
	var (
		project     = flag.String("project", os.Getenv("TENDER_PROJECT"), "Google Cloud project for buckets and service accounts (env TENDER_PROJECT)")
		prefix      = flag.String("name-prefix", envOr("TENDER_NAME_PREFIX", "tender"), "prefix for generated bucket names and service-account IDs (env TENDER_NAME_PREFIX)")
		resync      = flag.Duration("resync", 10*time.Minute, "how often ready resources are checked for drift")
		metricsAddr = flag.String("metrics-bind-address", "0", "metrics address, e.g. :8080 (0 disables)")
		probeAddr   = flag.String("health-probe-bind-address", ":8081", "health probe address")
		leaderElect = flag.Bool("leader-elect", false, "enable leader election (only needed with more than one replica)")
	)
	opts := zap.Options{}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	log := ctrl.Log.WithName("setup")

	if *project == "" {
		log.Info("--project (or TENDER_PROJECT) is required")
		os.Exit(2)
	}

	scheme := runtime.NewScheme()
	// Only the built-in types tender uses, which keeps the binary smaller.
	must(corev1.AddToScheme(scheme))
	must(cloudv1alpha1.AddToScheme(scheme))

	managed := labels.SelectorFromSet(labels.Set{controller.ManagedByLabel: controller.ManagedByValue})
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: *metricsAddr},
		HealthProbeBindAddress: *probeAddr,
		LeaderElection:         *leaderElect,
		LeaderElectionID:       "tender.cloud.paguos.dev",
		// Only cache the Secrets tender writes, not every Secret in the cluster.
		Cache: cache.Options{ByObject: map[client.Object]cache.ByObject{
			&corev1.Secret{}: {Label: managed},
		}},
	})
	if err != nil {
		log.Error(err, "create manager")
		os.Exit(1)
	}

	ctx := ctrl.SetupSignalHandler()
	provider, err := gcp.New(ctx, *project)
	if err != nil {
		log.Error(err, "create GCP clients")
		os.Exit(1)
	}

	if err := (&controller.BucketReconciler{
		Client:  mgr.GetClient(),
		Scheme:  mgr.GetScheme(),
		Cloud:   provider,
		Project: *project,
		Prefix:  *prefix,
		Resync:  *resync,
	}).SetupWithManager(mgr); err != nil {
		log.Error(err, "set up bucket controller")
		os.Exit(1)
	}

	must(mgr.AddHealthzCheck("healthz", healthz.Ping))
	must(mgr.AddReadyzCheck("readyz", healthz.Ping))

	log.Info("starting", "version", version, "project", *project, "prefix", *prefix)
	if err := mgr.Start(ctx); err != nil {
		log.Error(err, "manager stopped")
		os.Exit(1)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func must(err error) {
	if err != nil {
		ctrl.Log.Error(err, "setup")
		os.Exit(1)
	}
}
