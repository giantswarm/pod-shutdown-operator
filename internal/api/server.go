// Package api serves the HTTP endpoint of the operator. A caller outside the cluster sends the shutdown and
// cancel calls with a token, and a human operator reads the progress with no token.
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

// defaultBudget is the budget of a request that has no value. It is equal to the CRD default.
const defaultBudget = 10 * time.Minute

// writeTimeout limits the write of an intent, with its retries.
const writeTimeout = 30 * time.Second

var log = ctrl.Log.WithName("api")

// +kubebuilder:rbac:groups=shutdown.giantswarm.io,resources=shutdownrequests,verbs=get;create;patch;delete

// Server writes only the intent of the caller to the ShutdownRequest: it creates the request, or it sets spec.action.
// It never writes status. It runs in every operator pod, also in a pod that does not hold the lease.
type Server struct {
	// Client writes the ShutdownRequest. GET /progress reads it from the cache of Client,
	// so that a call with no token does not reach the API server.
	Client client.Client

	// Reader reads the ShutdownRequest from the API server, so that each write uses the latest phase.
	Reader client.Reader

	// TokenFile is the path of the file that holds the token. The file is read at each call,
	// so a new value of the Secret has an effect with no restart.
	TokenFile string
}

// Progress is the response of GET /progress. It holds no application names and no application states.
type Progress struct {
	Phase shutdownv1alpha1.Phase `json:"phase"`

	// Complete is true only in the phase Complete. It does not mean that every application stopped.
	Complete bool `json:"complete"`

	// BudgetRemainingSeconds is the time to the end of the budget. It is absent outside ShuttingDown and Complete.
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

// withToken refuses a call with no valid token. Otherwise it calls write and returns 202 Accepted:
// the operator acts on the intent later. A call that does nothing under the repeat-call rule is also accepted.
func (s *Server) withToken(write func(context.Context) error) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.validToken(r) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		// The write goes on when the caller goes away, so that a replace does not stop between the delete and the create.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), writeTimeout)
		defer cancel()
		// A write retries when the request changed after the read, so that the phase rule uses the latest phase.
		err := retry.OnError(retry.DefaultRetry, isRetryable, func() error { return write(ctx) })
		if err != nil {
			log.Error(err, "Failed to write the intent", "path", r.URL.Path)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	})
}

// validToken returns true when the Authorization header holds the token from the token file.
// An empty or unreadable token file refuses every call.
func (s *Server) validToken(r *http.Request) bool {
	want, err := os.ReadFile(s.TokenFile)
	if err != nil {
		log.Error(err, "Failed to read the token file, refuses the call")
		return false
	}
	// The Secret value can end with a line end.
	want = bytes.TrimSpace(want)
	// The scheme name is not case-sensitive.
	scheme, got, found := strings.Cut(r.Header.Get("Authorization"), " ")
	return found && strings.EqualFold(scheme, "Bearer") && len(want) > 0 && subtle.ConstantTimeCompare([]byte(got), want) == 1
}

// isRetryable returns true for an error that shows that the request changed between the read and the write.
func isRetryable(err error) bool {
	return apierrors.IsConflict(err) || apierrors.IsAlreadyExists(err) || apierrors.IsNotFound(err)
}

// shutdown applies the repeat-call rule of POST /shutdown.
// In ShuttingDown, Complete, and ScalingUp, it sets the action Shutdown. A repeat call writes nothing,
// because the action is already Shutdown. A Cancel that waits for the leader is replaced, so the latest call wins.
// In ScalingUp, the leader then returns the request to ShuttingDown and reuses the record.
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
	return s.setAction(ctx, request, shutdownv1alpha1.ActionShutdown)
}

// cancel applies the phase rule of POST /cancel. In ShuttingDown, Complete, and ScalingUp, it sets the action Cancel.
// In ScalingUp, a repeat call writes nothing, and a Shutdown that waits for the leader is replaced.
// With no phase, in Restored, or with no request, it does nothing.
func (s *Server) cancel(ctx context.Context) error {
	request, err := s.get(ctx, s.Reader)
	if err != nil {
		return err
	}
	if request == nil {
		return nil
	}
	switch request.Status.Phase {
	case shutdownv1alpha1.PhaseShuttingDown, shutdownv1alpha1.PhaseComplete, shutdownv1alpha1.PhaseScalingUp:
		return s.setAction(ctx, request, shutdownv1alpha1.ActionCancel)
	}
	return nil
}

// get returns the ShutdownRequest from the reader, or nil when it does not exist.
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

// create creates a ShutdownRequest with the action Shutdown. The CRD sets the budget and the safety margin.
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

// replace deletes the request and creates a new one. The delete fails with a conflict when the request changed after the read.
func (s *Server) replace(ctx context.Context, request *shutdownv1alpha1.ShutdownRequest) error {
	precondition := client.Preconditions{UID: &request.UID, ResourceVersion: &request.ResourceVersion}
	if err := s.Client.Delete(ctx, request, precondition); err != nil {
		return fmt.Errorf("delete the Restored ShutdownRequest: %w", err)
	}
	return s.create(ctx)
}

// setAction sets spec.action. The patch fails with a conflict when the request changed after the read.
func (s *Server) setAction(ctx context.Context, request *shutdownv1alpha1.ShutdownRequest, action shutdownv1alpha1.Action) error {
	if request.Spec.Action == action {
		return nil
	}
	patch := client.MergeFromWithOptions(request.DeepCopy(), client.MergeFromWithOptimisticLock{})
	request.Spec.Action = action
	if err := s.Client.Patch(ctx, request, patch); err != nil {
		return fmt.Errorf("set the action %s: %w", action, err)
	}
	log.Info("Set the action of the ShutdownRequest", "action", action, "phase", request.Status.Phase)
	return nil
}

// progress writes the progress of the ShutdownRequest. It needs no token.
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
