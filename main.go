package main

import (
	"errors"
	"flag"
	"os"
	"time"

	// Import all Kubernetes client auth plugins (for example OIDC) for out-of-cluster runs.
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	"k8s.io/utils/clock"
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
	var metricsAddr, probeAddr, leaderElectionNamespace, apiAddr, apiTokenFile, bootIDFile, nodeName string
	var enableLeaderElection bool
	var pollInterval, statefulSetScaleUpDelay, staleLimit time.Duration
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "The address of the HTTP metrics endpoint, for example :8080. "+
		"The endpoint has no authentication, so keep it inside the cluster. The value 0 disables the metrics endpoint.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address of the probe endpoint.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", true,
		"Enable leader election. Only the lease holder acts on a ShutdownRequest.")
	flag.StringVar(&leaderElectionNamespace, "leader-election-namespace", "",
		"The namespace of the leader election lease. Empty means the namespace of the pod.")
	flag.DurationVar(&pollInterval, "poll-interval", 2*time.Second, "The interval between checks while pods end and while workloads become ready.")
	flag.DurationVar(&statefulSetScaleUpDelay, "statefulset-scale-up-delay", 5*time.Second,
		"The time that a StatefulSet scale-up waits after a forced delete of its pods. "+
			"After a forced delete, the container can run for at least 2 more seconds, which is the kubelet minimum.")
	flag.StringVar(&apiAddr, "api-bind-address", ":8082", "The address of the HTTP endpoint for the shutdown, cancel, and progress calls. "+
		"Empty disables the endpoint.")
	flag.StringVar(&apiTokenFile, "api-token-file", "", "The path of the file that holds the token of the HTTP endpoint, "+
		"for example from a mounted Secret. With no readable token, the endpoint refuses every call that needs a token.")
	flag.StringVar(&bootIDFile, "boot-id-file", "/proc/sys/kernel/random/boot_id",
		"The path of the file that holds the boot ID of the operator node. The operator compares it with the record to find a power cycle.")
	flag.StringVar(&nodeName, "node-name", os.Getenv("NODE_NAME"),
		"The name of the operator node, for example from the Downward API. The default is the value of NODE_NAME.")
	flag.DurationVar(&staleLimit, "stale-limit", 5*time.Minute, "The age after which a request with no phase is stale. "+
		"The operator then sets the phase Restored with the reason Stale, and changes nothing else.")
	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// The node name is the key of the own boot ID in the record. With no name, the operator cannot tell its nodes apart.
	if nodeName == "" {
		setupLog.Error(errors.New("the node name is empty"), "Set --node-name or NODE_NAME")
		os.Exit(1)
	}
	// A stale limit of 0 or less makes each new request stale, so no shutdown starts.
	if staleLimit <= 0 {
		setupLog.Error(errors.New("the stale limit is not above 0"), "Set --stale-limit to a positive duration")
		os.Exit(1)
	}

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
		PollInterval:            pollInterval,
		FieldOwner:              fieldOwner,
		StatefulSetScaleUpDelay: statefulSetScaleUpDelay,
		APIBindAddress:          apiAddr,
		APITokenFile:            apiTokenFile,
		BootIDFile:              bootIDFile,
		NodeName:                nodeName,
		StaleLimit:              staleLimit,
		Clock:                   clock.RealClock{},
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
