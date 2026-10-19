package controller

import (
	"context"
	"fmt"

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

// continueShutdown scales each recorded workload to 0 and counts the applications whose pods have ended.
// It works only from the record. It sets the phase Complete when all applications have stopped.
func (r *ShutdownRequestReconciler) continueShutdown(ctx context.Context, request *shutdownv1alpha1.ShutdownRequest) (ctrl.Result, error) {
	var stopped int32
	for _, app := range request.Status.RecordedState.Applications {
		appStopped := true
		for _, w := range app.Workloads {
			ended, err := r.stopWorkload(ctx, app.Namespace, w)
			if err != nil {
				return ctrl.Result{}, err
			}
			appStopped = appStopped && ended
		}
		if appStopped {
			stopped++
		}
	}

	complete := stopped == request.Status.Total
	if stopped != request.Status.Stopped || complete {
		updated := request.DeepCopy()
		updated.Status.Stopped = stopped
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
	return ctrl.Result{RequeueAfter: r.PollInterval}, nil
}

// stopWorkload scales a workload to 0. It returns true when no pod of the workload remains.
// A workload that no longer exists counts as stopped.
func (r *ShutdownRequestReconciler) stopWorkload(ctx context.Context, namespace string, w shutdownv1alpha1.RecordedWorkload) (bool, error) {
	obj, ok := newScalable(w.Kind)
	if !ok {
		return true, nil
	}
	err := r.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: w.Name}, obj)
	if apierrors.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("get %s %s/%s: %w", w.Kind, namespace, w.Name, err)
	}

	replicas, selector := replicasAndSelector(obj)
	if ptr.Deref(replicas, 1) != 0 {
		if err := r.Client.Patch(ctx, obj, scaleToZero); err != nil {
			return false, fmt.Errorf("scale %s %s/%s to 0: %w", w.Kind, namespace, w.Name, err)
		}
	}
	return r.podsEnded(ctx, namespace, selector)
}

// podsEnded returns true when no Pod in the namespace matches the selector.
func (r *ShutdownRequestReconciler) podsEnded(ctx context.Context, namespace string, selector *metav1.LabelSelector) (bool, error) {
	s, err := metav1.LabelSelectorAsSelector(selector)
	if err != nil {
		return false, fmt.Errorf("parse the selector: %w", err)
	}
	var pods corev1.PodList
	if err := r.Client.List(ctx, &pods, client.InNamespace(namespace), client.MatchingLabelsSelector{Selector: s}); err != nil {
		return false, fmt.Errorf("list pods in %s: %w", namespace, err)
	}
	return len(pods.Items) == 0, nil
}
