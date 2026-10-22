package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	shutdownv1alpha1 "github.com/giantswarm/pod-shutdown-operator/api/v1alpha1"
)

const token = "test-token"

// newServer returns a server on a fake client that holds the request. The interceptor runs on each write.
func newServer(t *testing.T, request *shutdownv1alpha1.ShutdownRequest, funcs interceptor.Funcs) (*Server, client.Client) {
	scheme := runtime.NewScheme()
	if err := shutdownv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(request).
		WithStatusSubresource(request).
		WithInterceptorFuncs(funcs).
		Build()

	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte(token), 0o600); err != nil {
		t.Fatal(err)
	}
	return &Server{Client: c, Reader: c, TokenFile: tokenFile}, c
}

func newRequest(action shutdownv1alpha1.Action, phase shutdownv1alpha1.Phase) *shutdownv1alpha1.ShutdownRequest {
	return &shutdownv1alpha1.ShutdownRequest{
		ObjectMeta: metav1.ObjectMeta{Name: shutdownv1alpha1.ShutdownRequestName, UID: "old"},
		Spec:       shutdownv1alpha1.ShutdownRequestSpec{Action: action},
		Status:     shutdownv1alpha1.ShutdownRequestStatus{Phase: phase},
	}
}

func post(s *Server, path string) int {
	req := httptest.NewRequest(http.MethodPost, path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec.Code
}

// setPhase sets the phase of the stored request, in the place of the leader.
func setPhase(ctx context.Context, c client.Client, phase shutdownv1alpha1.Phase) error {
	var request shutdownv1alpha1.ShutdownRequest
	if err := c.Get(ctx, client.ObjectKey{Name: shutdownv1alpha1.ShutdownRequestName}, &request); err != nil {
		return err
	}
	request.Status.Phase = phase
	return c.Status().Update(ctx, &request)
}

func getRequest(t *testing.T, c client.Client) *shutdownv1alpha1.ShutdownRequest {
	var request shutdownv1alpha1.ShutdownRequest
	if err := c.Get(context.Background(), client.ObjectKey{Name: shutdownv1alpha1.ShutdownRequestName}, &request); err != nil {
		t.Fatal(err)
	}
	return &request
}

// A cancel reads Complete, and the leader sets Restored before the patch. The optimistic lock fails the patch,
// and the retry reads Restored, where a cancel does nothing.
func TestCancelUsesThePhaseAtTheTimeOfTheWrite(t *testing.T) {
	g := NewWithT(t)
	changed := false
	s, c := newServer(t, newRequest(shutdownv1alpha1.ActionShutdown, shutdownv1alpha1.PhaseComplete), interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if !changed {
				changed = true
				if err := setPhase(ctx, c, shutdownv1alpha1.PhaseRestored); err != nil {
					return err
				}
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	})

	g.Expect(post(s, "/cancel")).To(Equal(http.StatusAccepted))

	request := getRequest(t, c)
	g.Expect(request.Status.Phase).To(Equal(shutdownv1alpha1.PhaseRestored))
	g.Expect(request.Spec.Action).To(Equal(shutdownv1alpha1.ActionShutdown))
}

// A shutdown reads Restored, and the request changes before the delete. The precondition fails the delete,
// and the retry reads the request again.
func TestShutdownDoesNotDeleteARequestThatChanged(t *testing.T) {
	g := NewWithT(t)
	changed := false
	s, c := newServer(t, newRequest(shutdownv1alpha1.ActionCancel, shutdownv1alpha1.PhaseRestored), interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if !changed {
				changed = true
				// A new request replaced the old one after the read. Its phase is ShuttingDown.
				if err := c.Delete(ctx, obj); err != nil {
					return err
				}
				request := newRequest(shutdownv1alpha1.ActionShutdown, "")
				request.UID = "new"
				if err := c.Create(ctx, request); err != nil {
					return err
				}
				if err := setPhase(ctx, c, shutdownv1alpha1.PhaseShuttingDown); err != nil {
					return err
				}
			}
			return c.Delete(ctx, obj, opts...)
		},
	})

	g.Expect(post(s, "/shutdown")).To(Equal(http.StatusAccepted))

	request := getRequest(t, c)
	g.Expect(request.UID).To(BeEquivalentTo("new"))
	g.Expect(request.Status.Phase).To(Equal(shutdownv1alpha1.PhaseShuttingDown))
}
