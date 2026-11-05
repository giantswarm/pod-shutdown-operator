package controller

import (
	"context"
	"errors"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	shutdownv1alpha1 "github.com/giantswarm/pod-shutdown-operator/api/v1alpha1"
)

func TestStopWorkloadConfirmsACachedZeroWithALiveRead(t *testing.T) {
	tests := map[string]struct {
		// live is spec.replicas on the API server. The cache shows 0.
		live        int32
		wantPatches int
	}{
		// Just after a scale-up patch, the cache can still show 0.
		"the API server has replicas above 0": {live: 3, wantPatches: 1},
		// A patch that changes nothing still goes through admission, which can reject it.
		"the API server has 0 replicas": {live: 0, wantPatches: 0},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			frontend := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "frontend"},
				Spec: appsv1.DeploymentSpec{
					Replicas: ptr.To(tt.live),
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "frontend"}},
				},
			}
			c := fake.NewClientBuilder().WithObjects(frontend).Build()
			patches := 0
			stale := interceptor.NewClient(c, interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if err := c.Get(ctx, key, obj, opts...); err != nil {
						return err
					}
					if d, ok := obj.(*appsv1.Deployment); ok {
						d.Spec.Replicas = ptr.To[int32](0)
					}
					return nil
				},
				Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
					patches++
					return c.Patch(ctx, obj, patch, opts...)
				},
			})
			r := &ShutdownRequestReconciler{Client: stale, APIReader: c}

			w := shutdownv1alpha1.RecordedWorkload{Kind: shutdownv1alpha1.WorkloadKindDeployment, Name: frontend.Name}
			if _, err := r.stopWorkload(context.Background(), frontend.Namespace, w, false); err != nil {
				t.Fatalf("stopWorkload: %v", err)
			}

			if patches != tt.wantPatches {
				t.Errorf("patches: got %d, want %d", patches, tt.wantPatches)
			}
			var got appsv1.Deployment
			if err := c.Get(context.Background(), client.ObjectKeyFromObject(frontend), &got); err != nil {
				t.Fatal(err)
			}
			if replicas := ptr.Deref(got.Spec.Replicas, 1); replicas != 0 {
				t.Errorf("replicas on the API server: got %d, want 0", replicas)
			}
		})
	}
}

func TestStopWorkloadWhenTheLiveReadFails(t *testing.T) {
	tests := map[string]struct {
		liveErr   error
		wantEnded bool
		wantErr   bool
	}{
		// A workload that no longer exists counts as stopped.
		"the workload no longer exists": {liveErr: apierrors.NewNotFound(appsv1.Resource("deployments"), "backend"), wantEnded: true},
		// The next poll tries again.
		"the read fails": {liveErr: errors.New("connection refused"), wantErr: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			// The cache shows 0, so stopWorkload reads the workload live.
			backend := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "backend"},
				Spec:       appsv1.DeploymentSpec{Replicas: ptr.To[int32](0)},
			}
			c := fake.NewClientBuilder().WithObjects(backend).Build()
			patches := 0
			counting := interceptor.NewClient(c, interceptor.Funcs{
				Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
					patches++
					return c.Patch(ctx, obj, patch, opts...)
				},
			})
			live := interceptor.NewClient(c, interceptor.Funcs{
				Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
					return tt.liveErr
				},
			})
			r := &ShutdownRequestReconciler{Client: counting, APIReader: live}

			w := shutdownv1alpha1.RecordedWorkload{Kind: shutdownv1alpha1.WorkloadKindDeployment, Name: backend.Name}
			ended, err := r.stopWorkload(context.Background(), backend.Namespace, w, false)

			if (err != nil) != tt.wantErr {
				t.Errorf("error: got %v, want an error: %t", err, tt.wantErr)
			}
			if ended != tt.wantEnded {
				t.Errorf("ended: got %t, want %t", ended, tt.wantEnded)
			}
			// The operator does not send a patch when it cannot confirm the replicas.
			if patches != 0 {
				t.Errorf("patches: got %d, want 0", patches)
			}
		})
	}
}
