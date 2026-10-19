//go:build envtest

package operator_test

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/utils/ptr"

	shutdownv1alpha1 "github.com/giantswarm/pod-shutdown-operator/api/v1alpha1"
	"github.com/giantswarm/pod-shutdown-operator/internal/operator"
)

var _ = Describe("The record at the start of a shutdown", func() {
	var (
		ns1, ns2 string
		stop     func()
	)

	BeforeEach(func() {
		ns1, ns2 = newNamespace(), newNamespace()
		stop, _ = startOperator(operator.Config{})
	})

	AfterEach(func() {
		stop()
		cleanUp(ns1, ns2)
	})

	It("records the replicas of each listed workload and sets ShuttingDown", func() {
		createDeployment(ns1, "web", 3)
		createStatefulSet(ns1, "db", 2)
		createDeployment(ns1, "unlisted", 4)
		createDeployment(ns2, "api", 1)
		// A Pod that stays keeps the request in ShuttingDown.
		createPod(ns1, "web-0", appLabels("web"))
		createApplication(ns1, "shop",
			workload(shutdownv1alpha1.WorkloadKindDeployment, "web"),
			workload(shutdownv1alpha1.WorkloadKindStatefulSet, "db"),
			workload(shutdownv1alpha1.WorkloadKindDeployment, "missing"),
			workload(shutdownv1alpha1.WorkloadKindVirtualMachine, "vm"),
			workload(shutdownv1alpha1.WorkloadKindCronJob, "job"),
		)
		// The same application name in another namespace is a different application.
		createApplication(ns2, "shop", workload(shutdownv1alpha1.WorkloadKindDeployment, "api"))
		before := time.Now().Truncate(time.Second)

		createShutdownRequest(shutdownv1alpha1.ActionShutdown)

		Eventually(phaseOf).Should(Equal(shutdownv1alpha1.PhaseShuttingDown))
		status := getRequest().Status
		Expect(status.Total).To(Equal(int32(2)))
		Expect(status.ShutdownStartedAt).NotTo(BeNil())
		Expect(status.ShutdownStartedAt.Time).To(BeTemporally(">=", before))
		Expect(status.RecordedState).NotTo(BeNil())
		Expect(status.RecordedState.Applications).To(ConsistOf(
			shutdownv1alpha1.RecordedApplication{Namespace: ns1, Name: "shop", Workloads: []shutdownv1alpha1.RecordedWorkload{
				{Kind: shutdownv1alpha1.WorkloadKindDeployment, Name: "web", Replicas: ptr.To[int32](3)},
				{Kind: shutdownv1alpha1.WorkloadKindStatefulSet, Name: "db", Replicas: ptr.To[int32](2)},
			}},
			shutdownv1alpha1.RecordedApplication{Namespace: ns2, Name: "shop", Workloads: []shutdownv1alpha1.RecordedWorkload{
				{Kind: shutdownv1alpha1.WorkloadKindDeployment, Name: "api", Replicas: ptr.To[int32](1)},
			}},
		))
	})

	It("writes nothing for a request with the action Cancel and no phase", func() {
		web := createDeployment(ns1, "web", 1)
		createApplication(ns1, "shop", workload(shutdownv1alpha1.WorkloadKindDeployment, "web"))

		createShutdownRequest(shutdownv1alpha1.ActionCancel)

		Consistently(func() shutdownv1alpha1.ShutdownRequestStatus { return getRequest().Status }, 2*time.Second).Should(BeZero())
		Expect(replicasOf(web)()).To(Equal(int32(1)))
	})
})
