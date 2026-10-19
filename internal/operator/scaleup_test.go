//go:build envtest

package operator_test

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	shutdownv1alpha1 "github.com/giantswarm/pod-shutdown-operator/api/v1alpha1"
	"github.com/giantswarm/pod-shutdown-operator/internal/operator"
)

var _ = Describe("A scale-up from the record", func() {
	var (
		ns   string
		stop func()
	)

	BeforeEach(func() {
		ns = newNamespace()
		stop, _ = startOperator(operator.Config{})
	})

	AfterEach(func() {
		stop()
		cleanUp(ns)
	})

	It("moves a Complete request to ScalingUp on cancel and restores the recorded counts", func() {
		web := createDeployment(ns, "web", 3)
		db := createStatefulSet(ns, "db", 2)
		createApplication(ns, "shop",
			workload(shutdownv1alpha1.WorkloadKindDeployment, "web"),
			workload(shutdownv1alpha1.WorkloadKindStatefulSet, "db"),
		)
		completeShutdown()

		setAction(shutdownv1alpha1.ActionCancel)

		Eventually(phaseOf).Should(Equal(shutdownv1alpha1.PhaseScalingUp))
		Eventually(replicasOf(web)).Should(Equal(int32(3)))
		Eventually(replicasOf(db)).Should(Equal(int32(2)))
		Consistently(phaseOf, time.Second).Should(Equal(shutdownv1alpha1.PhaseScalingUp))

		markReady(web, 3)
		markReady(db, 2)

		Eventually(phaseOf).Should(Equal(shutdownv1alpha1.PhaseRestored))
		Expect(getRequest().Status.Degraded).To(BeEmpty())
	})

	It("sets scaleUpStartedAt in the update that sets ScalingUp", func() {
		createDeployment(ns, "web", 1)
		createApplication(ns, "shop", workload(shutdownv1alpha1.WorkloadKindDeployment, "web"))
		completeShutdown()
		scalingUpWithNoTime := watchFirst(&shutdownv1alpha1.ShutdownRequestList{}, "", func(o runtime.Object) bool {
			status := o.(*shutdownv1alpha1.ShutdownRequest).Status
			return status.Phase == shutdownv1alpha1.PhaseScalingUp && status.ScaleUpStartedAt == nil
		})

		setAction(shutdownv1alpha1.ActionCancel)

		Eventually(phaseOf).Should(Equal(shutdownv1alpha1.PhaseScalingUp))
		Expect(getRequest().Status.ScaleUpStartedAt).NotTo(BeNil())
		Consistently(scalingUpWithNoTime, time.Second).ShouldNot(Receive())
	})

	It("moves a ShuttingDown request to ScalingUp on cancel", func() {
		web := createDeployment(ns, "web", 2)
		createPod(ns, "web-0", appLabels("web"))
		createApplication(ns, "shop", workload(shutdownv1alpha1.WorkloadKindDeployment, "web"))
		createShutdownRequest(shutdownv1alpha1.ActionShutdown)
		Eventually(replicasOf(web)).Should(BeZero())
		Expect(phaseOf()).To(Equal(shutdownv1alpha1.PhaseShuttingDown))

		setAction(shutdownv1alpha1.ActionCancel)

		Eventually(phaseOf).Should(Equal(shutdownv1alpha1.PhaseScalingUp))
		Eventually(replicasOf(web)).Should(Equal(int32(2)))
		// The stop loop has ended, so no late scale-down follows the scale-up.
		Consistently(replicasOf(web), time.Second).Should(Equal(int32(2)))

		markReady(web, 2)
		Eventually(phaseOf).Should(Equal(shutdownv1alpha1.PhaseRestored))
	})

	It("does nothing on a cancel in a request with no phase", func() {
		web := createDeployment(ns, "web", 2)
		createApplication(ns, "shop", workload(shutdownv1alpha1.WorkloadKindDeployment, "web"))

		createShutdownRequest(shutdownv1alpha1.ActionCancel)

		Consistently(phaseOf, time.Second).Should(BeEmpty())
		Expect(getRequest().Status.RecordedState).To(BeNil())
		Expect(replicasOf(web)()).To(Equal(int32(2)))
	})

	It("does not change workloads in a Restored request", func() {
		web := createDeployment(ns, "web", 2)
		createApplication(ns, "shop", workload(shutdownv1alpha1.WorkloadKindDeployment, "web"))
		completeShutdown()
		setAction(shutdownv1alpha1.ActionCancel)
		Eventually(replicasOf(web)).Should(Equal(int32(2)))
		markReady(web, 2)
		Eventually(phaseOf).Should(Equal(shutdownv1alpha1.PhaseRestored))

		patch := client.RawPatch(types.MergePatchType, []byte(`{"spec":{"replicas":5}}`))
		Expect(k8sClient.Patch(ctx, web, patch)).To(Succeed())

		Consistently(replicasOf(web), time.Second).Should(Equal(int32(5)))
		Expect(phaseOf()).To(Equal(shutdownv1alpha1.PhaseRestored))
	})

	It("marks an application that is not ready at its readyTimeout as Degraded and starts the others", func() {
		slow := createDeployment(ns, "slow", 1)
		fast := createDeployment(ns, "fast", 2)
		createApplication(ns, "slow", workload(shutdownv1alpha1.WorkloadKindDeployment, "slow"))
		createApplication(ns, "fast", workload(shutdownv1alpha1.WorkloadKindDeployment, "fast"))
		setReadyTimeout(ns, "slow", time.Second)
		completeShutdown()

		setAction(shutdownv1alpha1.ActionCancel)

		Eventually(replicasOf(fast)).Should(Equal(int32(2)))
		markReady(fast, 2)

		Eventually(phaseOf).Should(Equal(shutdownv1alpha1.PhaseRestored))
		Expect(getRequest().Status.Degraded).To(ConsistOf(ns + "/slow"))
		Expect(replicasOf(slow)()).To(Equal(int32(1)))
	})

	It("waits for Deployment availableReplicas and StatefulSet readyReplicas of the current generation", func() {
		web := createDeployment(ns, "web", 2)
		db := createStatefulSet(ns, "db", 1)
		createApplication(ns, "shop",
			workload(shutdownv1alpha1.WorkloadKindDeployment, "web"),
			workload(shutdownv1alpha1.WorkloadKindStatefulSet, "db"),
		)
		completeShutdown()
		setAction(shutdownv1alpha1.ActionCancel)
		Eventually(replicasOf(web)).Should(Equal(int32(2)))
		Eventually(replicasOf(db)).Should(Equal(int32(1)))

		// The Deployment pods are ready, but not available.
		setWorkloadStatus(web, 0, 2, 0)
		markReady(db, 1)
		Consistently(phaseOf, time.Second).Should(Equal(shutdownv1alpha1.PhaseScalingUp))

		// The StatefulSet status is from the generation before the scale-up.
		// The StatefulSet becomes stale first, so that no reconcile sees both workloads ready.
		setWorkloadStatus(db, 1, 1, 1)
		markReady(web, 2)
		Consistently(phaseOf, time.Second).Should(Equal(shutdownv1alpha1.PhaseScalingUp))

		markReady(db, 1)
		Eventually(phaseOf).Should(Equal(shutdownv1alpha1.PhaseRestored))
	})

	It("counts a recorded workload that no longer exists as ready", func() {
		web := createDeployment(ns, "web", 1)
		createApplication(ns, "shop", workload(shutdownv1alpha1.WorkloadKindDeployment, "web"))
		completeShutdown()
		Expect(k8sClient.Delete(ctx, web)).To(Succeed())

		setAction(shutdownv1alpha1.ActionCancel)

		Eventually(phaseOf).Should(Equal(shutdownv1alpha1.PhaseRestored))
		Expect(getRequest().Status.Degraded).To(BeEmpty())
	})

	It("starts the readyTimeout again after an operator restart", func() {
		web := createDeployment(ns, "web", 1)
		createApplication(ns, "shop", workload(shutdownv1alpha1.WorkloadKindDeployment, "web"))
		setReadyTimeout(ns, "shop", 2*time.Second)
		completeShutdown()
		setAction(shutdownv1alpha1.ActionCancel)
		Eventually(replicasOf(web)).Should(Equal(int32(1)))
		degradedOf := func() []string { return getRequest().Status.Degraded }
		Consistently(degradedOf, 1500*time.Millisecond).Should(BeEmpty())

		stop()
		stop, _ = startOperator(operator.Config{})

		// More than 2s pass after the scale-up started, but the new operator starts the clock again.
		Consistently(degradedOf, time.Second).Should(BeEmpty())
		Eventually(degradedOf).Should(ConsistOf(ns + "/shop"))
		Expect(phaseOf()).To(Equal(shutdownv1alpha1.PhaseRestored))
	})
})
