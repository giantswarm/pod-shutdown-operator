package main

import (
	"flag"
	"os"
	"time"

	// Import all Kubernetes client auth plugins (for example OIDC) for out-of-cluster runs.
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/giantswarm/pod-shutdown-operator/internal/operator"
)

const (
	leaderElectionID = "pod-shutdown-operator.shutdown.giantswarm.io"
	fieldOwner       = "pod-shutdown-operator"
)

var setupLog = ctrl.Log.WithName("setup")

func main() {
	var metricsAddr, probeAddr, leaderElectionNamespace string
	var enableLeaderElection bool
	var pollInterval time.Duration
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "The address of the HTTP metrics endpoint, for example :8080. "+
		"The endpoint has no authentication, so keep it inside the cluster. The value 0 disables the metrics endpoint.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address of the probe endpoint.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", true,
		"Enable leader election. Only the lease holder acts on a ShutdownRequest.")
	flag.StringVar(&leaderElectionNamespace, "leader-election-namespace", "",
		"The namespace of the leader election lease. Empty means the namespace of the pod.")
	flag.DurationVar(&pollInterval, "poll-interval", 2*time.Second, "The interval between checks while pods end and while workloads become ready.")
	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	mgr, err := operator.NewManager(ctrl.GetConfigOrDie(), operator.Config{
		Manager: ctrl.Options{
			Metrics:                 metricsserver.Options{BindAddress: metricsAddr},
			HealthProbeBindAddress:  probeAddr,
			LeaderElection:          enableLeaderElection,
			LeaderElectionID:        leaderElectionID,
			LeaderElectionNamespace: leaderElectionNamespace,
			// The binary ends at once when the manager stops, so a voluntary lease release is safe.
			// It makes the hand-over to a new leader faster.
			LeaderElectionReleaseOnCancel: true,
		},
		PollInterval: pollInterval,
		FieldOwner:   fieldOwner,
	})
	if err != nil {
		setupLog.Error(err, "Failed to create the manager")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up the health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up the ready check")
		os.Exit(1)
	}

	setupLog.Info("Starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "Failed to run the manager")
		os.Exit(1)
	}
}
