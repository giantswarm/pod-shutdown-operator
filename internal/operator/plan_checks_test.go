//go:build envtest

package operator_test

import (
	"fmt"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	shutdownv1alpha1 "github.com/giantswarm/pod-shutdown-operator/api/v1alpha1"
	"github.com/giantswarm/pod-shutdown-operator/internal/operator"
)

// deleteAllWorkloads deletes the Deployments, StatefulSets, and VMs in all namespaces.
// envtest runs no namespace controller, so the workloads of earlier tests stay, and a plan reports them as unmanaged.
func deleteAllWorkloads() {
	var namespaces corev1.NamespaceList
	Expect(k8sClient.List(ctx, &namespaces)).To(Succeed())
	for _, ns := range namespaces.Items {
		Expect(k8sClient.DeleteAllOf(ctx, &appsv1.Deployment{}, client.InNamespace(ns.Name))).To(Succeed())
		Expect(k8sClient.DeleteAllOf(ctx, &appsv1.StatefulSet{}, client.InNamespace(ns.Name))).To(Succeed())
		Expect(k8sClient.DeleteAllOf(ctx, newKubeVirtObject("VirtualMachine", "", ""), client.InNamespace(ns.Name))).To(Succeed())
	}
}

// setTerminationGracePeriod sets terminationGracePeriodSeconds in the pod template of a Deployment or a StatefulSet.
func setTerminationGracePeriod(obj client.Object, seconds int64) {
	patch := client.RawPatch(types.MergePatchType, fmt.Appendf(nil, `{"spec":{"template":{"spec":{"terminationGracePeriodSeconds":%d}}}}`, seconds))
	Expect(k8sClient.Patch(ctx, obj, patch)).To(Succeed())
}

// guardVMs makes the API server reject a halt of each VM in the namespace with the runStrategy field.
// It does not reject a halt with the running field.
func guardVMs(ns string) {
	rejectUpdates(ns, "kubevirt.io", "virtualmachines",
		`!has(object.spec.runStrategy) || object.spec.runStrategy != "Halted"`, "halt is not allowed")

	// No application lists the probe VM.
	createVM(ns, "guard-probe", map[string]any{"runStrategy": "Always"})
	probe := newKubeVirtObject("VirtualMachine", ns, "guard-probe")
	waitForRejection(probe, client.RawPatch(types.MergePatchType, []byte(`{"spec":{"runStrategy":"Halted"}}`)), "halt is not allowed")
}

// createVMIWithGracePeriod creates the VMI of a running VM with a terminationGracePeriodSeconds, in the place of KubeVirt.
func createVMIWithGracePeriod(ns, name string, seconds int64) {
	vmi := newVMI(ns, name)
	vmi.Object["spec"] = map[string]any{"terminationGracePeriodSeconds": seconds}
	Expect(k8sClient.Create(ctx, vmi)).To(Succeed())
}

// applicationsIn returns the applications of the findings.
func applicationsIn(findings []shutdownv1alpha1.Finding) []string {
	var apps []string
	for _, f := range findings {
		apps = append(apps, f.Applications...)
	}
	return apps
}

