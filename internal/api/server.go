// Package api serves the HTTP endpoint of the operator.
package api

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	shutdownv1alpha1 "github.com/giantswarm/pod-shutdown-operator/api/v1alpha1"
)

// defaultBudget is equal to the CRD default.
const defaultBudget = 10 * time.Minute

// writeTimeout limits a write, with its retries.
const writeTimeout = 30 * time.Second

var log = ctrl.Log.WithName("api")

// +kubebuilder:rbac:groups=shutdown.giantswarm.io,resources=shutdownrequests,verbs=get;create;patch;delete

// Server writes only spec.action of the ShutdownRequest, or creates it. It runs in every operator pod.
type Server struct {
	// Client writes the request. GET /progress reads from its cache.
	Client client.Client

	// Reader reads the request from the API server for each write.
	Reader client.Reader

	// TokenFile is read at each call, so a new Secret value needs no restart.
	TokenFile string
}

// Progress is the response of GET /progress. It holds no application names.
type Progress struct {
	Phase    shutdownv1alpha1.Phase `json:"phase"`
	Complete bool                   `json:"complete"`

	// BudgetRemainingSeconds is absent outside ShuttingDown and Complete.
	BudgetRemainingSeconds *int64 `json:"budgetRemainingSeconds,omitempty"`

	Stopped int32 `json:"stopped"`
	Total   int32 `json:"total"`
}

// Handler returns the handler of the endpoint.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("POST /shutdown", s.withToken(s.shutdown))
	mux.Handle("POST /cancel", s.withToken(s.cancel))
	mux.HandleFunc("GET /progress", s.progress)
	return mux
}

// withToken calls write when the token is valid.
func (s *Server) withToken(write func(context.Context) error) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.validToken(r) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		// The write goes on when the caller goes away, so that a replace does not stop after the delete.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), writeTimeout)
		defer cancel()
		// A write retries when the request changed after the read.
		err := retry.OnError(retry.DefaultRetry, isRetryable, func() error { return write(ctx) })
		if err != nil {
			log.Error(err, "Failed to write the intent", "path", r.URL.Path)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		// The leader acts on the intent later.
		w.WriteHeader(http.StatusAccepted)
	})
}

// validToken returns true when the Authorization header holds the token.
func (s *Server) validToken(r *http.Request) bool {
	want, err := os.ReadFile(s.TokenFile)
	// An unreadable token file refuses every call.
	if err != nil {
		log.Error(err, "Failed to read the token file, refuses the call")
		return false
	}
	// The Secret value can end with a line end.
	want = bytes.TrimSpace(want)
	// The scheme name is not case-sensitive.
	scheme, got, found := strings.Cut(r.Header.Get("Authorization"), " ")
	// An empty token file refuses every call.
	return found && strings.EqualFold(scheme, "Bearer") && len(want) > 0 && subtle.ConstantTimeCompare([]byte(got), want) == 1
}

// isRetryable returns true when the request changed between the read and the write.
func isRetryable(err error) bool {
	return apierrors.IsConflict(err) || apierrors.IsAlreadyExists(err) || apierrors.IsNotFound(err)
}

// shutdown applies the phase rule of POST /shutdown.
func (s *Server) shutdown(ctx context.Context) error {
	request, err := s.get(ctx, s.Reader)
	if err != nil {
		return err
	}
	switch {
	case request == nil:
		return s.create(ctx)
	// After Restored, a new request replaces the old one.
	case request.Status.Phase == shutdownv1alpha1.PhaseRestored:
		return s.replace(ctx, request)
	// With no phase, the operator starts the request.
	case request.Status.Phase == "":
		return nil
	}
	// The latest call wins over a Cancel that waits for the leader. In ScalingUp, the leader reuses the record.
	return s.setAction(ctx, request, shutdownv1alpha1.ActionShutdown)
}

