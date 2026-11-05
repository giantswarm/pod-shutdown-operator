package controller

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	clocktesting "k8s.io/utils/clock/testing"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	shutdownv1alpha1 "github.com/giantswarm/pod-shutdown-operator/api/v1alpha1"
)

const (
	testNode   = "node-a"
	testLeader = "pod-a_1"
	testBootID = "boot-a"
)

var errTest = errors.New("the API server is not available")

// newBootTestReconciler returns a reconciler on a fake client that holds the request.
// With failNodes, the API reader fails each Node list.
func newBootTestReconciler(t *testing.T, request *shutdownv1alpha1.ShutdownRequest, failNodes bool, funcs interceptor.Funcs) (*ShutdownRequestReconciler, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := shutdownv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-b"}}
	node.Status.NodeInfo.BootID = "boot-b"
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(request, node).WithStatusSubresource(request).Build()
	reader := interceptor.NewClient(c, interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*corev1.NodeList); ok && failNodes {
				return errTest
			}
			return c.List(ctx, list, opts...)
		},
	})

	bootIDFile := filepath.Join(t.TempDir(), "boot_id")
	if err := os.WriteFile(bootIDFile, []byte(testBootID+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := &ShutdownRequestReconciler{
		Client:     interceptor.NewClient(c, funcs),
		APIReader:  reader,
		BootIDFile: bootIDFile,
		NodeName:   testNode,
		StaleLimit: time.Minute,
		Clock:      clocktesting.NewFakePassiveClock(time.Now()),
		LeaderIdentity: func(context.Context) (string, error) {
			return testLeader, nil
		},
	}
	return r, c
}

func TestTheBootCheckMarkerIsWrittenOnlyWhenNoPowerCycleHappened(t *testing.T) {
	tests := map[string]struct {
		phase   shutdownv1alpha1.Phase
		bootIDs map[string]string
		step    func(*ShutdownRequestReconciler, context.Context, *shutdownv1alpha1.ShutdownRequest) error
		want    string
	}{
		"checkBoot with an equal boot ID": {
			phase: shutdownv1alpha1.PhaseComplete, bootIDs: map[string]string{testNode: testBootID},
			step: (*ShutdownRequestReconciler).checkBoot, want: testLeader,
		},
		"checkBoot after a power cycle": {
			phase: shutdownv1alpha1.PhaseComplete, bootIDs: map[string]string{testNode: "boot-old"},
			step: (*ShutdownRequestReconciler).checkBoot, want: "old-leader",
		},
		"startShutdown": {phase: "", step: (*ShutdownRequestReconciler).startShutdown, want: testLeader},
		"reuseRecord":   {phase: shutdownv1alpha1.PhaseScalingUp, step: (*ShutdownRequestReconciler).reuseRecord, want: testLeader},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			request := &shutdownv1alpha1.ShutdownRequest{
				ObjectMeta: metav1.ObjectMeta{Name: shutdownv1alpha1.ShutdownRequestName},
				Spec:       shutdownv1alpha1.ShutdownRequestSpec{Action: shutdownv1alpha1.ActionShutdown},
				Status: shutdownv1alpha1.ShutdownRequestStatus{
					Phase:         tt.phase,
					RecordedState: &shutdownv1alpha1.RecordedState{},
					BootIDs:       tt.bootIDs,
					BootCheckedBy: "old-leader",
				},
			}
			r, c := newBootTestReconciler(t, request, false, interceptor.Funcs{})
			current := getTestRequest(t, c)

			if err := tt.step(r, context.Background(), &current); err != nil {
				t.Fatalf("step: %v", err)
			}
			if got := getTestRequest(t, c).Status.BootCheckedBy; got != tt.want {
				t.Errorf("bootCheckedBy: got %q, want %q", got, tt.want)
			}
		})
	}
}

