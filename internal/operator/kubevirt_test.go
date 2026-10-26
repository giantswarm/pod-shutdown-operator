//go:build envtest

package operator_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	shutdownv1alpha1 "github.com/giantswarm/pod-shutdown-operator/api/v1alpha1"
	"github.com/giantswarm/pod-shutdown-operator/internal/operator"
)

// vmCall is one call to the start or the stop subresource of a VM.
type vmCall struct {
	Subresource string
	Key         string
	GracePeriod int64
}

// vmActions is a stub for the start and stop subresources of KubeVirt, which envtest cannot serve.
// It acts as KubeVirt does: a stop sets the grace period of the VMI and then deletes the VMI,
// and a start creates the VMI.
type vmActions struct {
	mu    sync.Mutex
	calls []vmCall
	// stopErr is the error of each stop. KubeVirt sets the grace period of the VMI also when it returns an error.
	stopErr error
}

func (a *vmActions) Stop(ctx context.Context, namespace, name string, gracePeriod int64) error {
	a.mu.Lock()
	a.calls = append(a.calls, vmCall{Subresource: "stop", Key: namespace + "/" + name, GracePeriod: gracePeriod})
	stopErr := a.stopErr
	a.mu.Unlock()

	vmi := newVMI(namespace, name)
	patch := client.RawPatch(types.MergePatchType, fmt.Appendf(nil, `{"spec":{"terminationGracePeriodSeconds":%d}}`, gracePeriod))
	if err := k8sClient.Patch(ctx, vmi, patch); err != nil {
		return err
	}
	if stopErr != nil {
		return stopErr
	}
	return k8sClient.Delete(ctx, vmi)
}

func (a *vmActions) Start(ctx context.Context, namespace, name string) error {
	a.mu.Lock()
	a.calls = append(a.calls, vmCall{Subresource: "start", Key: namespace + "/" + name})
	a.mu.Unlock()
	return k8sClient.Create(ctx, newVMI(namespace, name))
}

func (a *vmActions) setStopErr(err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.stopErr = err
}

func (a *vmActions) callsOf() []vmCall {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.calls)
}

func newKubeVirtObject(kind, namespace, name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.GroupVersionKind{Group: "kubevirt.io", Version: "v1", Kind: kind})
	u.SetNamespace(namespace)
	u.SetName(name)
	return u
}

func newVMI(namespace, name string) *unstructured.Unstructured {
	return newKubeVirtObject("VirtualMachineInstance", namespace, name)
}

// createVM creates a VirtualMachine with a spec. envtest runs no KubeVirt, so no VMI follows.
func createVM(ns, name string, spec map[string]any) {
	vm := newKubeVirtObject("VirtualMachine", ns, name)
	vm.Object["spec"] = spec
	Expect(k8sClient.Create(ctx, vm)).To(Succeed())
}

// createVMI creates the VMI of a running VM, in the place of KubeVirt. A VMI with no phase is not final.
func createVMI(ns, name string) {
	Expect(k8sClient.Create(ctx, newVMI(ns, name))).To(Succeed())
}

// deleteVMI deletes the VMI of a VM, in the place of KubeVirt after a guest shutdown.
func deleteVMI(ns, name string) {
	Expect(k8sClient.Delete(ctx, newVMI(ns, name))).To(Succeed())
}

// terminateVMI starts the delete of a VMI, in the place of KubeVirt at the start of a graceful guest shutdown.
// A finalizer keeps the VMI until finishVMI.
func terminateVMI(ns, name string) {
	vmi := newVMI(ns, name)
	keep := client.RawPatch(types.MergePatchType, []byte(`{"metadata":{"finalizers":["test.giantswarm.io/keep"]}}`))
	Expect(k8sClient.Patch(ctx, vmi, keep)).To(Succeed())
	DeferCleanup(func() { finishVMI(ns, name) })
	Expect(k8sClient.Delete(ctx, vmi)).To(Succeed())
}

