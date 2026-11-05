package controller

import (
	"context"
	"slices"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	shutdownv1alpha1 "github.com/giantswarm/pod-shutdown-operator/api/v1alpha1"
)

func TestStartShutdownRecordsNoUnmanagedWorkloadsWhenTheReadFails(t *testing.T) {
	stray := shutdownv1alpha1.UnmanagedWorkload{Namespace: "shop", Kind: shutdownv1alpha1.WorkloadKindDeployment, Name: "stray"}
	tests := map[string]struct {
		failStatefulSets bool
		wantUnmanaged    []shutdownv1alpha1.UnmanagedWorkload
	}{
		"the read succeeds": {wantUnmanaged: []shutdownv1alpha1.UnmanagedWorkload{stray}},
		// The read lists the Deployments before the StatefulSets, so a part of the result must not stay.
		"the read fails": {failStatefulSets: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			request := &shutdownv1alpha1.ShutdownRequest{
				ObjectMeta: metav1.ObjectMeta{Name: shutdownv1alpha1.ShutdownRequestName},
				Spec:       shutdownv1alpha1.ShutdownRequestSpec{Action: shutdownv1alpha1.ActionShutdown},
			}
			r, c := newBootTestReconciler(t, request, false, interceptor.Funcs{
				List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					if _, ok := list.(*appsv1.StatefulSetList); ok && tt.failStatefulSets {
						return errTest
					}
					return c.List(ctx, list, opts...)
				},
			})
			// An unlisted Deployment that runs.
			deployment := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Namespace: stray.Namespace, Name: stray.Name},
				Spec:       appsv1.DeploymentSpec{Replicas: ptr.To[int32](1)},
			}
			if err := c.Create(context.Background(), deployment); err != nil {
				t.Fatal(err)
			}
			current := getTestRequest(t, c)

			// The unmanaged workloads are information only, so a failed read does not block the shutdown.
			if err := r.startShutdown(context.Background(), &current); err != nil {
				t.Fatalf("startShutdown: %v", err)
			}

			got := getTestRequest(t, c).Status
			if got.Phase != shutdownv1alpha1.PhaseShuttingDown {
				t.Errorf("phase: got %q, want %q", got.Phase, shutdownv1alpha1.PhaseShuttingDown)
			}
			if !slices.Equal(got.Unmanaged, tt.wantUnmanaged) {
				t.Errorf("unmanaged: got %v, want %v", got.Unmanaged, tt.wantUnmanaged)
			}
		})
	}
}
