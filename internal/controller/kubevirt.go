package controller

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	shutdownv1alpha1 "github.com/giantswarm/pod-shutdown-operator/api/v1alpha1"
)

// VirtualMachineActions calls the stop and start subresources of KubeVirt VirtualMachines.
type VirtualMachineActions interface {
	// Stop stops a VM with a grace period in seconds.
	Stop(ctx context.Context, namespace, name string, gracePeriod int64) error

	Start(ctx context.Context, namespace, name string) error
}

// NewVirtualMachineActions returns VirtualMachineActions that call the KubeVirt aggregated API.
func NewVirtualMachineActions(config *rest.Config, httpClient *http.Client) (VirtualMachineActions, error) {
	c := rest.CopyConfig(config)
	c.APIPath = "/apis"
	c.GroupVersion = &schema.GroupVersion{Group: "subresources.kubevirt.io", Version: "v1"}
	// The codecs decode the Status of an error response, so that a Conflict is an API status error.
	c.NegotiatedSerializer = clientgoscheme.Codecs.WithoutConversion()
	restClient, err := rest.RESTClientForConfigAndClient(c, httpClient)
	if err != nil {
		return nil, fmt.Errorf("create the KubeVirt subresource client: %w", err)
	}
	return &subresourceActions{client: restClient}, nil
}

type subresourceActions struct {
	client rest.Interface
}

func (a *subresourceActions) Stop(ctx context.Context, namespace, name string, gracePeriod int64) error {
	return a.put(ctx, namespace, name, "stop", fmt.Appendf(nil, `{"gracePeriod":%d}`, gracePeriod))
}

func (a *subresourceActions) Start(ctx context.Context, namespace, name string) error {
	return a.put(ctx, namespace, name, "start", []byte(`{}`))
}

func (a *subresourceActions) put(ctx context.Context, namespace, name, subresource string, body []byte) error {
	// A slow virt-api must not hold the single worker. The REST client ignores the Timeout of its config.
	return a.client.Put().
		Timeout(subresourceTimeout).
		Namespace(namespace).
		Resource("virtualmachines").
		Name(name).
		SubResource(subresource).
		Body(body).
		Do(ctx).
		Error()
}

const (
	runStrategyAlways = "Always"
	runStrategyHalted = "Halted"
	runStrategyManual = "Manual"

	runStrategyRerunOnFailure = "RerunOnFailure"

	subresourceTimeout = 10 * time.Second
)

// strategyTellsState returns true when the run strategy alone tells if the VM runs.
func strategyTellsState(strategy string) bool {
	return strategy == runStrategyAlways || strategy == runStrategyHalted
}

// newKubeVirtObject returns an unstructured KubeVirt object.
// The client does not cache unstructured objects, so a read goes to the API server.
func newKubeVirtObject(kind, namespace, name string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(schema.GroupVersionKind{Group: "kubevirt.io", Version: "v1", Kind: kind})
	obj.SetNamespace(namespace)
	obj.SetName(name)
	return obj
}

// isAbsent returns true when the object does not exist, or when the cluster has no KubeVirt API.
func isAbsent(err error) bool {
	return apierrors.IsNotFound(err) || meta.IsNoMatchError(err)
}

// recordVM returns the recorded state of a VM: the field that it uses, with its value.
// It returns false when the VM does not exist.
func (r *ShutdownRequestReconciler) recordVM(ctx context.Context, namespace string, ref shutdownv1alpha1.WorkloadReference) (shutdownv1alpha1.RecordedWorkload, bool, error) {
	vm := newKubeVirtObject("VirtualMachine", namespace, ref.Name)
	err := r.Client.Get(ctx, client.ObjectKeyFromObject(vm), vm)
	if isAbsent(err) {
		return shutdownv1alpha1.RecordedWorkload{}, false, nil
	}
	if err != nil {
		return shutdownv1alpha1.RecordedWorkload{}, false, fmt.Errorf("get %s %s/%s: %w", ref.Kind, namespace, ref.Name, err)
	}

	w := shutdownv1alpha1.RecordedWorkload{Kind: ref.Kind, Name: ref.Name}
	if running, found, _ := unstructured.NestedBool(vm.Object, "spec", "running"); found {
		w.Running = &running
		return w, true, nil
	}
	// KubeVirt treats a VM with neither field as Halted.
	strategy, _, _ := unstructured.NestedString(vm.Object, "spec", "runStrategy")
	strategy = cmp.Or(strategy, runStrategyHalted)
	w.RunStrategy = &strategy
	if !strategyTellsState(strategy) {
		vmi, err := r.runningVMI(ctx, namespace, ref.Name)
		if err != nil {
			return shutdownv1alpha1.RecordedWorkload{}, false, err
		}
		// A VMI in deletion stops, for example after a stop by a user.
		w.Started = ptr.To(vmi != nil && vmi.GetDeletionTimestamp() == nil)
	}
	return w, true, nil
}

