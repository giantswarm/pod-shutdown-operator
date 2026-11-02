package controller

import (
	"context"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	shutdownv1alpha1 "github.com/giantswarm/pod-shutdown-operator/api/v1alpha1"
)

// suspendPatch returns the merge patch that sets spec.suspend of a CronJob.
func suspendPatch(suspend bool) client.Patch {
	return client.RawPatch(types.MergePatchType, fmt.Appendf(nil, `{"spec":{"suspend":%t}}`, suspend))
}

// recordCronJob returns the recorded state of a CronJob: its suspend value.
// It returns false when the CronJob does not exist.
func (p planner) recordCronJob(ctx context.Context, namespace string, ref shutdownv1alpha1.WorkloadReference) (shutdownv1alpha1.RecordedWorkload, bool, error) {
	var cronJob batchv1.CronJob
	err := p.reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: ref.Name}, &cronJob)
	if apierrors.IsNotFound(err) {
		return shutdownv1alpha1.RecordedWorkload{}, false, nil
	}
	if err != nil {
		return shutdownv1alpha1.RecordedWorkload{}, false, fmt.Errorf("get %s %s/%s: %w", ref.Kind, namespace, ref.Name, err)
	}
	return shutdownv1alpha1.RecordedWorkload{
		Kind:    ref.Kind,
		Name:    ref.Name,
		Suspend: ptr.To(ptr.Deref(cronJob.Spec.Suspend, false)),
	}, true, nil
}

// suspendCronJob suspends a CronJob, so that no new Job starts. A suspended CronJob counts as stopped.
// It does not stop the Jobs that run.
func (r *ShutdownRequestReconciler) suspendCronJob(ctx context.Context, namespace, name string) (bool, error) {
	kind := shutdownv1alpha1.WorkloadKindCronJob
	cronJob := &batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}}
	err := retryOnce(func() error { return r.Client.Patch(ctx, cronJob, suspendPatch(true)) })
	if apierrors.IsNotFound(err) {
		return true, nil
	}
	if isRejected(err) {
		return false, fmt.Errorf("%w: suspend %s %s/%s: %w", errRejected, kind, namespace, name, err)
	}
	if err != nil {
		return false, fmt.Errorf("suspend %s %s/%s: %w", kind, namespace, name, err)
	}
	return true, nil
}

// restoreCronJob sets spec.suspend of a CronJob to its recorded value. A CronJob has no readiness,
// so it is ready when the patch succeeds.
func (r *ShutdownRequestReconciler) restoreCronJob(ctx context.Context, namespace string, w shutdownv1alpha1.RecordedWorkload) (bool, error) {
	suspend := ptr.Deref(w.Suspend, false)
	cronJob := &batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: w.Name}}
	err := r.Client.Patch(ctx, cronJob, suspendPatch(suspend))
	if apierrors.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("set suspend of %s %s/%s to %t: %w", w.Kind, namespace, w.Name, suspend, err)
	}
	return true, nil
}
