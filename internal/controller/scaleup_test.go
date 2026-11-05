package controller

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	shutdownv1alpha1 "github.com/giantswarm/pod-shutdown-operator/api/v1alpha1"
)

// scaleUpCase is a scale-up in which some calls to the API server fail.
type scaleUpCase struct {
	pastLimit bool
	// wantFlux is the Flux replicas after the reconcile. wantPhase and wantDegraded are the status after it.
	wantFlux     int32
	wantPhase    shutdownv1alpha1.Phase
	wantDegraded []string
	wantResult   ctrl.Result
}

func TestContinueScaleUpWhenTheManagedApplicationReadsFail(t *testing.T) {
	tests := map[string]scaleUpCase{
		// The backoff of controller-runtime can delay the scale-up past the global time limit.
		"before the global time limit": {
			wantFlux:   0,
			wantPhase:  shutdownv1alpha1.PhaseScalingUp,
			wantResult: ctrl.Result{RequeueAfter: time.Second},
		},
		// A failed read must not keep Flux at 0.
		"past the global time limit": {
			pastLimit:    true,
			wantFlux:     1,
			wantPhase:    shutdownv1alpha1.PhaseRestored,
			wantDegraded: []string{"shop/store"},
		},
	}
	// Each read of a ManagedApplication fails, for example while the cache has not synced.
	errRead := errors.New("the cache has not synced")
	failing := interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*shutdownv1alpha1.ManagedApplication); ok {
				return errRead
			}
			return c.Get(ctx, key, obj, opts...)
		},
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*shutdownv1alpha1.ManagedApplicationList); ok {
				return errRead
			}
			return c.List(ctx, list, opts...)
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			testScaleUp(t, tt, failing)
		})
	}
}

func TestContinueScaleUpWhenTheStartOfAWorkloadFails(t *testing.T) {
	tests := map[string]scaleUpCase{
		// The application is not ready for this poll. Its readyTimeout has not passed, so it is not degraded yet.
		"before the global time limit": {
			wantFlux:   0,
			wantPhase:  shutdownv1alpha1.PhaseScalingUp,
			wantResult: ctrl.Result{RequeueAfter: time.Second},
		},
		// A failed write must not keep Flux at 0, so the application becomes degraded at once.
		"past the global time limit": {
			pastLimit:    true,
			wantFlux:     1,
			wantPhase:    shutdownv1alpha1.PhaseRestored,
			wantDegraded: []string{"shop/store"},
		},
	}
	// Each scale patch of the application workload fails, for example when a webhook cannot be reached.
	failing := interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if obj.GetNamespace() == "shop" {
				return errors.New("failed calling webhook")
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			testScaleUp(t, tt, failing)
		})
	}
}

// testScaleUp runs one reconcile of a scale-up of one application and one Flux controller.
// The funcs make some calls to the API server fail.
func testScaleUp(t *testing.T, tt scaleUpCase, funcs interceptor.Funcs) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := shutdownv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	deployment := func(namespace, name string) *appsv1.Deployment {
		return &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
			Spec:       appsv1.DeploymentSpec{Replicas: ptr.To[int32](0)},
		}
	}
	flux := deployment("flux-system", "kustomize-controller")
	web := deployment("shop", "web")
	request := &shutdownv1alpha1.ShutdownRequest{
		ObjectMeta: metav1.ObjectMeta{Name: shutdownv1alpha1.ShutdownRequestName},
		Status: shutdownv1alpha1.ShutdownRequestStatus{
			Phase:            shutdownv1alpha1.PhaseScalingUp,
			ScaleUpStartedAt: &metav1.Time{Time: time.Now().Add(-time.Hour)},
			RecordedState: &shutdownv1alpha1.RecordedState{
				Applications: []shutdownv1alpha1.RecordedApplication{{
					Namespace: web.Namespace,
					Name:      "store",
					Workloads: []shutdownv1alpha1.RecordedWorkload{{Kind: shutdownv1alpha1.WorkloadKindDeployment, Name: web.Name, Replicas: ptr.To[int32](2)}},
				}},
				FluxControllers: []shutdownv1alpha1.RecordedFluxController{{Namespace: "flux-system", Name: "kustomize-controller", Replicas: 1}},
			},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(flux, web, request).WithStatusSubresource(request).Build()
	r := &ShutdownRequestReconciler{Client: interceptor.NewClient(c, funcs), ScaleUpTimeout: 2 * time.Hour, PollInterval: time.Second}
	if tt.pastLimit {
		r.ScaleUpTimeout = time.Minute
	}

	ctx := context.Background()
	var current shutdownv1alpha1.ShutdownRequest
	if err := c.Get(ctx, client.ObjectKeyFromObject(request), &current); err != nil {
		t.Fatal(err)
	}
	result, err := r.continueScaleUp(ctx, &current)
	if err != nil {
		t.Fatalf("continueScaleUp: %v", err)
	}
	if result != tt.wantResult {
		t.Errorf("result: got %+v, want %+v", result, tt.wantResult)
	}

	if err := c.Get(ctx, client.ObjectKeyFromObject(flux), flux); err != nil {
		t.Fatal(err)
	}
	if replicas := ptr.Deref(flux.Spec.Replicas, 0); replicas != tt.wantFlux {
		t.Errorf("Flux replicas: got %d, want %d", replicas, tt.wantFlux)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(request), &current); err != nil {
		t.Fatal(err)
	}
	if current.Status.Phase != tt.wantPhase {
		t.Errorf("phase: got %q, want %q", current.Status.Phase, tt.wantPhase)
	}
	if !slices.Equal(current.Status.Degraded, tt.wantDegraded) {
		t.Errorf("degraded: got %v, want %v", current.Status.Degraded, tt.wantDegraded)
	}
}