// cancel applies the phase rule of POST /cancel.
func (s *Server) cancel(ctx context.Context) error {
	request, err := s.get(ctx, s.Reader)
	if err != nil {
		return err
	}
	if request == nil {
		return nil
	}
	switch request.Status.Phase {
	// In ScalingUp, the latest call wins over a Shutdown that waits for the leader.
	case shutdownv1alpha1.PhaseShuttingDown, shutdownv1alpha1.PhaseComplete, shutdownv1alpha1.PhaseScalingUp:
		return s.setAction(ctx, request, shutdownv1alpha1.ActionCancel)
	}
	// With no phase or in Restored, a cancel does nothing.
	return nil
}

// get returns the request, or nil when it does not exist.
func (s *Server) get(ctx context.Context, reader client.Reader) (*shutdownv1alpha1.ShutdownRequest, error) {
	var request shutdownv1alpha1.ShutdownRequest
	err := reader.Get(ctx, client.ObjectKey{Name: shutdownv1alpha1.ShutdownRequestName}, &request)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get the ShutdownRequest: %w", err)
	}
	return &request, nil
}

// create creates a request with the action Shutdown.
func (s *Server) create(ctx context.Context) error {
	request := &shutdownv1alpha1.ShutdownRequest{
		ObjectMeta: metav1.ObjectMeta{Name: shutdownv1alpha1.ShutdownRequestName},
		Spec:       shutdownv1alpha1.ShutdownRequestSpec{Action: shutdownv1alpha1.ActionShutdown},
	}
	if err := s.Client.Create(ctx, request); err != nil {
		return fmt.Errorf("create the ShutdownRequest: %w", err)
	}
	log.Info("Created a ShutdownRequest")
	return nil
}

// replace deletes the request and creates a new one.
func (s *Server) replace(ctx context.Context, request *shutdownv1alpha1.ShutdownRequest) error {
	// The delete fails with a conflict when the request changed after the read.
	precondition := client.Preconditions{UID: &request.UID, ResourceVersion: &request.ResourceVersion}
	if err := s.Client.Delete(ctx, request, precondition); err != nil {
		return fmt.Errorf("delete the Restored ShutdownRequest: %w", err)
	}
	return s.create(ctx)
}

// setAction sets spec.action.
func (s *Server) setAction(ctx context.Context, request *shutdownv1alpha1.ShutdownRequest, action shutdownv1alpha1.Action) error {
	// A repeat call writes nothing.
	if request.Spec.Action == action {
		return nil
	}
	// The patch fails with a conflict when the request changed after the read.
	patch := client.MergeFromWithOptions(request.DeepCopy(), client.MergeFromWithOptimisticLock{})
	request.Spec.Action = action
	if err := s.Client.Patch(ctx, request, patch); err != nil {
		return fmt.Errorf("set the action %s: %w", action, err)
	}
	log.Info("Set the action of the ShutdownRequest", "action", action, "phase", request.Status.Phase)
	return nil
}

// progress writes the progress of the request. It needs no token.
func (s *Server) progress(w http.ResponseWriter, r *http.Request) {
	request, err := s.get(r.Context(), s.Client)
	if err != nil {
		log.Error(err, "Failed to read the progress")
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	var p Progress
	if request != nil {
		status := request.Status
		p = Progress{
			Phase:    status.Phase,
			Complete: status.Phase == shutdownv1alpha1.PhaseComplete,
			Stopped:  status.Stopped,
			Total:    status.Total,
		}
		if (status.Phase == shutdownv1alpha1.PhaseShuttingDown || status.Phase == shutdownv1alpha1.PhaseComplete) && status.ShutdownStartedAt != nil {
			// The budget ends at the node power-off, so the remainder ignores the safety margin.
			end := status.ShutdownStartedAt.Add(ptr.Deref(request.Spec.Budget, metav1.Duration{Duration: defaultBudget}).Duration)
			p.BudgetRemainingSeconds = ptr.To(int64(max(time.Until(end), 0) / time.Second))
		}
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(p); err != nil {
		log.Error(err, "Failed to write the progress")
	}
}
