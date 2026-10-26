package controller

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	shutdownv1alpha1 "github.com/giantswarm/pod-shutdown-operator/api/v1alpha1"
)

// subresourceCall is one request that the fake KubeVirt API server got.
type subresourceCall struct {
	method, path, query, body string
}

func TestVirtualMachineActionsCallTheSubresources(t *testing.T) {
	var calls []subresourceCall
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		calls = append(calls, subresourceCall{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery, body: string(body)})
		// KubeVirt answers a stop of a VM that does not run with a Conflict.
		if r.URL.Path == "/apis/subresources.kubevirt.io/v1/namespaces/ns/virtualmachines/off/stop" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"Conflict","code":409,"message":"VM is not running"}`)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	actions, err := NewVirtualMachineActions(&rest.Config{Host: server.URL}, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := actions.Stop(ctx, "ns", "vm", 0); err != nil {
		t.Errorf("Stop: %v", err)
	}
	if err := actions.Start(ctx, "ns", "vm"); err != nil {
		t.Errorf("Start: %v", err)
	}
	if err := actions.Stop(ctx, "ns", "off", 0); !apierrors.IsConflict(err) {
		t.Errorf("Stop of a VM that does not run: got %v, want a Conflict", err)
	}

	// The timeout query tells that each call has the time limit. The HTTP client of the manager has none.
	want := []subresourceCall{
		{method: http.MethodPut, path: "/apis/subresources.kubevirt.io/v1/namespaces/ns/virtualmachines/vm/stop", query: "timeout=10s", body: `{"gracePeriod":0}`},
		{method: http.MethodPut, path: "/apis/subresources.kubevirt.io/v1/namespaces/ns/virtualmachines/vm/start", query: "timeout=10s", body: `{}`},
		{method: http.MethodPut, path: "/apis/subresources.kubevirt.io/v1/namespaces/ns/virtualmachines/off/stop", query: "timeout=10s", body: `{"gracePeriod":0}`},
	}
	if len(calls) != len(want) {
		t.Fatalf("got calls %+v, want %+v", calls, want)
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Errorf("call %d: got %+v, want %+v", i, calls[i], want[i])
		}
	}
}

// TestVirtualMachineWithNoKubeVirt checks that a cluster with no KubeVirt API does not block a shutdown or a scale-up.
// The scheme of the fake client has no KubeVirt types, so its REST mapper returns a no-match error.
func TestVirtualMachineWithNoKubeVirt(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := shutdownv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	r := &ShutdownRequestReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).Build()}
	ctx := context.Background()
	ref := shutdownv1alpha1.WorkloadReference{Kind: shutdownv1alpha1.WorkloadKindVirtualMachine, Name: "vm"}
	recorded := shutdownv1alpha1.RecordedWorkload{Kind: ref.Kind, Name: ref.Name, RunStrategy: ptr.To("Always")}

	if _, found, err := r.recordWorkload(ctx, "ns", ref); found || err != nil {
		t.Errorf("recordWorkload: got found %v and error %v, want no workload and no error", found, err)
	}
	if stopped, err := r.stopWorkload(ctx, "ns", recorded, true); !stopped || err != nil {
		t.Errorf("stopWorkload: got stopped %v and error %v, want stopped and no error", stopped, err)
	}
	if ready, err := r.startWorkload(ctx, "ns", recorded); !ready || err != nil {
		t.Errorf("startWorkload: got ready %v and error %v, want ready and no error", ready, err)
	}
}
