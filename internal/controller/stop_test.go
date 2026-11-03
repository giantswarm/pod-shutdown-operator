package controller

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
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
