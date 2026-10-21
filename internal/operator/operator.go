package operator

import (
	"fmt"
	"time"

	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

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
}

// NewManager returns a manager with the ShutdownRequest controller.
// With leader election, the controller runs only in the lease holder.
func NewManager(restConfig *rest.Config, c Config) (ctrl.Manager, error) {
	opts := c.Manager
	opts.Scheme = NewScheme()

	mgr, err := ctrl.NewManager(restConfig, opts)
	if err != nil {
		return nil, fmt.Errorf("create manager: %w", err)
	}

	reconciler := &controller.ShutdownRequestReconciler{
		Client:                  client.WithFieldOwner(mgr.GetClient(), c.FieldOwner),
		PollInterval:            c.PollInterval,
		StatefulSetScaleUpDelay: c.StatefulSetScaleUpDelay,
	}
	if err := reconciler.SetupWithManager(mgr); err != nil {
		return nil, fmt.Errorf("set up ShutdownRequest controller: %w", err)
	}
	return mgr, nil
}