// runningVMI returns the VMI of a VM. It returns nil when the VMI does not exist or is in a final phase.
func (r *ShutdownRequestReconciler) runningVMI(ctx context.Context, namespace, name string) (*unstructured.Unstructured, error) {
	vmi := newKubeVirtObject("VirtualMachineInstance", namespace, name)
	err := r.Client.Get(ctx, client.ObjectKeyFromObject(vmi), vmi)
	if isAbsent(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get VirtualMachineInstance %s/%s: %w", namespace, name, err)
	}
	// A VMI in a final phase has no running guest.
	phase, _, _ := unstructured.NestedString(vmi.Object, "status", "phase")
	if phase == "Succeeded" || phase == "Failed" {
		return nil, nil
	}
	return vmi, nil
}

// stopVM sets the field of a VM that halts it. KubeVirt then sends a graceful guest shutdown.
// It returns true when the VMI has ended. With force, it calls the stop subresource with a grace period of 0.
func (r *ShutdownRequestReconciler) stopVM(ctx context.Context, namespace, name string, force bool) (bool, error) {
	kind := shutdownv1alpha1.WorkloadKindVirtualMachine
	vm := newKubeVirtObject(string(kind), namespace, name)
	// A VM that no longer exists counts as stopped.
	err := r.Client.Get(ctx, client.ObjectKeyFromObject(vm), vm)
	if isAbsent(err) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("get %s %s/%s: %w", kind, namespace, name, err)
	}

	// KubeVirt rejects a VM with both fields, so the patch sets the field that the VM uses now.
	strategy, _, _ := unstructured.NestedString(vm.Object, "spec", "runStrategy")
	strategy = cmp.Or(strategy, runStrategyHalted)
	halted := strategy == runStrategyHalted
	halt := []byte(`{"spec":{"runStrategy":"Halted"}}`)
	running, usesRunning, _ := unstructured.NestedBool(vm.Object, "spec", "running")
	if usesRunning {
		halted = !running
		halt = []byte(`{"spec":{"running":false}}`)
	}

	vmi, err := r.runningVMI(ctx, namespace, name)
	if err != nil {
		return false, err
	}
	// A halt and a restore of a RerunOnFailure or a Once VM can start it, so a VM that does not run keeps its field.
	if vmi == nil && !usesRunning && !strategyTellsState(strategy) {
		return true, nil
	}
	if !halted {
		err := retryOnce(func() error { return r.Client.Patch(ctx, vm, client.RawPatch(types.MergePatchType, halt)) })
		if isAbsent(err) {
			return true, nil
		}
		if isRejected(err) {
			return false, fmt.Errorf("%w: halt %s %s/%s: %w", errRejected, kind, namespace, name, err)
		}
		if err != nil {
			return false, fmt.Errorf("halt %s %s/%s: %w", kind, namespace, name, err)
		}
	}

	if vmi == nil {
		return true, nil
	}
	if !force {
		return false, nil
	}
	// A VMI that already has a grace period of 0 counts as ended, as a pod does.
	if forceStopped(vmi) {
		return true, nil
	}
	err = retryOnce(func() error { return r.VirtualMachines.Stop(ctx, namespace, name, 0) })
	// KubeVirt returns a Conflict when the VM does not run. An older KubeVirt also does this after it sets
	// the VMI grace period to 0. A new read of the VMI tells if the stop is in effect.
	if apierrors.IsConflict(err) || apierrors.IsNotFound(err) {
		vmi, getErr := r.runningVMI(ctx, namespace, name)
		if getErr == nil && (vmi == nil || forceStopped(vmi)) {
			return true, nil
		}
	}
	if isRejected(err) {
		return false, fmt.Errorf("%w: stop %s %s/%s: %w", errRejected, kind, namespace, name, err)
	}
	if err != nil {
		return false, fmt.Errorf("stop %s %s/%s: %w", kind, namespace, name, err)
	}
	ctrl.LoggerFrom(ctx).Info("Forced stop: stopped a VM with a grace period of 0", "vm", appKey(namespace, name))
	return false, nil
}

