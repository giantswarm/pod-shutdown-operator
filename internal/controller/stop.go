package controller

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	shutdownv1alpha1 "github.com/giantswarm/pod-shutdown-operator/api/v1alpha1"
)

var scaleToZero = client.RawPatch(types.MergePatchType, []byte(`{"spec":{"replicas":0}}`))

// errRejected tells that the API server rejected a write after one retry.
var errRejected = errors.New("the API server rejected the write")

// isRejected returns true when the API server rejected a request, for example because of RBAC, validation, or admission.
func isRejected(err error) bool {
	// An error with no API status has no response from the API server.
	var status apierrors.APIStatus
	if !errors.As(err, &status) {
		return false
	}
	switch code := status.Status().Code; code {
	// A later request can avoid these errors: a failed authentication, NotFound, a timeout, a conflict, too many requests.
	case http.StatusUnauthorized, http.StatusNotFound, http.StatusRequestTimeout, http.StatusConflict, http.StatusTooManyRequests:
		return false
	default:
		// A server error (5xx) is also not a rejection.
		return code >= 400 && code < 500
	}
}

// stopState is the state of one shutdown in the memory of the operator.
type stopState struct {
	// startedAt is status.shutdownStartedAt of the shutdown that the state is for.
	startedAt metav1.Time

	// apps holds the start time of each application by key. It is nil when this process did not start the shutdown.
	apps map[string]time.Time
}

// forceAt returns the time of the forced stop of an application.
func (s *stopState) forceAt(key string, times stopTimes, now time.Time) time.Time {
	// The operator does not store when an application started, so after an operator restart only the deadline applies.
	if s.apps == nil {
		return times.deadline
	}
	// The first call for an application sets its start time.
	startedAt, found := s.apps[key]
	if !found {
		startedAt = now
		s.apps[key] = now
	}
	// The forced stop comes at the earlier of the deadline and the start time plus the shrunk gracePeriod.
	if limit := startedAt.Add(times.gracePeriod); limit.Before(times.deadline) {
		return limit
	}
	return times.deadline
}

// continueShutdown scales the recorded workloads to 0 in dependency order and counts the applications whose pods
// have ended. It sets the phase Complete when all applications have a final state: stopped or failed.
func (r *ShutdownRequestReconciler) continueShutdown(ctx context.Context, request *shutdownv1alpha1.ShutdownRequest) (ctrl.Result, error) {
	// The workloads come from the record. dependsOn and gracePeriod come from the ManagedApplications.
	apps := request.Status.RecordedState.Applications
	deps, err := r.dependencies(ctx, apps)
	if err != nil {
		return ctrl.Result{}, err
	}
	gracePeriods, err := r.gracePeriods(ctx, apps)
	if err != nil {
		return ctrl.Result{}, err
	}
	keys := make([]string, 0, len(apps))
	for _, app := range apps {
		keys = append(keys, appKey(app.Namespace, app.Name))
	}

	// A state for another start time is from an earlier shutdown, or this process did not start the shutdown.
	startedAt := ptr.Deref(request.Status.ShutdownStartedAt, metav1.Time{})
	if !r.stop.startedAt.Equal(&startedAt) {
		r.stop = stopState{startedAt: startedAt}
	}
	// No validation stops a negative safety margin, which would move the end past the budget.
	end := startedAt.Add(ptr.Deref(request.Spec.Budget, metav1.Duration{Duration: defaultBudget}).Duration -
		max(ptr.Deref(request.Spec.SafetyMargin, metav1.Duration{Duration: defaultSafetyMargin}).Duration, 0))
	// The dependency groups start when the shutdown starts.
	times := planStopTimes(keys, deps, gracePeriods, startedAt.Time, end)
	// An application stops after its dependents, and a cycle stops as one step.
	reverse := dependents(cyclesAsSteps(deps))
	now := time.Now()

	failed := slices.Clone(request.Status.Failed)
	// final holds the applications that have stopped or failed.
	final := map[string]bool{}
	for _, key := range failed {
		final[key] = true
	}
	var stopped int32
	// errs holds the errors that a later poll can avoid. They do not stop the other applications.
	var errs []error
	// The loop repeats while an application gets a final state, so that a chain can go past more than one step in one reconcile.
	attempted := map[string]bool{}
	// canStop returns true when the application has no final state, when this reconcile has not yet
	// tried to stop it, and when all its dependents have a final state. Thus each dependency group runs by itself.
	canStop := func(key string) bool {
		return !final[key] && !attempted[key] && allIn(reverse[key], final)
	}
	for progress := true; progress; {
		progress = false
		for _, app := range apps {
			key := appKey(app.Namespace, app.Name)
			if !canStop(key) {
				continue
			}
			attempted[key] = true
			force := !now.Before(r.stop.forceAt(key, times[key], now))
			appStopped, err := r.stopApplication(ctx, app, force)
			// A rejected write fails the application. So does an error at the forced stop, so that a stuck
			// application does not block its dependencies past their deadlines.
			if errors.Is(err, errRejected) || (err != nil && force) {
				ctrl.LoggerFrom(ctx).Error(err, "Failed to stop the application", "application", key)
				failed = append(failed, key)
				final[key] = true
				progress = true
				continue
			}
			// The next poll tries again, up to the forced stop.
			if err != nil {
				errs = append(errs, err)
				continue
			}
			if appStopped {
				final[key] = true
				stopped++
				progress = true
			}
		}
	}

	// A transient error can hide an application that stopped in an earlier poll, and the applications below it.
	// The count for the human operator does not go down.
	stopped = max(stopped, request.Status.Stopped)
	complete := len(final) == len(apps)
	if stopped != request.Status.Stopped || len(failed) != len(request.Status.Failed) || complete {
		updated := request.DeepCopy()
		updated.Status.Stopped = stopped
		updated.Status.Failed = failed
		if complete {
			updated.Status.Phase = shutdownv1alpha1.PhaseComplete
		}
		if err := r.Client.Status().Update(ctx, updated); err != nil {
			return ctrl.Result{}, fmt.Errorf("write the progress: %w", err)
		}
	}
	if complete {
		return ctrl.Result{}, nil
	}
	// The error does not go back to controller-runtime, because its backoff can delay a forced stop past the deadline.
	if len(errs) > 0 {
		ctrl.LoggerFrom(ctx).Error(errors.Join(errs...), "Failed to stop some applications, tries again at the next poll")
	}
	return ctrl.Result{RequeueAfter: r.PollInterval}, nil
}