// A failed read of the leader identity must not give a marker. The step tries again at the retry.
func TestTheBootCheckReturnsTheLeaderIdentityError(t *testing.T) {
	request := &shutdownv1alpha1.ShutdownRequest{
		ObjectMeta: metav1.ObjectMeta{Name: shutdownv1alpha1.ShutdownRequestName},
		Spec:       shutdownv1alpha1.ShutdownRequestSpec{Action: shutdownv1alpha1.ActionShutdown},
		Status: shutdownv1alpha1.ShutdownRequestStatus{
			Phase:   shutdownv1alpha1.PhaseComplete,
			BootIDs: map[string]string{testNode: testBootID},
		},
	}
	r, c := newBootTestReconciler(t, request, false, interceptor.Funcs{})
	r.LeaderIdentity = func(context.Context) (string, error) { return "", errTest }
	current := getTestRequest(t, c)

	if err := r.checkBoot(context.Background(), &current); !errors.Is(err, errTest) {
		t.Fatalf("got %v, want the leader identity error", err)
	}
	if got := getTestRequest(t, c).Status.BootCheckedBy; got != "" {
		t.Errorf("bootCheckedBy: got %q, want none", got)
	}
}

func getTestRequest(t *testing.T, c client.Client) shutdownv1alpha1.ShutdownRequest {
	t.Helper()
	var request shutdownv1alpha1.ShutdownRequest
	if err := c.Get(context.Background(), client.ObjectKey{Name: shutdownv1alpha1.ShutdownRequestName}, &request); err != nil {
		t.Fatal(err)
	}
	return request
}

func TestTheRecordStepsReturnTheNodeListError(t *testing.T) {
	tests := map[string]struct {
		phase  shutdownv1alpha1.Phase
		record func(*ShutdownRequestReconciler, context.Context, *shutdownv1alpha1.ShutdownRequest) error
	}{
		"startShutdown": {phase: "", record: (*ShutdownRequestReconciler).startShutdown},
		"reuseRecord":   {phase: shutdownv1alpha1.PhaseScalingUp, record: (*ShutdownRequestReconciler).reuseRecord},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			request := &shutdownv1alpha1.ShutdownRequest{
				ObjectMeta: metav1.ObjectMeta{Name: shutdownv1alpha1.ShutdownRequestName},
				Spec:       shutdownv1alpha1.ShutdownRequestSpec{Action: shutdownv1alpha1.ActionShutdown},
				Status:     shutdownv1alpha1.ShutdownRequestStatus{Phase: tt.phase, RecordedState: &shutdownv1alpha1.RecordedState{}},
			}
			r, c := newBootTestReconciler(t, request, true, interceptor.Funcs{})
			current := getTestRequest(t, c)

			// A record with no entry for node-b gives a false power cycle when the leader moves to node-b.
			if err := tt.record(r, context.Background(), &current); !errors.Is(err, errTest) {
				t.Fatalf("got %v, want the Node list error", err)
			}
			if phase := getTestRequest(t, c).Status.Phase; phase != tt.phase {
				t.Errorf("phase: got %q, want %q", phase, tt.phase)
			}
		})
	}
}

func TestCheckBootRecordsTheOwnBootIDWhenTheNodeListFails(t *testing.T) {
	request := &shutdownv1alpha1.ShutdownRequest{
		ObjectMeta: metav1.ObjectMeta{Name: shutdownv1alpha1.ShutdownRequestName},
		Spec:       shutdownv1alpha1.ShutdownRequestSpec{Action: shutdownv1alpha1.ActionShutdown},
		Status: shutdownv1alpha1.ShutdownRequestStatus{
			Phase:   shutdownv1alpha1.PhaseComplete,
			BootIDs: map[string]string{testNode: "boot-old"},
		},
	}
	r, c := newBootTestReconciler(t, request, true, interceptor.Funcs{})
	current := getTestRequest(t, c)

	// A failed list after a power cycle must not keep the cluster at 0.
	if err := r.checkBoot(context.Background(), &current); err != nil {
		t.Fatalf("checkBoot: %v", err)
	}
	got := getTestRequest(t, c)
	if got.Spec.Action != shutdownv1alpha1.ActionCancel {
		t.Errorf("action: got %q, want %q", got.Spec.Action, shutdownv1alpha1.ActionCancel)
	}
	if bootID := got.Status.BootIDs[testNode]; bootID != testBootID {
		t.Errorf("own boot ID: got %q, want boot-a", bootID)
	}
}