// forceStopped returns true when the VMI has a grace period of 0.
func forceStopped(vmi *unstructured.Unstructured) bool {
	gracePeriod, found, _ := unstructured.NestedInt64(vmi.Object, "spec", "terminationGracePeriodSeconds")
	return found && gracePeriod == 0
}

// startVM sets the field of a VM to its recorded value. It returns true when the VM is ready, or when it
// did not run at the shutdown.
func (r *ShutdownRequestReconciler) startVM(ctx context.Context, namespace string, w shutdownv1alpha1.RecordedWorkload) (bool, error) {
	// The patch removes the other field, because KubeVirt rejects a VM with both fields.
	restore := fmt.Appendf(nil, `{"spec":{"runStrategy":%q,"running":null}}`, ptr.Deref(w.RunStrategy, runStrategyHalted))
	if w.Running != nil {
		restore = fmt.Appendf(nil, `{"spec":{"running":%t,"runStrategy":null}}`, *w.Running)
	}
	vm := newKubeVirtObject(string(w.Kind), namespace, w.Name)
	err := r.Client.Patch(ctx, vm, client.RawPatch(types.MergePatchType, restore))
	if isAbsent(err) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("restore %s %s/%s: %w", w.Kind, namespace, w.Name, err)
	}
	if !mustRun(w) {
		return true, nil
	}
	vmi, err := r.runningVMI(ctx, namespace, w.Name)
	if err != nil {
		return false, err
	}
	// After a cancel during the graceful guest shutdown, the Ready condition can still be True for the old VMI.
	if vmi != nil {
		return vmi.GetDeletionTimestamp() == nil && vmReady(vm), nil
	}
	// KubeVirt does not start a Manual VM, or a RerunOnFailure VM whose status already has this run strategy.
	if strategy := ptr.Deref(w.RunStrategy, ""); strategy != runStrategyManual && strategy != runStrategyRerunOnFailure {
		return false, nil
	}

	// KubeVirt rejects a start while an earlier start request waits for its VMI.
	requests, _, _ := unstructured.NestedSlice(vm.Object, "status", "stateChangeRequests")
	if len(requests) > 0 {
		return false, nil
	}
	err = r.VirtualMachines.Start(ctx, namespace, w.Name)
	// KubeVirt returns a Conflict when the VM already runs.
	if err != nil && !apierrors.IsConflict(err) {
		return false, fmt.Errorf("start %s %s/%s: %w", w.Kind, namespace, w.Name, err)
	}
	return false, nil
}

// mustRun returns true when the record tells that the VM ran at the start of the shutdown.
func mustRun(w shutdownv1alpha1.RecordedWorkload) bool {
	if w.Running != nil {
		return *w.Running
	}
	switch strategy := ptr.Deref(w.RunStrategy, runStrategyHalted); {
	case strategy == runStrategyHalted:
		return false
	case !strategyTellsState(strategy):
		return ptr.Deref(w.Started, false)
	}
	return true
}

// vmReady returns true when the Ready condition of the VM is True.
func vmReady(vm *unstructured.Unstructured) bool {
	conditions, _, _ := unstructured.NestedSlice(vm.Object, "status", "conditions")
	for _, c := range conditions {
		condition, _ := c.(map[string]any)
		if condition["type"] == "Ready" {
			return condition["status"] == "True"
		}
	}
	return false
}
