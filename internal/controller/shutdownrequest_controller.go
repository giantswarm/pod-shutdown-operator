// Package controller holds the reconciler of the ShutdownRequest.
package controller

import (
	"context"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	shutdownv1alpha1 "github.com/giantswarm/pod-shutdown-operator/api/v1alpha1"
)

// ShutdownRequestReconciler acts on the ShutdownRequest. It runs only in the lease holder.
type ShutdownRequestReconciler struct {
	Client client.Client

	// PollInterval is the requeue interval while pods end.
	PollInterval time.Duration
}

// +kubebuilder:rbac:groups=shutdown.giantswarm.io,resources=shutdownrequests,verbs=get;list;watch
// +kubebuilder:rbac:groups=shutdown.giantswarm.io,resources=shutdownrequests/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=shutdown.giantswarm.io,resources=managedapplications,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=deployments;statefulsets,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile moves the ShutdownRequest through its phases.
func (r *ShutdownRequestReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var request shutdownv1alpha1.ShutdownRequest
	if err := r.Client.Get(ctx, req.NamespacedName, &request); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if request.Status.Phase == "" && request.Spec.Action == shutdownv1alpha1.ActionShutdown {
		return ctrl.Result{}, r.startShutdown(ctx, &request)
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
