package controller

import (
	"context"
	"time"

	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"

	shutdownv1alpha1 "github.com/giantswarm/pod-shutdown-operator/api/v1alpha1"
)

// recordFlux reads the replicas of each configured Flux controller Deployment.
// It skips a Flux controller that does not exist.
func (r *ShutdownRequestReconciler) recordFlux(ctx context.Context) ([]shutdownv1alpha1.RecordedFluxController, error) {
	var record []shutdownv1alpha1.RecordedFluxController
	for _, c := range r.FluxControllers {
		ref := shutdownv1alpha1.WorkloadReference{Kind: shutdownv1alpha1.WorkloadKindDeployment, Name: c.Name}
		w, found, err := r.recordWorkload(ctx, c.Namespace, ref)
		if err != nil {
			return nil, err
		}
		if found {
			record = append(record, shutdownv1alpha1.RecordedFluxController{Namespace: c.Namespace, Name: c.Name, Replicas: *w.Replicas})
		}
	}
	return record, nil
}

// stopFlux scales the recorded Flux controllers to 0. It returns true when no pod of them remains,
// or when the time limit has passed. It does not delete the pods, and an error does not stop the shutdown.
func (r *ShutdownRequestReconciler) stopFlux(ctx context.Context, controllers []shutdownv1alpha1.RecordedFluxController, limit time.Time) bool {
	log := ctrl.LoggerFrom(ctx)
	stopped := true
	for _, c := range controllers {
		w := shutdownv1alpha1.RecordedWorkload{Kind: shutdownv1alpha1.WorkloadKindDeployment, Name: c.Name}
		ended, err := r.stopWorkload(ctx, c.Namespace, w, false)
		// The next poll tries again, up to the time limit.
		if err != nil {
			log.Error(err, "Failed to stop the Flux controller", "controller", appKey(c.Namespace, c.Name))
		}
		stopped = stopped && ended && err == nil
	}
	if stopped {
		return true
	}
	// A slow Flux stop uses only a bounded part of the budget.
	if !time.Now().Before(limit) {
		log.Info("The Flux time limit passed before Flux stopped, stops the applications")
		return true
	}
	return false
}

// startFlux scales the recorded Flux controllers back to their recorded replicas. It does not wait for readiness.
func (r *ShutdownRequestReconciler) startFlux(ctx context.Context, controllers []shutdownv1alpha1.RecordedFluxController) error {
	for _, c := range controllers {
		w := shutdownv1alpha1.RecordedWorkload{Kind: shutdownv1alpha1.WorkloadKindDeployment, Name: c.Name, Replicas: ptr.To(c.Replicas)}
		if _, err := r.startWorkload(ctx, c.Namespace, w); err != nil {
			return err
		}
	}
	return nil
}
