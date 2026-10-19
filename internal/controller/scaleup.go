package controller

import (
	"context"
	"fmt"
	"slices"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	shutdownv1alpha1 "github.com/giantswarm/pod-shutdown-operator/api/v1alpha1"
)

// defaultReadyTimeout is the readyTimeout of an application whose ManagedApplication no longer exists.
// It is equal to the CRD default.
const defaultReadyTimeout = 5 * time.Minute

// scaleUpState is the state of one scale-up in the memory of the operator.
// The operator does not store when an application started, so an operator restart starts each readyTimeout again.
type scaleUpState struct {
	// startedAt is status.scaleUpStartedAt of the scale-up that the state is for.
	startedAt metav1.Time
	apps      map[string]*appScaleUp
}

type appScaleUp struct {
	startedAt time.Time
	ready     bool
}

// startScaleUp writes the phase ScalingUp and the start time in one status update. It changes no workload.
// One worker reconciles the request, so the stop steps of an earlier reconcile have ended.
// A late scale-down patch cannot land after the first scale-up patch.
func (r *ShutdownRequestReconciler) startScaleUp(ctx context.Context, request *shutdownv1alpha1.ShutdownRequest) error {
	now := metav1.Now()
	updated := request.DeepCopy()
	updated.Status.Phase = shutdownv1alpha1.PhaseScalingUp
	updated.Status.ScaleUpStartedAt = &now
	updated.Status.Degraded = nil
	if err := r.Client.Status().Update(ctx, updated); err != nil {
		return fmt.Errorf("start the scale-up: %w", err)
	}
	return nil
}

// continueScaleUp scales each recorded workload to its recorded count and waits for readiness.
// An application that is not ready at its readyTimeout goes into status.degraded.
// It sets the phase Restored when each application is ready or degraded.
func (r *ShutdownRequestReconciler) continueScaleUp(ctx context.Context, request *shutdownv1alpha1.ShutdownRequest) (ctrl.Result, error) {
	startedAt := ptr.Deref(request.Status.ScaleUpStartedAt, metav1.Time{})
	if r.scaleUp.apps == nil || !r.scaleUp.startedAt.Equal(&startedAt) {
		r.scaleUp = scaleUpState{startedAt: startedAt, apps: map[string]*appScaleUp{}}
	}

	degraded := slices.Clone(request.Status.Degraded)
	pending := false
	for _, app := range request.Status.RecordedState.Applications {
		key := app.Namespace + "/" + app.Name
		state, found := r.scaleUp.apps[key]
		if !found {
			state = &appScaleUp{startedAt: time.Now()}
			r.scaleUp.apps[key] = state
		}
		if state.ready || slices.Contains(degraded, key) {
			continue
		}

		ready := true
		for _, w := range app.Workloads {
			workloadReady, err := r.startWorkload(ctx, app.Namespace, w)
			if err != nil {
				return ctrl.Result{}, err
			}
			ready = ready && workloadReady
		}
		if ready {
			state.ready = true
			continue
		}

		timeout, err := r.readyTimeout(ctx, app.Namespace, app.Name)
		if err != nil {
			return ctrl.Result{}, err
		}
		if time.Since(state.startedAt) > timeout {
			degraded = append(degraded, key)
			continue
		}
		pending = true
	}

	if len(degraded) != len(request.Status.Degraded) || !pending {
		updated := request.DeepCopy()
		updated.Status.Degraded = degraded
		if !pending {
			updated.Status.Phase = shutdownv1alpha1.PhaseRestored
		}
		if err := r.Client.Status().Update(ctx, updated); err != nil {
			return ctrl.Result{}, fmt.Errorf("write the scale-up progress: %w", err)
		}
	}
	if !pending {
		return ctrl.Result{}, nil
	}
	return ctrl.Result{RequeueAfter: r.PollInterval}, nil
}

// startWorkload scales a workload to its recorded count. It returns true when the workload is ready.
// A workload that no longer exists counts as ready.
// It sends the patch at each check and reads readiness from the response, not from the cache.
// After a cancel in ShuttingDown, the cache can still hold the count before the scale to 0.
// The API server does not store a patch that changes nothing.
func (r *ShutdownRequestReconciler) startWorkload(ctx context.Context, namespace string, w shutdownv1alpha1.RecordedWorkload) (bool, error) {
	obj, ok := newScalable(w.Kind)
	if !ok {
		return true, nil
	}
	obj.SetNamespace(namespace)
	obj.SetName(w.Name)

	want := ptr.Deref(w.Replicas, 1)
	patch := client.RawPatch(types.MergePatchType, fmt.Appendf(nil, `{"spec":{"replicas":%d}}`, want))
	err := r.Client.Patch(ctx, obj, patch)
	if apierrors.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("scale %s %s/%s to %d: %w", w.Kind, namespace, w.Name, want, err)
	}
	return workloadReady(obj, want), nil
}

// readyTimeout returns the readyTimeout of an application.
func (r *ShutdownRequestReconciler) readyTimeout(ctx context.Context, namespace, name string) (time.Duration, error) {
	var app shutdownv1alpha1.ManagedApplication
	err := r.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &app)
	if apierrors.IsNotFound(err) {
		return defaultReadyTimeout, nil
	}
	if err != nil {
		return 0, fmt.Errorf("get ManagedApplication %s/%s: %w", namespace, name, err)
	}
	if app.Spec.ReadyTimeout == nil {
		return defaultReadyTimeout, nil
	}
	return app.Spec.ReadyTimeout.Duration, nil
}
