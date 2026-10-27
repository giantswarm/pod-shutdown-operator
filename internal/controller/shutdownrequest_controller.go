// Package controller holds the reconciler of the ShutdownRequest.
package controller

import (
	"context"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	shutdownv1alpha1 "github.com/giantswarm/pod-shutdown-operator/api/v1alpha1"
)

// ShutdownRequestReconciler acts on the ShutdownRequest. It runs only in the lease holder.
type ShutdownRequestReconciler struct {
	Client client.Client

	APIReader client.Reader

	// PollInterval is the requeue interval while pods end and while workloads become ready.
	PollInterval time.Duration

	// StatefulSetScaleUpDelay is the time that a StatefulSet scale-up waits after a forced delete of its pods.
	StatefulSetScaleUpDelay time.Duration

	// FluxControllers are the Flux controller Deployments that the operator scales to 0 before it stops an application.
	FluxControllers []types.NamespacedName

	// FluxStopTimeout is the longest time that the shutdown waits for the pods of the Flux controllers to end.
	FluxStopTimeout time.Duration

	// ScaleUpTimeout is the global time limit of the scale-up.
	ScaleUpTimeout time.Duration

	// VirtualMachines calls the stop and start subresources of KubeVirt.
	VirtualMachines VirtualMachineActions

	// BootIDFile and NodeName are for the operator node.
	BootIDFile string
	NodeName   string

	// StaleLimit is the age after which a request with no phase is stale.
	StaleLimit time.Duration

	Clock clock.PassiveClock

	bootChecked bool

	// The state in memory has no lock. One worker reconciles the request, because it is the only object.
	stop    stopState
	scaleUp scaleUpState

	// forcedDeletes holds the time of the last forced delete of a pod of each StatefulSet, by namespace/name.
	forcedDeletes map[string]time.Time
}

// +kubebuilder:rbac:groups=shutdown.giantswarm.io,resources=shutdownrequests,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups=shutdown.giantswarm.io,resources=shutdownrequests/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=shutdown.giantswarm.io,resources=managedapplications,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=deployments;statefulsets,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups=apps,resources=replicasets,verbs=get;list;watch
// +kubebuilder:rbac:groups=batch,resources=cronjobs,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups=kubevirt.io,resources=virtualmachines,verbs=get;patch
// +kubebuilder:rbac:groups=kubevirt.io,resources=virtualmachineinstances,verbs=get
// +kubebuilder:rbac:groups=subresources.kubevirt.io,resources=virtualmachines/start;virtualmachines/stop,verbs=update
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups="",resources=nodes,verbs=list
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile moves the ShutdownRequest through its phases.
func (r *ShutdownRequestReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var request shutdownv1alpha1.ShutdownRequest
	if err := r.Client.Get(ctx, req.NamespacedName, &request); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Once per process, tell a power cycle from an operator restart.
	if !r.bootChecked {
		if err := r.checkBoot(ctx, &request); err != nil {
			return ctrl.Result{}, err
		}
		r.bootChecked = true
	}

	switch {
	case request.Spec.Action == shutdownv1alpha1.ActionCancel &&
		(request.Status.Phase == shutdownv1alpha1.PhaseShuttingDown || request.Status.Phase == shutdownv1alpha1.PhaseComplete):
		return ctrl.Result{}, r.startScaleUp(ctx, &request)
	case request.Status.Phase == "" && request.Spec.Action == shutdownv1alpha1.ActionShutdown:
		// An old request must not stop the cluster after a power-on. A negative age is not stale.
		if r.Clock.Since(request.CreationTimestamp.Time) > r.StaleLimit {
			return ctrl.Result{}, r.markStale(ctx, &request)
		}
		return ctrl.Result{}, r.startShutdown(ctx, &request)
	case request.Status.Phase == shutdownv1alpha1.PhaseScalingUp && request.Spec.Action == shutdownv1alpha1.ActionShutdown:
		return ctrl.Result{}, r.reuseRecord(ctx, &request)
	case request.Status.Phase == shutdownv1alpha1.PhaseShuttingDown:
		return r.continueShutdown(ctx, &request)
	case request.Status.Phase == shutdownv1alpha1.PhaseScalingUp:
		return r.continueScaleUp(ctx, &request)
	}
	return ctrl.Result{}, nil
}

// SetupWithManager registers the reconciler with the manager.
func (r *ShutdownRequestReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&shutdownv1alpha1.ShutdownRequest{}).
		Named("shutdownrequest").
		Complete(r)
}
