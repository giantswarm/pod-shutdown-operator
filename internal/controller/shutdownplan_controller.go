package controller

import (
	"context"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	shutdownv1alpha1 "github.com/giantswarm/pod-shutdown-operator/api/v1alpha1"
)

// ShutdownPlanReconciler writes the plan of a ShutdownPlan once and deletes it after TTL. It writes no other object.
type ShutdownPlanReconciler struct {
	Client          client.Client
	FluxControllers []types.NamespacedName

	// FluxStopTimeout sets the start of the groups in the plan.
	FluxStopTimeout time.Duration

	// TTL is the age after which the operator deletes a ShutdownPlan.
	TTL time.Duration

	Clock clock.PassiveClock
}

// +kubebuilder:rbac:groups=shutdown.giantswarm.io,resources=shutdownplans,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups=shutdown.giantswarm.io,resources=shutdownplans/status,verbs=get;update;patch

// Reconcile writes the plan of a new ShutdownPlan and deletes an old one.
func (r *ShutdownPlanReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var plan shutdownv1alpha1.ShutdownPlan
	if err := r.Client.Get(ctx, req.NamespacedName, &plan); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	age := r.Clock.Since(plan.CreationTimestamp.Time)
	if age >= r.TTL {
		// The UID precondition keeps a new ShutdownPlan with the same name.
		err := r.Client.Delete(ctx, &plan, client.Preconditions{UID: &plan.UID})
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// The status is a snapshot, so the operator writes it once.
	if plan.Status.ComputedAt == nil {
		p := planner{reader: r.Client, fluxControllers: r.FluxControllers}
		status, err := p.plan(ctx, r.FluxStopTimeout, r.Clock.Now())
		if err != nil {
			return ctrl.Result{}, err
		}
		updated := plan.DeepCopy()
		updated.Status = status
		if err := r.Client.Status().Update(ctx, updated); err != nil {
			return ctrl.Result{}, fmt.Errorf("write the plan: %w", err)
		}
	}
	return ctrl.Result{RequeueAfter: r.TTL - age}, nil
}

// SetupWithManager registers the reconciler with the manager.
func (r *ShutdownPlanReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&shutdownv1alpha1.ShutdownPlan{}).
		Named("shutdownplan").
		Complete(r)
}
