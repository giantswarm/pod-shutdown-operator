//go:build envtest

package operator_test

import (
	"slices"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	shutdownv1alpha1 "gitlab.opendefense.cloud/odc/shutdown-op/pod-shutdown-operator/api/v1alpha1"
	"gitlab.opendefense.cloud/odc/shutdown-op/pod-shutdown-operator/internal/operator"
)

var _ = Describe("The Deployment of the operator", func() {
	var (
		ns   string
		stop func()
	)

	BeforeEach(func() {
		deleteAllWorkloads()
		ns = newNamespace()
		stop, _ = startOperator(operator.Config{
			OperatorDeployment: types.NamespacedName{Namespace: ns, Name: "pod-shutdown-operator"},
		})
	})

	AfterEach(func() {
		stop()
		cleanUpPlans()
		cleanUp(ns)
	})

	It("is not in the record of a real shutdown when an application lists it, and keeps its replicas", func() {
		self := createDeployment(ns, "pod-shutdown-operator", 2)
		createDeployment(ns, "web", 1)
		createApplication(ns, "shop",
			workload(shutdownv1alpha1.WorkloadKindDeployment, "web"),
			workload(shutdownv1alpha1.WorkloadKindDeployment, "pod-shutdown-operator"),
		)

		createShutdownRequest(shutdownv1alpha1.ActionShutdown)

		Eventually(phaseOf).Should(Equal(shutdownv1alpha1.PhaseComplete))
		Expect(getRequest().Status.RecordedState.Applications).To(ConsistOf(
			shutdownv1alpha1.RecordedApplication{Namespace: ns, Name: "shop", Workloads: []shutdownv1alpha1.RecordedWorkload{
				{Kind: shutdownv1alpha1.WorkloadKindDeployment, Name: "web", Replicas: ptr.To[int32](1)},
			}},
		))
		Expect(replicasOf(self)()).To(Equal(int32(2)))
	})

	It("is not an unmanaged workload when no application lists it", func() {
		createDeployment(ns, "pod-shutdown-operator", 1)
		createDeployment(ns, "unlisted", 1)
		createDeployment(ns, "web", 1)
		createPod(ns, "web-0", appLabels("web"))
		createApplication(ns, "shop", workload(shutdownv1alpha1.WorkloadKindDeployment, "web"))

		createShutdownRequest(shutdownv1alpha1.ActionShutdown)

		Eventually(phaseOf).Should(Equal(shutdownv1alpha1.PhaseShuttingDown))
		// Other tests leave workloads in their namespaces.
		Expect(slices.DeleteFunc(getRequest().Status.Unmanaged, func(w shutdownv1alpha1.UnmanagedWorkload) bool {
			return w.Namespace != ns
		})).To(ConsistOf(shutdownv1alpha1.UnmanagedWorkload{Namespace: ns, Kind: shutdownv1alpha1.WorkloadKindDeployment, Name: "unlisted"}))
	})

	It("gives an Error in the dry run when an application lists it", func() {
		createDeployment(ns, "pod-shutdown-operator", 1)
		createApplication(ns, "shop", workload(shutdownv1alpha1.WorkloadKindDeployment, "pod-shutdown-operator"))

		status := createPlan()

		Expect(status.Result).To(Equal(shutdownv1alpha1.PlanResultErrors))
		Expect(findingsWith(status, shutdownv1alpha1.ReasonOperatorWorkload)).To(ConsistOf(SatisfyAll(
			HaveField("Severity", shutdownv1alpha1.SeverityError),
			HaveField("Applications", ConsistOf(ns+"/shop")),
			HaveField("Message", ContainSubstring("Deployment pod-shutdown-operator")),
			HaveField("Fallback", ContainSubstring("Skip the workload")),
		)))
		Expect(findingsWith(status, shutdownv1alpha1.ReasonMissingWorkload)).To(BeEmpty())
		Expect(findingsWith(status, shutdownv1alpha1.ReasonUnmanagedWorkload)).To(BeEmpty())
	})
})
