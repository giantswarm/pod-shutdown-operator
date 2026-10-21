package controller

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	shutdownv1alpha1 "github.com/giantswarm/pod-shutdown-operator/api/v1alpha1"
)

// startShutdown writes the record, the start time, and the phase ShuttingDown in one status update.
// It changes no workload. The update uses the resource version of the request,
// so a stale read fails with a conflict and the record is never written twice.
func (r *ShutdownRequestReconciler) startShutdown(ctx context.Context, request *shutdownv1alpha1.ShutdownRequest) error {
	record, err := r.recordState(ctx)
	if err != nil {
		return err
	}

	now := metav1.Now()
	updated := request.DeepCopy()
	updated.Status = shutdownv1alpha1.ShutdownRequestStatus{
		Phase:             shutdownv1alpha1.PhaseShuttingDown,
		RecordedState:     record,
		ShutdownStartedAt: &now,
		Total:             int32(len(record.Applications)), //nolint:gosec // The number of applications is small.
	}
	if err := r.Client.Status().Update(ctx, updated); err != nil {
		return fmt.Errorf("write the record: %w", err)
	}
	// This process starts the shutdown, so it knows the start time of each application.
	// The update reads the stored start time back, which has a precision of one second.
	r.stop = stopState{startedAt: *updated.Status.ShutdownStartedAt, apps: map[string]time.Time{}}
	return nil
}

// recordState reads the replicas of each listed Deployment and StatefulSet.
// It skips a listed workload that does not exist.
func (r *ShutdownRequestReconciler) recordState(ctx context.Context) (*shutdownv1alpha1.RecordedState, error) {
	var apps shutdownv1alpha1.ManagedApplicationList
	if err := r.Client.List(ctx, &apps); err != nil {
		return nil, fmt.Errorf("list ManagedApplications: %w", err)
	}

	record := &shutdownv1alpha1.RecordedState{}
	for _, app := range apps.Items {
		recorded := shutdownv1alpha1.RecordedApplication{Namespace: app.Namespace, Name: app.Name}
		for _, ref := range app.Spec.Workloads {
			w, found, err := r.recordWorkload(ctx, app.Namespace, ref)
			if err != nil {
				return nil, err
			}
			if found {
				recorded.Workloads = append(recorded.Workloads, w)
			}
		}
		record.Applications = append(record.Applications, recorded)
	}
	slices.SortFunc(record.Applications, func(a, b shutdownv1alpha1.RecordedApplication) int {
		return cmp.Or(cmp.Compare(a.Namespace, b.Namespace), cmp.Compare(a.Name, b.Name))
	})
	return record, nil
}

// recordWorkload returns the recorded state of one workload. It returns false when the operator
// does not scale the kind, or when the workload does not exist.
func (r *ShutdownRequestReconciler) recordWorkload(ctx context.Context, namespace string, ref shutdownv1alpha1.WorkloadReference) (shutdownv1alpha1.RecordedWorkload, bool, error) {
	obj, ok := newScalable(ref.Kind)
	if !ok {
		return shutdownv1alpha1.RecordedWorkload{}, false, nil
	}
	err := r.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: ref.Name}, obj)
	if apierrors.IsNotFound(err) {
		return shutdownv1alpha1.RecordedWorkload{}, false, nil
	}
	if err != nil {
		return shutdownv1alpha1.RecordedWorkload{}, false, fmt.Errorf("get %s %s/%s: %w", ref.Kind, namespace, ref.Name, err)
	}

	replicas, _ := replicasAndSelector(obj)
	return shutdownv1alpha1.RecordedWorkload{
		Kind:     ref.Kind,
		Name:     ref.Name,
		Replicas: ptr.To(ptr.Deref(replicas, 1)),
	}, true, nil
}
