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

// continueScaleUp scales the recorded workloads to their recorded counts in dependency order and waits
// for readiness. An application starts when all its dependencies are ready or degraded, so each
// dependency group runs by itself. Its readyTimeout starts when it starts. An application that is
// not ready at its readyTimeout goes into status.degraded.
// When each application is ready or degraded, it scales Flux back and sets the phase Restored.
func (r *ShutdownRequestReconciler) continueScaleUp(ctx context.Context, request *shutdownv1alpha1.ShutdownRequest) (ctrl.Result, error) {
	startedAt := ptr.Deref(request.Status.ScaleUpStartedAt, metav1.Time{})
	if r.scaleUp.apps == nil || !r.scaleUp.startedAt.Equal(&startedAt) {
		r.scaleUp = scaleUpState{startedAt: startedAt, apps: map[string]*appScaleUp{}}
	}
	// The global time limit comes from the stored start time, so an operator restart does not start it again.
	pastLimit := time.Since(startedAt.Time) >= r.ScaleUpTimeout

	apps := request.Status.RecordedState.Applications
	deps, err := r.dependencies(ctx, apps)
	if err != nil {
		return ctrl.Result{}, err
	}
	deps = cyclesAsSteps(deps)

	degraded := slices.Clone(request.Status.Degraded)
	// done holds the applications that are ready or degraded.
	done := map[string]bool{}
	for _, key := range degraded {
		done[key] = true
	}
	// The loop repeats while an application becomes done, so that a chain can go past more than one step in one reconcile.
	attempted := map[string]bool{}
	// canStart returns true when the application is not yet ready or degraded, when this reconcile
	// has not yet tried to start it, and when all its dependencies are ready or degraded.
	// Past the global time limit, an application starts with no wait for its dependencies.
	canStart := func(key string) bool {
		return !done[key] && !attempted[key] && (pastLimit || allIn(deps[key], done))
	}
	for progress := true; progress; {
		progress = false
		for _, app := range apps {
			key := appKey(app.Namespace, app.Name)
			if !canStart(key) {
				continue
			}
			attempted[key] = true

			state, found := r.scaleUp.apps[key]
			if !found {
				state = &appScaleUp{startedAt: time.Now()}
				r.scaleUp.apps[key] = state
			}
			if !state.ready {
				ready, err := r.startApplication(ctx, app)
				// Past the global time limit, a failed write must not keep Flux at 0, so the application becomes degraded.
				if err != nil && pastLimit {
					ctrl.LoggerFrom(ctx).Error(err, "Failed to start the application at the global time limit", "application", key)
				} else if err != nil {
					return ctrl.Result{}, err
				}
				state.ready = ready
			}
			if state.ready {
				done[key] = true
				progress = true
				continue
			}

			timeout, err := r.readyTimeout(ctx, app.Namespace, app.Name)
			if err != nil {
				return ctrl.Result{}, err
			}
			// Past the global time limit, an application that is not ready becomes degraded at once.
			if time.Since(state.startedAt) > timeout || pastLimit {
				degraded = append(degraded, key)
				done[key] = true
				progress = true
			}
		}
	}
	pending := len(done) < len(apps)

	// Flux comes back last, so that it cannot revert the counts during the scale-up.
	if !pending {
		if err := r.startFlux(ctx, request.Status.RecordedState.FluxControllers); err != nil {
			return ctrl.Result{}, err
		}
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

// startApplication scales the workloads of an application to their recorded counts.
// It returns true when all of them are ready.
func (r *ShutdownRequestReconciler) startApplication(ctx context.Context, app shutdownv1alpha1.RecordedApplication) (bool, error) {
	ready := true
	for _, w := range app.Workloads {
		workloadReady, err := r.startWorkload(ctx, app.Namespace, w)
		if err != nil {
			return false, err
		}
		ready = ready && workloadReady
	}
	return ready, nil
}

// startWorkload scales a workload to its recorded count. It returns true when the workload is ready.
// A workload that no longer exists counts as ready. A StatefulSet waits for StatefulSetScaleUpDelay
// after a forced delete of its pods, because the old container can still run and hold the volume.
// It sends the patch at each check and reads readiness from the response, not from the cache.
// After a cancel in ShuttingDown, the cache can still hold the count before the scale to 0.
// The API server does not store a patch that changes nothing.
func (r *ShutdownRequestReconciler) startWorkload(ctx context.Context, namespace string, w shutdownv1alpha1.RecordedWorkload) (bool, error) {
	switch w.Kind {
	case shutdownv1alpha1.WorkloadKindVirtualMachine:
		return r.startVM(ctx, namespace, w)
	case shutdownv1alpha1.WorkloadKindCronJob:
		return r.restoreCronJob(ctx, namespace, w)
	}
	obj, ok := newScalable(w.Kind)
	if !ok {
		return true, nil
	}
	if forcedAt, found := r.forcedDeletes[appKey(namespace, w.Name)]; found &&
		w.Kind == shutdownv1alpha1.WorkloadKindStatefulSet && time.Since(forcedAt) < r.StatefulSetScaleUpDelay {
		return false, nil
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