// stopApplication scales the workloads of an application to 0. It returns true when no pod of them remains.
// With force, it deletes the remaining pods with a grace period of 0.
func (r *ShutdownRequestReconciler) stopApplication(ctx context.Context, app shutdownv1alpha1.RecordedApplication, force bool) (bool, error) {
	stopped := true
	var errs []error
	for _, w := range app.Workloads {
		ended, err := r.stopWorkload(ctx, app.Namespace, w, force)
		// An error on one workload does not stop the other workloads.
		if err != nil {
			errs = append(errs, err)
			continue
		}
		stopped = stopped && ended
	}
	// The joined error wraps errRejected when the API server rejected a write.
	if len(errs) > 0 {
		return false, errors.Join(errs...)
	}
	return stopped, nil
}

// stopWorkload scales a workload to 0. It returns true when no pod of the workload remains.
// With force, it deletes the remaining pods with a grace period of 0.
func (r *ShutdownRequestReconciler) stopWorkload(ctx context.Context, namespace string, w shutdownv1alpha1.RecordedWorkload, force bool) (bool, error) {
	obj, ok := newScalable(w.Kind)
	if !ok {
		return true, nil
	}
	// A workload that no longer exists counts as stopped.
	err := r.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: w.Name}, obj)
	if apierrors.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("get %s %s/%s: %w", w.Kind, namespace, w.Name, err)
	}

	replicas, selector := replicasAndSelector(obj)
	if ptr.Deref(replicas, 1) != 0 {
		err := retryOnce(func() error { return r.Client.Patch(ctx, obj, scaleToZero) })
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		if isRejected(err) {
			return false, fmt.Errorf("%w: scale %s %s/%s to 0: %w", errRejected, w.Kind, namespace, w.Name, err)
		}
		if err != nil {
			return false, fmt.Errorf("scale %s %s/%s to 0: %w", w.Kind, namespace, w.Name, err)
		}
	}
	// A failed scale patch returns before this step. With replicas above 0, the controller only makes the pods again.
	if force {
		// The workload stops when the delete finds no pod. Thus the next poll also deletes a pod
		// that the workload controller made after the delete.
		deleted, err := r.forceDelete(ctx, obj, selector)
		return deleted == 0 && err == nil, err
	}
	pods, err := r.runningPods(ctx, namespace, selector)
	return len(pods) == 0, err
}

