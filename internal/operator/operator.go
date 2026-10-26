package operator

import (
	"fmt"
	"net/http"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/utils/clock"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/giantswarm/pod-shutdown-operator/internal/api"
	"github.com/giantswarm/pod-shutdown-operator/internal/controller"
)

// Config holds the settings of the operator.
type Config struct {
	// Manager holds the controller-runtime manager options, for example leader election,
	// metrics, and probes. NewManager sets the scheme.
	Manager ctrl.Options

	// PollInterval is the requeue interval while pods end and while workloads become ready.
	PollInterval time.Duration

	// FieldOwner is the field manager name of each write that the operator sends.
	FieldOwner string

	// StatefulSetScaleUpDelay is the time that a StatefulSet scale-up waits after a forced delete of its pods.
	// After a forced delete, the container can still run for some seconds.
	StatefulSetScaleUpDelay time.Duration

	// FluxControllers are the Flux controller Deployments that the operator scales to 0 before it stops an application.
	FluxControllers []types.NamespacedName

	// FluxStopTimeout is the longest time that the shutdown waits for the pods of the Flux controllers to end.
	FluxStopTimeout time.Duration

	// ScaleUpTimeout is the global time limit of the scale-up. At the limit, the operator starts each remaining
	// application with no order, and scales the Flux controllers back.
	ScaleUpTimeout time.Duration

	// APIBindAddress is the address of the HTTP endpoint for the shutdown, cancel, and progress calls.
	// Empty disables the endpoint.
	APIBindAddress string

	// APITokenFile is the path of the file that holds the token of the HTTP endpoint, for example from a mounted Secret.
	APITokenFile string

	// BootIDFile and NodeName are for the operator node.
	BootIDFile string
	NodeName   string

	// StaleLimit is the age after which a request with no phase is stale.
	StaleLimit time.Duration

	// Clock gives the age of a request, so that a test can set it.
	Clock clock.PassiveClock
}

// NewManager returns a manager with the ShutdownRequest controller and the HTTP endpoint.
// With leader election, the controller runs only in the lease holder. The endpoint runs in every pod.
func NewManager(restConfig *rest.Config, c Config) (ctrl.Manager, error) {
	opts := c.Manager
	opts.Scheme = NewScheme()

	mgr, err := ctrl.NewManager(restConfig, opts)
	if err != nil {
		return nil, fmt.Errorf("create manager: %w", err)
	}

	reconciler := &controller.ShutdownRequestReconciler{
		Client:                  client.WithFieldOwner(mgr.GetClient(), c.FieldOwner),
		APIReader:               mgr.GetAPIReader(),
		PollInterval:            c.PollInterval,
		StatefulSetScaleUpDelay: c.StatefulSetScaleUpDelay,
		FluxControllers:         c.FluxControllers,
		FluxStopTimeout:         c.FluxStopTimeout,
		ScaleUpTimeout:          c.ScaleUpTimeout,
		BootIDFile:              c.BootIDFile,
		NodeName:                c.NodeName,
		StaleLimit:              c.StaleLimit,
		Clock:                   c.Clock,
	}
	if err := reconciler.SetupWithManager(mgr); err != nil {
		return nil, fmt.Errorf("set up ShutdownRequest controller: %w", err)
	}

	if c.APIBindAddress != "" {
		server := &api.Server{
			Client:    client.WithFieldOwner(mgr.GetClient(), c.FieldOwner),
			Reader:    mgr.GetAPIReader(),
			TokenFile: c.APITokenFile,
		}
		if err := mgr.Add(&manager.Server{
			Name: "api",
			// The timeouts close slow and idle connections, so that a client with no token cannot keep them open.
			Server: &http.Server{
				Addr:              c.APIBindAddress,
				Handler:           server.Handler(),
				ReadHeaderTimeout: 10 * time.Second,
				ReadTimeout:       10 * time.Second,
				WriteTimeout:      time.Minute,
				IdleTimeout:       time.Minute,
				MaxHeaderBytes:    16 << 10,
			},
			ShutdownTimeout: ptr.To(5 * time.Second),
		}); err != nil {
			return nil, fmt.Errorf("add the HTTP endpoint: %w", err)
		}
	}
	return mgr, nil
}