// finishVMI removes the finalizer of terminateVMI, so that the VMI goes away.
func finishVMI(ns, name string) {
	remove := client.RawPatch(types.MergePatchType, []byte(`{"metadata":{"finalizers":null}}`))
	Expect(client.IgnoreNotFound(k8sClient.Patch(ctx, newVMI(ns, name), remove))).To(Succeed())
}

// setVMIPhase sets the phase of a VMI, in the place of KubeVirt.
func setVMIPhase(ns, name, phase string) {
	vmi := newVMI(ns, name)
	Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(vmi), vmi)).To(Succeed())
	vmi.Object["status"] = map[string]any{"phase": phase}
	Expect(k8sClient.Status().Update(ctx, vmi)).To(Succeed())
}

// setVMReady sets the Ready condition of a VM, in the place of KubeVirt.
func setVMReady(ns, name string) {
	Eventually(func() error {
		vm := newKubeVirtObject("VirtualMachine", ns, name)
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(vm), vm); err != nil {
			return err
		}
		vm.Object["status"] = map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True"}}}
		return k8sClient.Status().Update(ctx, vm)
	}).Should(Succeed())
}

// vmSpecOf returns a function that reads the spec of a VM.
func vmSpecOf(ns, name string) func() map[string]any {
	return func() map[string]any {
		vm := newKubeVirtObject("VirtualMachine", ns, name)
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(vm), vm)).To(Succeed())
		spec, _, err := unstructured.NestedMap(vm.Object, "spec")
		Expect(err).NotTo(HaveOccurred())
		return spec
	}
}

// vmiExists returns a function that returns true while the VMI exists.
func vmiExists(ns, name string) func() bool {
	return func() bool {
		vmi := newVMI(ns, name)
		err := k8sClient.Get(ctx, client.ObjectKeyFromObject(vmi), vmi)
		if apierrors.IsNotFound(err) {
			return false
		}
		Expect(err).NotTo(HaveOccurred())
		return true
	}
}

func vmWorkload(name string) shutdownv1alpha1.WorkloadReference {
	return workload(shutdownv1alpha1.WorkloadKindVirtualMachine, name)
}