func TestReconcileAppliesTheStaleLimitUntilARequestPassesIt(t *testing.T) {
	const staleLimit = time.Minute
	tests := map[string]struct {
		// age is the age of the request at the first reconcile.
		age  time.Duration
		want shutdownv1alpha1.Phase
	}{
		// The retries of a young request go on for longer than the stale limit.
		"a young request whose start fails": {age: 0, want: shutdownv1alpha1.PhaseShuttingDown},
		// A power-on finds an old request, and the API server fails the first write.
		"an old request whose markStale fails": {age: 2 * staleLimit, want: shutdownv1alpha1.PhaseRestored},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			now := time.Now()
			request := &shutdownv1alpha1.ShutdownRequest{
				ObjectMeta: metav1.ObjectMeta{Name: shutdownv1alpha1.ShutdownRequestName, CreationTimestamp: metav1.NewTime(now.Add(-tt.age))},
				Spec:       shutdownv1alpha1.ShutdownRequestSpec{Action: shutdownv1alpha1.ActionShutdown},
			}
			failUpdate := true
			r, c := newBootTestReconciler(t, request, false, interceptor.Funcs{
				SubResourceUpdate: func(ctx context.Context, c client.Client, subResource string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
					if failUpdate {
						return errTest
					}
					return c.SubResource(subResource).Update(ctx, obj, opts...)
				},
			})
			clock := clocktesting.NewFakePassiveClock(now)
			r.Clock = clock
			r.StaleLimit = staleLimit
			req := ctrl.Request{NamespacedName: client.ObjectKey{Name: shutdownv1alpha1.ShutdownRequestName}}

			if _, err := r.Reconcile(context.Background(), req); !errors.Is(err, errTest) {
				t.Fatalf("first reconcile: got %v, want the update error", err)
			}

			clock.SetTime(now.Add(2 * staleLimit))
			failUpdate = false
			if _, err := r.Reconcile(context.Background(), req); err != nil {
				t.Fatalf("second reconcile: %v", err)
			}
			if phase := getTestRequest(t, c).Status.Phase; phase != tt.want {
				t.Errorf("phase: got %q, want %q", phase, tt.want)
			}
		})
	}
}

func TestCheckBootCountsAMissingOwnEntryAsAPowerCycle(t *testing.T) {
	tests := map[string]shutdownv1alpha1.Phase{
		"ShuttingDown": shutdownv1alpha1.PhaseShuttingDown,
		"Complete":     shutdownv1alpha1.PhaseComplete,
	}
	for name, phase := range tests {
		t.Run(name, func(t *testing.T) {
			// The own node joined after the record, or had an empty boot ID at record time.
			request := &shutdownv1alpha1.ShutdownRequest{
				ObjectMeta: metav1.ObjectMeta{Name: shutdownv1alpha1.ShutdownRequestName},
				Spec:       shutdownv1alpha1.ShutdownRequestSpec{Action: shutdownv1alpha1.ActionShutdown},
				Status: shutdownv1alpha1.ShutdownRequestStatus{
					Phase:   phase,
					BootIDs: map[string]string{"node-b": "boot-b"},
				},
			}
			r, c := newBootTestReconciler(t, request, false, interceptor.Funcs{})
			current := getTestRequest(t, c)

			// The operator cannot tell this from a power cycle, so it scales up.
			if err := r.checkBoot(context.Background(), &current); err != nil {
				t.Fatalf("checkBoot: %v", err)
			}
			got := getTestRequest(t, c)
			if got.Spec.Action != shutdownv1alpha1.ActionCancel {
				t.Errorf("action: got %q, want %q", got.Spec.Action, shutdownv1alpha1.ActionCancel)
			}
			// A later restart in this boot then compares the own entry and does not revert a new Shutdown.
			if bootID := got.Status.BootIDs[testNode]; bootID != "boot-a" {
				t.Errorf("own boot ID: got %q, want boot-a", bootID)
			}
		})
	}
}
