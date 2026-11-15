package main

import (
	"flag"
	"os"

	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	platformv1alpha1 "github.com/mella7/project-claim/api/v1alpha1"
	"github.com/mella7/project-claim/internal/controller"
	"github.com/mella7/project-claim/internal/metrics"
)

var setupLog = ctrl.Log.WithName("setup")

// The secured metrics endpoint checks each scrape with these two reviews.
// +kubebuilder:rbac:groups=authentication.k8s.io,resources=tokenreviews,verbs=create
// +kubebuilder:rbac:groups=authorization.k8s.io,resources=subjectaccessreviews,verbs=create

func main() {
	var (
		metricsAddr    string
		probeAddr      string
		secureMetrics  bool
		leaderElection bool
		workers        int
	)
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8443", "Address the metrics endpoint binds to. 0 turns it off.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "Address the health probes bind to.")
	flag.BoolVar(&secureMetrics, "metrics-secure", true,
		"Serve metrics over HTTPS and require a token that is allowed to GET /metrics. Turn off only for local runs.")
	flag.BoolVar(&leaderElection, "leader-elect", false,
		"Run leader election, so that only one of several replicas reconciles.")
	flag.IntVar(&workers, "max-concurrent-reconciles", 3, "Number of claims reconciled in parallel.")

	zapOptions := zap.Options{}
	zapOptions.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zapOptions)))

	// The scheme maps Go types to API kinds. The client can only read and
	// write kinds that are registered here.
	scheme := clientgoscheme.Scheme
	utilruntime.Must(platformv1alpha1.AddToScheme(scheme))

	metricsOptions := metricsserver.Options{BindAddress: metricsAddr, SecureServing: secureMetrics}
	if secureMetrics {
		// Metrics give away tenant names and how projects are doing. Each
		// scrape has to present a token, which is checked with a TokenReview
		// and a SubjectAccessReview for GET /metrics.
		metricsOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Cache:                  controller.CacheOptions(),
		Metrics:                metricsOptions,
		HealthProbeBindAddress: probeAddr,
		// With leader election a second replica is a standby, not a second
		// worker. Two active copies would act on the same claims.
		LeaderElection:   leaderElection,
		LeaderElectionID: "projectclaim.platform.mella7.me",
		// Give up the lease on shutdown, so a rolling update hands over at
		// once instead of waiting for the lease to run out.
		LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		setupLog.Error(err, "creating the manager")
		os.Exit(1)
	}

	ctx := ctrl.SetupSignalHandler()

	if err := controller.SetupIndexes(ctx, mgr); err != nil {
		setupLog.Error(err, "registering field indexes")
		os.Exit(1)
	}
	if err := (&controller.ProjectClaimReconciler{
		Client:                  mgr.GetClient(),
		APIReader:               mgr.GetAPIReader(),
		Scheme:                  mgr.GetScheme(),
		Recorder:                mgr.GetEventRecorderFor(controller.ManagerName),
		Clock:                   clock.RealClock{},
		MaxConcurrentReconciles: workers,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "setting up the ProjectClaim controller")
		os.Exit(1)
	}
	if err := (&controller.TenantReconciler{
		Client:    mgr.GetClient(),
		APIReader: mgr.GetAPIReader(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "setting up the Tenant controller")
		os.Exit(1)
	}

	ctrlmetrics.Registry.MustRegister(metrics.NewStateCollector(mgr.GetCache()))

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "adding the health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "adding the readiness check")
		os.Exit(1)
	}

	setupLog.Info("starting the manager")
	// Start waits for the informer caches to fill before the first
	// Reconcile runs, and returns when the context is cancelled.
	if err := mgr.Start(ctx); err != nil {
		setupLog.Error(err, "running the manager")
		os.Exit(1)
	}
}