var _ = Describe("KubeVirt VirtualMachines", func() {
	var (
		ns      string
		actions *vmActions
		stop    func()
	)

	BeforeEach(func() {
		ns = newNamespace()
		actions = &vmActions{}
		stop, _ = startOperator(operator.Config{VirtualMachineActions: actions})
	})

	AfterEach(func() {
		stop()
		cleanUp(ns)
	})

	It("records the field that each VM uses with its value, and if a VM whose run strategy does not tell it ran", func() {
		createVM(ns, "always", map[string]any{"runStrategy": "Always"})
		createVMI(ns, "always")
		createVM(ns, "legacy", map[string]any{"running": true})
		createVM(ns, "manual-on", map[string]any{"runStrategy": "Manual"})
		createVMI(ns, "manual-on")
		createVM(ns, "manual-done", map[string]any{"runStrategy": "Manual"})
		createVMI(ns, "manual-done")
		setVMIPhase(ns, "manual-done", "Succeeded")
		createVM(ns, "manual-off", map[string]any{"runStrategy": "Manual"})
		createVM(ns, "manual-stopping", map[string]any{"runStrategy": "Manual"})
		createVMI(ns, "manual-stopping")
		terminateVMI(ns, "manual-stopping")
		createVM(ns, "rerun-on", map[string]any{"runStrategy": "RerunOnFailure"})
		createVMI(ns, "rerun-on")
		createVM(ns, "rerun-off", map[string]any{"runStrategy": "RerunOnFailure"})
		createApplication(ns, "vms",
			vmWorkload("always"), vmWorkload("legacy"), vmWorkload("manual-on"), vmWorkload("manual-done"), vmWorkload("manual-off"),
			vmWorkload("manual-stopping"), vmWorkload("rerun-on"), vmWorkload("rerun-off"))

		createShutdownRequest(shutdownv1alpha1.ActionShutdown)

		Eventually(func() *shutdownv1alpha1.RecordedState { return getRequest().Status.RecordedState }).ShouldNot(BeNil())
		vm := shutdownv1alpha1.WorkloadKindVirtualMachine
		Expect(getRequest().Status.RecordedState.Applications[0].Workloads).To(Equal([]shutdownv1alpha1.RecordedWorkload{
			{Kind: vm, Name: "always", RunStrategy: ptr.To("Always")},
			{Kind: vm, Name: "legacy", Running: ptr.To(true)},
			{Kind: vm, Name: "manual-on", RunStrategy: ptr.To("Manual"), Started: ptr.To(true)},
			// A VMI in a final phase does not run.
			{Kind: vm, Name: "manual-done", RunStrategy: ptr.To("Manual"), Started: ptr.To(false)},
			{Kind: vm, Name: "manual-off", RunStrategy: ptr.To("Manual"), Started: ptr.To(false)},
			// A VMI that is in deletion stops, for example after a stop by a user.
			{Kind: vm, Name: "manual-stopping", RunStrategy: ptr.To("Manual"), Started: ptr.To(false)},
			{Kind: vm, Name: "rerun-on", RunStrategy: ptr.To("RerunOnFailure"), Started: ptr.To(true)},
			{Kind: vm, Name: "rerun-off", RunStrategy: ptr.To("RerunOnFailure"), Started: ptr.To(false)},
		}))
	})

	It("does not change a VM that does not run when its run strategy keeps it off", func() {
		specs := map[string]map[string]any{
			"manual":  {"runStrategy": "Manual"},
			"rerun":   {"runStrategy": "RerunOnFailure"},
			"once":    {"runStrategy": "Once"},
			"halted":  {"runStrategy": "Halted"},
			"neither": {},
		}
		var workloads []shutdownv1alpha1.WorkloadReference
		for name, spec := range specs {
			createVM(ns, name, spec)
			workloads = append(workloads, vmWorkload(name))
		}
		createApplication(ns, "vms", workloads...)

		completeShutdown()
		for name, spec := range specs {
			Expect(vmSpecOf(ns, name)()).To(Equal(spec), name)
		}

		// A halt and a restore of a RerunOnFailure or a Once VM can start it. No change keeps it off.
		setAction(shutdownv1alpha1.ActionCancel)

		Eventually(phaseOf).Should(Equal(shutdownv1alpha1.PhaseRestored))
		for name, spec := range specs {
			if name == "neither" {
				// KubeVirt treats a VM with neither field as Halted, and the restore sets this value.
				spec = map[string]any{"runStrategy": "Halted"}
			}
			Expect(vmSpecOf(ns, name)()).To(Equal(spec), name)
		}
		Expect(actions.callsOf()).To(BeEmpty())
	})

	It("fails the application when the forced stop fails", func() {
		actions.setStopErr(apierrors.NewInternalError(errors.New("virt-api is down")))
		createVM(ns, "vm", map[string]any{"runStrategy": "Always"})
		createVMI(ns, "vm")
		createApplication(ns, "app", vmWorkload("vm"))
		setGracePeriod(ns, "app", time.Second)

		createShutdownRequest(shutdownv1alpha1.ActionShutdown)

		Eventually(phaseOf).Should(Equal(shutdownv1alpha1.PhaseComplete))
		Expect(getRequest().Status.Failed).To(ConsistOf(ns + "/app"))
	})

	It("waits for a VMI that is not in deletion after a cancel during the graceful guest shutdown", func() {
		createVM(ns, "win", map[string]any{"runStrategy": "Always"})
		createVMI(ns, "win")
		setVMReady(ns, "win")
		createApplication(ns, "windows", vmWorkload("win"))
		createShutdownRequest(shutdownv1alpha1.ActionShutdown)
		Eventually(vmSpecOf(ns, "win")).Should(Equal(map[string]any{"runStrategy": "Halted"}))
		terminateVMI(ns, "win")

		setAction(shutdownv1alpha1.ActionCancel)

		// The Ready condition is still True, but the old VMI ends.
		Eventually(vmSpecOf(ns, "win")).Should(Equal(map[string]any{"runStrategy": "Always"}))
		Consistently(phaseOf, time.Second).Should(Equal(shutdownv1alpha1.PhaseScalingUp))

		finishVMI(ns, "win")
		createVMI(ns, "win")
		Eventually(phaseOf).Should(Equal(shutdownv1alpha1.PhaseRestored))
	})

	It("gives a VM a graceful stop first and force-stops it at its deadline", func() {
		createVM(ns, "win", map[string]any{"runStrategy": "Always"})
		createVMI(ns, "win")
		createVM(ns, "legacy", map[string]any{"running": true})
		createVMI(ns, "legacy")
		createApplication(ns, "windows", vmWorkload("win"), vmWorkload("legacy"))
		setGracePeriod(ns, "windows", 2*time.Second)

		createShutdownRequest(shutdownv1alpha1.ActionShutdown)

		// KubeVirt sends the graceful guest shutdown when the VM field tells it to halt.
		Eventually(vmSpecOf(ns, "win")).Should(Equal(map[string]any{"runStrategy": "Halted"}))
		Eventually(vmSpecOf(ns, "legacy")).Should(Equal(map[string]any{"running": false}))
		Consistently(actions.callsOf, 1500*time.Millisecond).Should(BeEmpty())
		Expect(phaseOf()).To(Equal(shutdownv1alpha1.PhaseShuttingDown))

		Eventually(actions.callsOf).Should(ConsistOf(
			vmCall{Subresource: "stop", Key: ns + "/win", GracePeriod: 0},
			vmCall{Subresource: "stop", Key: ns + "/legacy", GracePeriod: 0},
		))
		Eventually(phaseOf).Should(Equal(shutdownv1alpha1.PhaseComplete))
		Expect(getRequest().Status.Stopped).To(Equal(int32(1)))
		Expect(getRequest().Status.Failed).To(BeEmpty())
	})

	It("counts a VM as stopped when its VMI ends before the deadline", func() {
		createVM(ns, "vm", map[string]any{"runStrategy": "Always"})
		createVMI(ns, "vm")
		createApplication(ns, "app", vmWorkload("vm"))

		createShutdownRequest(shutdownv1alpha1.ActionShutdown)

		Eventually(vmSpecOf(ns, "vm")).Should(Equal(map[string]any{"runStrategy": "Halted"}))
		Consistently(phaseOf, time.Second).Should(Equal(shutdownv1alpha1.PhaseShuttingDown))

		deleteVMI(ns, "vm")
		Eventually(phaseOf).Should(Equal(shutdownv1alpha1.PhaseComplete))
		Expect(actions.callsOf()).To(BeEmpty())
	})

	It("counts a VM as stopped when the forced stop returns a Conflict and the VMI has a grace period of 0", func() {
		// An older KubeVirt returns a Conflict for a grace period of 0 when the VMI value is unset,
		// after it sets the VMI value to 0.
		actions.setStopErr(apierrors.NewConflict(schema.GroupResource{Group: "kubevirt.io", Resource: "virtualmachine"}, "vm",
			errors.New("Halted only supports manual stop requests with a shorter graceperiod")))
		createVM(ns, "vm", map[string]any{"runStrategy": "Always"})
		createVMI(ns, "vm")
		createApplication(ns, "app", vmWorkload("vm"))
		setGracePeriod(ns, "app", time.Second)

		createShutdownRequest(shutdownv1alpha1.ActionShutdown)

		Eventually(phaseOf).Should(Equal(shutdownv1alpha1.PhaseComplete))
		status := getRequest().Status
		Expect(status.Stopped).To(Equal(int32(1)))
		Expect(status.Failed).To(BeEmpty())
		// A failed write gets one retry, so the stop can come two times.
		Expect(actions.callsOf()).To(HaveEach(vmCall{Subresource: "stop", Key: ns + "/vm", GracePeriod: 0}))
		Expect(vmiExists(ns, "vm")()).To(BeTrue())
	})

	It("restores the runStrategy or the running field of each VM as it was and waits for the Ready condition", func() {
		createVM(ns, "always", map[string]any{"runStrategy": "Always"})
		createVMI(ns, "always")
		createVM(ns, "legacy", map[string]any{"running": true})
		createVMI(ns, "legacy")
		createVM(ns, "halted", map[string]any{"runStrategy": "Halted"})
		createVM(ns, "off", map[string]any{"running": false})
		createApplication(ns, "vms", vmWorkload("always"), vmWorkload("legacy"), vmWorkload("halted"), vmWorkload("off"))
		createShutdownRequest(shutdownv1alpha1.ActionShutdown)
		Eventually(vmSpecOf(ns, "always")).Should(Equal(map[string]any{"runStrategy": "Halted"}))
		Eventually(vmSpecOf(ns, "legacy")).Should(Equal(map[string]any{"running": false}))
		deleteVMI(ns, "always")
		deleteVMI(ns, "legacy")
		Eventually(phaseOf).Should(Equal(shutdownv1alpha1.PhaseComplete))

		setAction(shutdownv1alpha1.ActionCancel)

		Eventually(vmSpecOf(ns, "always")).Should(Equal(map[string]any{"runStrategy": "Always"}))
		Eventually(vmSpecOf(ns, "legacy")).Should(Equal(map[string]any{"running": true}))
		Expect(vmSpecOf(ns, "halted")()).To(Equal(map[string]any{"runStrategy": "Halted"}))
		Expect(vmSpecOf(ns, "off")()).To(Equal(map[string]any{"running": false}))
		// A VM that did not run at the shutdown is ready at once. A VM that ran waits for its Ready condition.
		Consistently(phaseOf, time.Second).Should(Equal(shutdownv1alpha1.PhaseScalingUp))

		// KubeVirt starts a new VMI for each VM that must run.
		createVMI(ns, "always")
		createVMI(ns, "legacy")
		setVMReady(ns, "always")
		setVMReady(ns, "legacy")
		Eventually(phaseOf).Should(Equal(shutdownv1alpha1.PhaseRestored))
		Expect(getRequest().Status.Degraded).To(BeEmpty())
		Expect(actions.callsOf()).To(BeEmpty())
	})

	It("starts a Manual VM that ran through the start subresource and leaves a Manual VM that was off", func() {
		createVM(ns, "manual-on", map[string]any{"runStrategy": "Manual"})
		createVMI(ns, "manual-on")
		createVM(ns, "manual-off", map[string]any{"runStrategy": "Manual"})
		createApplication(ns, "vms", vmWorkload("manual-on"), vmWorkload("manual-off"))
		createShutdownRequest(shutdownv1alpha1.ActionShutdown)
		Eventually(vmSpecOf(ns, "manual-on")).Should(Equal(map[string]any{"runStrategy": "Halted"}))
		deleteVMI(ns, "manual-on")
		Eventually(phaseOf).Should(Equal(shutdownv1alpha1.PhaseComplete))

		setAction(shutdownv1alpha1.ActionCancel)

		Eventually(vmSpecOf(ns, "manual-on")).Should(Equal(map[string]any{"runStrategy": "Manual"}))
		Eventually(vmSpecOf(ns, "manual-off")).Should(Equal(map[string]any{"runStrategy": "Manual"}))
		Eventually(actions.callsOf).Should(Equal([]vmCall{{Subresource: "start", Key: ns + "/manual-on"}}))
		Consistently(phaseOf, time.Second).Should(Equal(shutdownv1alpha1.PhaseScalingUp))

		setVMReady(ns, "manual-on")
		Eventually(phaseOf).Should(Equal(shutdownv1alpha1.PhaseRestored))
		// The VMI exists after the first start, so the operator does not call start again.
		Expect(actions.callsOf()).To(Equal([]vmCall{{Subresource: "start", Key: ns + "/manual-on"}}))
		Expect(vmiExists(ns, "manual-off")()).To(BeFalse())
	})
})