var _ = Describe("The cluster checks of a ShutdownPlan", func() {
	var (
		ns   string
		stop func()
	)

	BeforeEach(func() {
		deleteAllWorkloads()
		ns = newNamespace()
	})

	AfterEach(func() {
		stop()
		cleanUpPlans()
		cleanUp(ns)
	})

	It("reports a patch that the API server rejects in the dry run as an Error, for an application and for a Flux controller", func() {
		fluxNS := newNamespace()
		DeferCleanup(cleanUp, fluxNS)
		kustomize := createGuardedDeployment(fluxNS, "kustomize-controller", 3)
		stop, _ = startOperator(operator.Config{
			FluxControllers: []types.NamespacedName{{Namespace: fluxNS, Name: "kustomize-controller"}},
			FluxStopTimeout: time.Minute,
		})
		guarded := createGuardedDeployment(ns, "guarded", 3)
		createApplication(ns, "guarded", workload(shutdownv1alpha1.WorkloadKindDeployment, "guarded"))

		status := createPlan()

		Expect(status.Result).To(Equal(shutdownv1alpha1.PlanResultErrors))
		rejected := findingsWith(status, shutdownv1alpha1.ReasonPatchRejected)
		Expect(rejected).To(HaveLen(2))
		Expect(rejected).To(HaveEach(HaveField("Severity", shutdownv1alpha1.SeverityError)))
		Expect(rejected).To(ContainElement(SatisfyAll(
			HaveField("Applications", ConsistOf(ns+"/guarded")),
			HaveField("Message", ContainSubstring("scale to 0 is not allowed")),
			HaveField("Fallback", ContainSubstring("status.failed")),
		)))
		// A Flux controller belongs to no application.
		Expect(rejected).To(ContainElement(SatisfyAll(
			HaveField("Applications", BeEmpty()),
			HaveField("Message", ContainSubstring(fluxNS+"/kustomize-controller")),
			HaveField("Fallback", ContainSubstring("time limit of the Flux step")),
		)))
		Expect(status.Report).To(ContainSubstring("PatchRejected"))
		Expect(status.Report).To(ContainSubstring(fluxNS + "/kustomize-controller"))
		Expect(replicasOf(guarded)()).To(Equal(int32(3)))
		Expect(replicasOf(kustomize)()).To(Equal(int32(3)))
	})

	It("reports a rejected dry run for the patch of a StatefulSet, a CronJob, and a VM with the running field", func() {
		stop, _ = startOperator(operator.Config{})
		// Each policy rejects only the patch that a real shutdown sends, so a wrong patch gives no finding.
		db := createStatefulSet(ns, "db", 1)
		rejectUpdates(ns, "apps", "statefulsets", "object.spec.replicas != 0", "scale to 0 is not allowed")
		waitForRejection(db, client.RawPatch(types.MergePatchType, []byte(`{"spec":{"replicas":0}}`)), "scale to 0 is not allowed")
		report := createCronJob(ns, "report", false)
		rejectUpdates(ns, "batch", "cronjobs", "!has(object.spec.suspend) || !object.spec.suspend", "suspend is not allowed")
		waitForRejection(report, client.RawPatch(types.MergePatchType, []byte(`{"spec":{"suspend":true}}`)), "suspend is not allowed")
		createVM(ns, "legacy-vm", map[string]any{"running": true})
		legacy := newKubeVirtObject("VirtualMachine", ns, "legacy-vm")
		rejectUpdates(ns, "kubevirt.io", "virtualmachines", "!has(object.spec.running) || object.spec.running", "halt is not allowed")
		waitForRejection(legacy, client.RawPatch(types.MergePatchType, []byte(`{"spec":{"running":false}}`)), "halt is not allowed")
		createApplication(ns, "database", workload(shutdownv1alpha1.WorkloadKindStatefulSet, "db"))
		createApplication(ns, "reports", workload(shutdownv1alpha1.WorkloadKindCronJob, "report"))
		createApplication(ns, "legacy", workload(shutdownv1alpha1.WorkloadKindVirtualMachine, "legacy-vm"))

		status := createPlan()

		Expect(status.Result).To(Equal(shutdownv1alpha1.PlanResultErrors))
		rejected := findingsWith(status, shutdownv1alpha1.ReasonPatchRejected)
		Expect(rejected).To(HaveEach(HaveField("Severity", shutdownv1alpha1.SeverityError)))
		Expect(rejected).To(ConsistOf(
			SatisfyAll(
				HaveField("Applications", ConsistOf(ns+"/database")),
				HaveField("Message", ContainSubstring("scale to 0 is not allowed")),
			),
			SatisfyAll(
				HaveField("Applications", ConsistOf(ns+"/reports")),
				HaveField("Message", ContainSubstring("suspend is not allowed")),
			),
			SatisfyAll(
				HaveField("Applications", ConsistOf(ns+"/legacy")),
				HaveField("Message", ContainSubstring("halt is not allowed")),
			),
		))
		// A dry run changes no workload.
		Expect(replicasOf(db)()).To(Equal(int32(1)))
		Expect(suspendOf(report)()).To(BeFalse())
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(legacy), legacy)).To(Succeed())
		Expect(legacy.Object["spec"]).To(HaveKeyWithValue("running", true))
	})

	It("sends no dry run for a workload that a real shutdown does not patch, because its replicas are already 0", func() {
		fluxNS := newNamespace()
		DeferCleanup(cleanUp, fluxNS)
		createGuardedDeployment(fluxNS, "kustomize-controller", 0)
		stop, _ = startOperator(operator.Config{
			FluxControllers: []types.NamespacedName{{Namespace: fluxNS, Name: "kustomize-controller"}},
			FluxStopTimeout: time.Minute,
		})
		createGuardedDeployment(ns, "idle", 0)
		createApplication(ns, "idle", workload(shutdownv1alpha1.WorkloadKindDeployment, "idle"))

		status := createPlan()

		Expect(status.Result).To(Equal(shutdownv1alpha1.PlanResultOK))
		Expect(findingsWith(status, shutdownv1alpha1.ReasonPatchRejected)).To(BeEmpty())
	})

	It("sends no dry run for a RerunOnFailure VM whose VMI is in deletion, because a real shutdown does not halt it", func() {
		stop, _ = startOperator(operator.Config{})
		guardVMs(ns)
		createVM(ns, "rerun-on", map[string]any{"runStrategy": "RerunOnFailure"})
		createVMI(ns, "rerun-on")
		// A stop by a user just before the shutdown deletes the VMI.
		createVM(ns, "rerun-stopping", map[string]any{"runStrategy": "RerunOnFailure"})
		createVMI(ns, "rerun-stopping")
		terminateVMI(ns, "rerun-stopping")
		createApplication(ns, "on", workload(shutdownv1alpha1.WorkloadKindVirtualMachine, "rerun-on"))
		createApplication(ns, "stopping", workload(shutdownv1alpha1.WorkloadKindVirtualMachine, "rerun-stopping"))

		status := createPlan()

		Expect(applicationsIn(findingsWith(status, shutdownv1alpha1.ReasonPatchRejected))).To(ConsistOf(ns + "/on"))
	})

	It("reports a dry run that fails with a server error as an Error, and still writes the plan", func() {
		stop, _ = startOperator(operator.Config{})
		createStuckDeployment(ns, "web")
		createApplication(ns, "shop", workload(shutdownv1alpha1.WorkloadKindDeployment, "web"))

		status := createPlan()

		Expect(status.Result).To(Equal(shutdownv1alpha1.PlanResultErrors))
		Expect(findingsWith(status, shutdownv1alpha1.ReasonPatchFailed)).To(ConsistOf(SatisfyAll(
			HaveField("Severity", shutdownv1alpha1.SeverityError),
			HaveField("Applications", ConsistOf(ns+"/shop")),
			HaveField("Message", ContainSubstring("failed calling webhook")),
			HaveField("Fallback", ContainSubstring("forced stop")),
		)))
		Expect(findingsWith(status, shutdownv1alpha1.ReasonPatchRejected)).To(BeEmpty())
		Expect(status.Report).To(ContainSubstring("PatchFailed"))
	})

	It("reports each unlisted workload that runs as Info, but not a Flux controller, and keeps the result OK", func() {
		fluxNS := newNamespace()
		DeferCleanup(cleanUp, fluxNS)
		createDeployment(fluxNS, "kustomize-controller", 1)
		stop, _ = startOperator(operator.Config{
			FluxControllers: []types.NamespacedName{{Namespace: fluxNS, Name: "kustomize-controller"}},
			FluxStopTimeout: time.Minute,
		})
		createDeployment(ns, "web", 1)
		createApplication(ns, "shop", workload(shutdownv1alpha1.WorkloadKindDeployment, "web"))
		createDeployment(ns, "stray", 2)
		createStatefulSet(ns, "stray-db", 1)
		createDeployment(ns, "idle", 0)
		createVM(ns, "stray-vm", map[string]any{"runStrategy": "Always"})
		createVMI(ns, "stray-vm")
		createVM(ns, "halted-vm", map[string]any{"runStrategy": "Halted"})
		// The run strategy Always tells that the VM runs, so the check does not read its VMI.
		createVM(ns, "starting-vm", map[string]any{"runStrategy": "Always"})
		// The run strategy Manual does not tell this, so the check reads the VMI.
		createVM(ns, "manual-vm", map[string]any{"runStrategy": "Manual"})
		createVMI(ns, "manual-vm")
		createVM(ns, "stopped-manual-vm", map[string]any{"runStrategy": "Manual"})
		// A VMI in a final phase has no running guest.
		createVM(ns, "finished-manual-vm", map[string]any{"runStrategy": "Manual"})
		createVMI(ns, "finished-manual-vm")
		setVMIPhase(ns, "finished-manual-vm", "Succeeded")

		status := createPlan()

		// System workloads are always unmanaged, so their findings must not change the result.
		Expect(status.Result).To(Equal(shutdownv1alpha1.PlanResultOK))
		unmanaged := findingsWith(status, shutdownv1alpha1.ReasonUnmanagedWorkload)
		Expect(unmanaged).To(HaveEach(SatisfyAll(
			HaveField("Severity", shutdownv1alpha1.SeverityInfo),
			HaveField("Applications", BeEmpty()),
		)))
		Expect(unmanaged).To(ConsistOf(
			HaveField("Message", ContainSubstring("Deployment "+ns+"/stray ")),
			HaveField("Message", ContainSubstring("StatefulSet "+ns+"/stray-db ")),
			HaveField("Message", ContainSubstring("VirtualMachine "+ns+"/stray-vm ")),
			HaveField("Message", ContainSubstring("VirtualMachine "+ns+"/starting-vm ")),
			HaveField("Message", ContainSubstring("VirtualMachine "+ns+"/manual-vm ")),
		))
		Expect(status.Report).To(ContainSubstring("UnmanagedWorkload"))
	})

	It("reports a terminationGracePeriodSeconds that is longer or shorter than the gracePeriod as a Warning, for pods and for VMs", func() {
		stop, _ = startOperator(operator.Config{})
		web := createDeployment(ns, "web", 1)
		setTerminationGracePeriod(web, 60)
		db := createStatefulSet(ns, "db", 1)
		setTerminationGracePeriod(db, 10)
		createDeployment(ns, "api", 1)
		createVM(ns, "slow-vm", map[string]any{
			"runStrategy": "Always",
			"template":    map[string]any{"spec": map[string]any{"terminationGracePeriodSeconds": int64(120)}},
		})
		// An unset VM value counts as 30 seconds.
		createVM(ns, "windows-vm", map[string]any{"runStrategy": "Always"})
		createVM(ns, "linux-vm", map[string]any{"runStrategy": "Always"})
		// The VMI value wins over the VM template, for example from a VirtualMachinePreference or before a restart.
		createVM(ns, "preferred-vm", map[string]any{"runStrategy": "Always"})
		createVMIWithGracePeriod(ns, "preferred-vm", 120)
		createVM(ns, "changed-vm", map[string]any{
			"runStrategy": "Always",
			"template":    map[string]any{"spec": map[string]any{"terminationGracePeriodSeconds": int64(120)}},
		})
		createVMIWithGracePeriod(ns, "changed-vm", 30)
		// A real shutdown stops no pod of a workload that does not run, so these give no finding.
		setTerminationGracePeriod(createDeployment(ns, "idle", 0), 60)
		createVM(ns, "halted-vm", map[string]any{
			"runStrategy": "Halted",
			"template":    map[string]any{"spec": map[string]any{"terminationGracePeriodSeconds": int64(120)}},
		})
		createApplication(ns, "idle", workload(shutdownv1alpha1.WorkloadKindDeployment, "idle"))
		createApplication(ns, "halted", workload(shutdownv1alpha1.WorkloadKindVirtualMachine, "halted-vm"))
		createApplication(ns, "frontend", workload(shutdownv1alpha1.WorkloadKindDeployment, "web"))
		createApplication(ns, "database", workload(shutdownv1alpha1.WorkloadKindStatefulSet, "db"))
		createApplication(ns, "api", workload(shutdownv1alpha1.WorkloadKindDeployment, "api"))
		createApplication(ns, "slow", workload(shutdownv1alpha1.WorkloadKindVirtualMachine, "slow-vm"))
		createApplication(ns, "windows", workload(shutdownv1alpha1.WorkloadKindVirtualMachine, "windows-vm"))
		createApplication(ns, "linux", workload(shutdownv1alpha1.WorkloadKindVirtualMachine, "linux-vm"))
		createApplication(ns, "preferred", workload(shutdownv1alpha1.WorkloadKindVirtualMachine, "preferred-vm"))
		createApplication(ns, "changed", workload(shutdownv1alpha1.WorkloadKindVirtualMachine, "changed-vm"))
		setGracePeriod(ns, "windows", time.Minute)

		status := createPlan()

		Expect(status.Result).To(Equal(shutdownv1alpha1.PlanResultWarnings))
		longer := findingsWith(status, shutdownv1alpha1.ReasonTerminationGracePeriodLonger)
		Expect(longer).To(HaveEach(SatisfyAll(
			HaveField("Severity", shutdownv1alpha1.SeverityWarning),
			HaveField("Fallback", ContainSubstring("grace period of 0")),
		)))
		Expect(applicationsIn(longer)).To(ConsistOf(ns+"/frontend", ns+"/slow", ns+"/preferred"))
		// With no VMI, the value comes from the VM template, and a preference can change it.
		Expect(longer).To(ContainElement(HaveField("Message", SatisfyAll(
			ContainSubstring("VirtualMachine slow-vm is 2m0s"),
			ContainSubstring("preferredTerminationGracePeriodSeconds"),
		))))
		Expect(longer).To(ContainElement(HaveField("Message", SatisfyAll(
			ContainSubstring("VirtualMachine preferred-vm is 2m0s"),
			Not(ContainSubstring("preferredTerminationGracePeriodSeconds")),
		))))

		shorter := findingsWith(status, shutdownv1alpha1.ReasonTerminationGracePeriodShorter)
		Expect(shorter).To(HaveEach(HaveField("Severity", shutdownv1alpha1.SeverityWarning)))
		Expect(applicationsIn(shorter)).To(ConsistOf(ns+"/database", ns+"/windows"))
		Expect(shorter).To(ContainElement(HaveField("Message", ContainSubstring("VirtualMachine windows-vm is 30s"))))
		Expect(status.Report).To(ContainSubstring("TerminationGracePeriodLonger"))
	})

	It("reports a boot ID file that the operator cannot read as an Error", func() {
		stop, _ = startOperator(operator.Config{BootIDFile: filepath.Join(GinkgoT().TempDir(), "missing")})

		status := createPlan()

		Expect(status.Result).To(Equal(shutdownv1alpha1.PlanResultErrors))
		bootID := findingsWith(status, shutdownv1alpha1.ReasonUnreadableBootID)
		Expect(bootID).To(HaveLen(1))
		Expect(bootID[0].Severity).To(Equal(shutdownv1alpha1.SeverityError))
		Expect(bootID[0].Fallback).To(ContainSubstring("power cycle"))
		Expect(status.Report).To(ContainSubstring("UnreadableBootID"))
	})
})