// forceDelete deletes the pods of a workload that have not ended, with a grace period of 0.
// It returns the number of pods that it deleted.
func (r *ShutdownRequestReconciler) forceDelete(ctx context.Context, workload client.Object, selector *metav1.LabelSelector) (int, error) {
	namespace := workload.GetNamespace()
	pods, err := r.runningPods(ctx, namespace, selector)
	if err != nil {
		return 0, err
	}
	deleted := 0
	for _, pod := range pods {
		// A pod that already has a grace period of 0 counts as ended, also when a finalizer keeps it.
		if ptr.Deref(pod.DeletionGracePeriodSeconds, -1) == 0 {
			continue
		}
		// The operator does not manage bare pods or unlisted workloads with the same labels.
		owned, err := r.ownedBy(ctx, &pod, workload)
		if err != nil {
			return deleted, err
		}
		if !owned {
			continue
		}
		err = retryOnce(func() error { return r.Client.Delete(ctx, &pod, client.GracePeriodSeconds(0)) })
		if apierrors.IsNotFound(err) {
			continue
		}
		if isRejected(err) {
			return deleted, fmt.Errorf("%w: delete pod %s/%s: %w", errRejected, namespace, pod.Name, err)
		}
		if err != nil {
			return deleted, fmt.Errorf("delete pod %s/%s: %w", namespace, pod.Name, err)
		}
		deleted++
		ctrl.LoggerFrom(ctx).Info("Forced stop: deleted a pod with a grace period of 0", "pod", appKey(namespace, pod.Name))
		// The scale-up of a StatefulSet waits after a forced delete, because the old container can still hold the volume.
		if _, isStatefulSet := workload.(*appsv1.StatefulSet); isStatefulSet {
			if r.forcedDeletes == nil {
				r.forcedDeletes = map[string]time.Time{}
			}
			r.forcedDeletes[appKey(namespace, workload.GetName())] = time.Now()
		}
	}
	return deleted, nil
}

// ownedBy returns true when the workload controls the pod.
func (r *ShutdownRequestReconciler) ownedBy(ctx context.Context, pod *corev1.Pod, workload client.Object) (bool, error) {
	// The controller of a StatefulSet pod is the StatefulSet.
	if _, isDeployment := workload.(*appsv1.Deployment); !isDeployment {
		return metav1.IsControlledBy(pod, workload), nil
	}
	// The controller of a Deployment pod is a ReplicaSet, whose controller is the Deployment.
	ref := metav1.GetControllerOf(pod)
	if ref == nil || ref.Kind != "ReplicaSet" {
		return false, nil
	}
	var rs appsv1.ReplicaSet
	err := r.Client.Get(ctx, client.ObjectKey{Namespace: pod.Namespace, Name: ref.Name}, &rs)
	// A ReplicaSet that no longer exists controls no pod.
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("get ReplicaSet %s/%s: %w", pod.Namespace, ref.Name, err)
	}
	// The UID check rejects a new ReplicaSet with the name of a deleted one.
	return rs.UID == ref.UID && metav1.IsControlledBy(&rs, workload), nil
}

// retryOnce calls write. It calls write one more time when the first call fails with an error other than NotFound.
func retryOnce(write func() error) error {
	err := write()
	if err != nil && !apierrors.IsNotFound(err) {
		err = write()
	}
	return err
}

// runningPods returns the Pods in the namespace that match the selector and are not in a terminal phase.
func (r *ShutdownRequestReconciler) runningPods(ctx context.Context, namespace string, selector *metav1.LabelSelector) ([]corev1.Pod, error) {
	s, err := metav1.LabelSelectorAsSelector(selector)
	if err != nil {
		return nil, fmt.Errorf("parse the selector: %w", err)
	}
	var pods corev1.PodList
	if err := r.Client.List(ctx, &pods, client.InNamespace(namespace), client.MatchingLabelsSelector{Selector: s}); err != nil {
		return nil, fmt.Errorf("list pods in %s: %w", namespace, err)
	}
	// A Pod in a terminal phase has no running container, and the scale to 0 does not delete it.
	return slices.DeleteFunc(pods.Items, func(pod corev1.Pod) bool {
		return pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed
	}), nil
}
