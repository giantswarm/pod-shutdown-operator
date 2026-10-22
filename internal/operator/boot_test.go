//go:build envtest

package operator_test

import (
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	clocktesting "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"

	shutdownv1alpha1 "github.com/giantswarm/pod-shutdown-operator/api/v1alpha1"
	"github.com/giantswarm/pod-shutdown-operator/internal/operator"
)

// start starts an operator and stops it at the test cleanup.
func start(c operator.Config) func() {
	stop, _ := startOperator(c)
	DeferCleanup(stop)
	return stop
}

func actionOf() shutdownv1alpha1.Action {
	return getRequest().Spec.Action
}

var _ = Describe("The boot IDs in the record", func() {
	var ns string

	BeforeEach(func() {
		ns = newNamespace()
		DeferCleanup(cleanUp, ns)
		createDeployment(ns, "web", 1)
		// A Pod that stays keeps the request in ShuttingDown.
		createPod(ns, "web-0", appLabels("web"))
		createApplication(ns, "shop", workload(shutdownv1alpha1.WorkloadKindDeployment, "web"))
	})

	It("holds the own entry from the boot ID file and the others from the Node objects", func() {
		createNode("worker-1", "worker-boot")
		createNode(operatorNode, "node-object-boot")
		start(operator.Config{BootIDFile: bootIDFile("file-boot")})

		createShutdownRequest(shutdownv1alpha1.ActionShutdown)

		Eventually(phaseOf).Should(Equal(shutdownv1alpha1.PhaseShuttingDown))
		bootIDs := getRequest().Status.BootIDs
		Expect(bootIDs).To(HaveKeyWithValue("worker-1", "worker-boot"))
		Expect(bootIDs).To(HaveKeyWithValue(operatorNode, "file-boot"))
	})

	It("holds no own entry when the boot ID file is unreadable, and the shutdown goes on", func() {
		createNode(operatorNode, "node-object-boot")
		start(operator.Config{BootIDFile: filepath.Join(GinkgoT().TempDir(), "missing")})

		createShutdownRequest(shutdownv1alpha1.ActionShutdown)

		Eventually(phaseOf).Should(Equal(shutdownv1alpha1.PhaseShuttingDown))
		Expect(getRequest().Status.BootIDs).NotTo(HaveKey(operatorNode))
		deletePod(ns, "web-0")
		Eventually(phaseOf).Should(Equal(shutdownv1alpha1.PhaseComplete))
	})

	It("is written again when a ScalingUp request returns to ShuttingDown", func() {
		stop := start(operator.Config{})
		createShutdownRequest(shutdownv1alpha1.ActionShutdown)
		deletePod(ns, "web-0")
		Eventually(phaseOf).Should(Equal(shutdownv1alpha1.PhaseComplete))

		// A power cycle starts the scale-up, and a new shutdown comes during the scale-up.
		stop()
		stop = start(operator.Config{BootIDFile: bootIDFile("boot-2")})
		Eventually(phaseOf).Should(Equal(shutdownv1alpha1.PhaseScalingUp))
		setAction(shutdownv1alpha1.ActionShutdown)
		Eventually(phaseOf).Should(Equal(shutdownv1alpha1.PhaseComplete))
		Expect(getRequest().Status.BootIDs).To(HaveKeyWithValue(operatorNode, "boot-2"))

		// The next restart on the same boot is not a power cycle.
		stop()
		start(operator.Config{BootIDFile: bootIDFile("boot-2")})
		Consistently(phaseOf, time.Second).Should(Equal(shutdownv1alpha1.PhaseComplete))
		Expect(actionOf()).To(Equal(shutdownv1alpha1.ActionShutdown))
	})
})

var _ = Describe("An operator start", func() {
	var ns string

	BeforeEach(func() {
		ns = newNamespace()
		DeferCleanup(cleanUp, ns)
	})

	It("does nothing with no request", func() {
		web := createDeployment(ns, "web", 2)
		createApplication(ns, "shop", workload(shutdownv1alpha1.WorkloadKindDeployment, "web"))

		start(operator.Config{BootIDFile: bootIDFile("boot-2")})

		Consistently(func() bool {
			err := k8sClient.Get(ctx, client.ObjectKey{Name: shutdownv1alpha1.ShutdownRequestName}, &shutdownv1alpha1.ShutdownRequest{})
			return apierrors.IsNotFound(err)
		}, time.Second).Should(BeTrue())
		Expect(replicasOf(web)()).To(Equal(int32(2)))
	})

	It("does nothing in Restored, also with a changed boot ID", func() {
		web := createDeployment(ns, "web", 2)
		createApplication(ns, "shop", workload(shutdownv1alpha1.WorkloadKindDeployment, "web"))
		stop := start(operator.Config{})
		completeShutdown()
		setAction(shutdownv1alpha1.ActionCancel)
		Eventually(replicasOf(web)).Should(Equal(int32(2)))
		markReady(web, 2)
		Eventually(phaseOf).Should(Equal(shutdownv1alpha1.PhaseRestored))
		patch := client.RawPatch(types.MergePatchType, []byte(`{"spec":{"replicas":5}}`))
		Expect(k8sClient.Patch(ctx, web, patch)).To(Succeed())
		before := getRequest()

		stop()
		start(operator.Config{BootIDFile: bootIDFile("boot-2")})

		Consistently(replicasOf(web), time.Second).Should(Equal(int32(5)))
		Expect(getRequest().ResourceVersion).To(Equal(before.ResourceVersion))
	})

	It("continues a ShuttingDown request when the boot ID is equal", func() {
		web := createDeployment(ns, "web", 2)
		createPod(ns, "web-0", appLabels("web"))
		createApplication(ns, "shop", workload(shutdownv1alpha1.WorkloadKindDeployment, "web"))
		stop := start(operator.Config{})
		createShutdownRequest(shutdownv1alpha1.ActionShutdown)
		Eventually(replicasOf(web)).Should(BeZero())

		stop()
		start(operator.Config{})

		Consistently(phaseOf, time.Second).Should(Equal(shutdownv1alpha1.PhaseShuttingDown))
		Expect(replicasOf(web)()).To(BeZero())
		deletePod(ns, "web-0")
		Eventually(phaseOf).Should(Equal(shutdownv1alpha1.PhaseComplete))
	})

	It("keeps a Complete request when the boot ID is equal", func() {
		web := createDeployment(ns, "web", 2)
		createApplication(ns, "shop", workload(shutdownv1alpha1.WorkloadKindDeployment, "web"))
		stop := start(operator.Config{})
		completeShutdown()

		stop()
		start(operator.Config{})

		Consistently(phaseOf, time.Second).Should(Equal(shutdownv1alpha1.PhaseComplete))
		Expect(replicasOf(web)()).To(BeZero())
		Expect(actionOf()).To(Equal(shutdownv1alpha1.ActionShutdown))
	})

	It("keeps a Complete request when the leader moves to a node with an equal boot ID in the record", func() {
		createNode("node-b", "boot-b")
		createApplication(ns, "shop", workload(shutdownv1alpha1.WorkloadKindDeployment, "web"))
		createDeployment(ns, "web", 2)
		stop := start(operator.Config{})
		completeShutdown()

		stop()
		start(operator.Config{NodeName: "node-b", BootIDFile: bootIDFile("boot-b")})

		Consistently(phaseOf, time.Second).Should(Equal(shutdownv1alpha1.PhaseComplete))
		Expect(actionOf()).To(Equal(shutdownv1alpha1.ActionShutdown))
	})

	DescribeTable("scales up from the record after a power cycle",
		func(phase shutdownv1alpha1.Phase, restart func() operator.Config) {
			web := createDeployment(ns, "web", 2)
			if phase == shutdownv1alpha1.PhaseShuttingDown {
				createPod(ns, "web-0", appLabels("web"))
			}
			createApplication(ns, "shop", workload(shutdownv1alpha1.WorkloadKindDeployment, "web"))
			stop := start(operator.Config{})
			createShutdownRequest(shutdownv1alpha1.ActionShutdown)
			Eventually(replicasOf(web)).Should(BeZero())
			Eventually(phaseOf).Should(Equal(phase))

			stop()
			start(restart())

			Eventually(phaseOf).Should(Equal(shutdownv1alpha1.PhaseScalingUp))
			Eventually(replicasOf(web)).Should(Equal(int32(2)))
			Expect(actionOf()).To(Equal(shutdownv1alpha1.ActionCancel))
			// The action Shutdown from before the power cycle does not stop the cluster again.
			Consistently(replicasOf(web), time.Second).Should(Equal(int32(2)))
			markReady(web, 2)
			Eventually(phaseOf).Should(Equal(shutdownv1alpha1.PhaseRestored))
		},
		Entry("in ShuttingDown with a changed boot ID", shutdownv1alpha1.PhaseShuttingDown, func() operator.Config {
			return operator.Config{BootIDFile: bootIDFile("boot-2")}
		}),
		Entry("in Complete with a changed boot ID", shutdownv1alpha1.PhaseComplete, func() operator.Config {
			return operator.Config{BootIDFile: bootIDFile("boot-2")}
		}),
		Entry("in Complete with an unreadable boot ID file", shutdownv1alpha1.PhaseComplete, func() operator.Config {
			return operator.Config{BootIDFile: filepath.Join(GinkgoT().TempDir(), "missing")}
		}),
		Entry("in Complete with an empty boot ID file", shutdownv1alpha1.PhaseComplete, func() operator.Config {
			return operator.Config{BootIDFile: bootIDFile("")}
		}),
		Entry("in Complete with no entry for the node in the record", shutdownv1alpha1.PhaseComplete, func() operator.Config {
			return operator.Config{NodeName: "other-node"}
		}),
	)

	DescribeTable("continues a ScalingUp request",
		func(restart func() operator.Config) {
			web := createDeployment(ns, "web", 2)
			createApplication(ns, "shop", workload(shutdownv1alpha1.WorkloadKindDeployment, "web"))
			stop := start(operator.Config{})
			completeShutdown()
			setAction(shutdownv1alpha1.ActionCancel)
			Eventually(replicasOf(web)).Should(Equal(int32(2)))

			stop()
			start(restart())

			Consistently(phaseOf, time.Second).Should(Equal(shutdownv1alpha1.PhaseScalingUp))
			Expect(replicasOf(web)()).To(Equal(int32(2)))
			markReady(web, 2)
			Eventually(phaseOf).Should(Equal(shutdownv1alpha1.PhaseRestored))
		},
		Entry("with an equal boot ID", func() operator.Config { return operator.Config{} }),
		Entry("with a changed boot ID", func() operator.Config {
			return operator.Config{BootIDFile: bootIDFile("boot-2")}
		}),
	)

	It("starts a Shutdown that waits in ScalingUp after a restart in the same boot as the power cycle", func() {
		web := createDeployment(ns, "web", 2)
		createApplication(ns, "shop", workload(shutdownv1alpha1.WorkloadKindDeployment, "web"))
		stop := start(operator.Config{})
		completeShutdown()
		stop()
		stop = start(operator.Config{BootIDFile: bootIDFile("boot-2")})
		Eventually(replicasOf(web)).Should(Equal(int32(2)))

		// The Shutdown comes while no operator runs, and the next operator runs in the same boot.
		stop()
		setAction(shutdownv1alpha1.ActionShutdown)
		start(operator.Config{BootIDFile: bootIDFile("boot-2")})

		Eventually(replicasOf(web)).Should(BeZero())
		Eventually(phaseOf).Should(Equal(shutdownv1alpha1.PhaseComplete))
		Expect(actionOf()).To(Equal(shutdownv1alpha1.ActionShutdown))
	})

	It("continues the scale-up after a power cycle, also when a Shutdown waits in ScalingUp", func() {
		web := createDeployment(ns, "web", 2)
		createApplication(ns, "shop", workload(shutdownv1alpha1.WorkloadKindDeployment, "web"))
		stop := start(operator.Config{})
		completeShutdown()
		setAction(shutdownv1alpha1.ActionCancel)
		Eventually(replicasOf(web)).Should(Equal(int32(2)))

		// The Shutdown comes while no operator runs.
		stop()
		setAction(shutdownv1alpha1.ActionShutdown)
		start(operator.Config{BootIDFile: bootIDFile("boot-2")})

		Eventually(actionOf).Should(Equal(shutdownv1alpha1.ActionCancel))
		Consistently(replicasOf(web), time.Second).Should(Equal(int32(2)))
		Expect(phaseOf()).To(Equal(shutdownv1alpha1.PhaseScalingUp))
	})
})

var _ = Describe("A request with no phase at an operator start", func() {
	var ns string

	BeforeEach(func() {
		ns = newNamespace()
		DeferCleanup(cleanUp, ns)
	})

	It("starts a shutdown when the request is younger than the stale limit", func() {
		web := createDeployment(ns, "web", 2)
		createApplication(ns, "shop", workload(shutdownv1alpha1.WorkloadKindDeployment, "web"))
		createShutdownRequest(shutdownv1alpha1.ActionShutdown)

		start(operator.Config{})

		Eventually(replicasOf(web)).Should(BeZero())
		Eventually(phaseOf).Should(Equal(shutdownv1alpha1.PhaseComplete))
	})

	It("ends a request older than the stale limit as Restored with the reason Stale, and changes nothing else", func() {
		web := createDeployment(ns, "web", 2)
		createApplication(ns, "shop", workload(shutdownv1alpha1.WorkloadKindDeployment, "web"))
		createShutdownRequest(shutdownv1alpha1.ActionShutdown)

		start(operator.Config{
			StaleLimit: time.Minute,
			Clock:      clocktesting.NewFakePassiveClock(time.Now().Add(2 * time.Minute)),
		})

		Eventually(phaseOf).Should(Equal(shutdownv1alpha1.PhaseRestored))
		request := getRequest()
		Expect(request.Status).To(Equal(shutdownv1alpha1.ShutdownRequestStatus{
			Phase:  shutdownv1alpha1.PhaseRestored,
			Reason: shutdownv1alpha1.ReasonStale,
		}))
		Expect(request.Spec.Action).To(Equal(shutdownv1alpha1.ActionShutdown))
		Consistently(replicasOf(web), time.Second).Should(Equal(int32(2)))
	})

	It("starts a shutdown when the clock is behind the creation time", func() {
		web := createDeployment(ns, "web", 2)
		createApplication(ns, "shop", workload(shutdownv1alpha1.WorkloadKindDeployment, "web"))
		createShutdownRequest(shutdownv1alpha1.ActionShutdown)

		start(operator.Config{
			StaleLimit: time.Minute,
			Clock:      clocktesting.NewFakePassiveClock(time.Now().Add(-time.Hour)),
		})

		Eventually(replicasOf(web)).Should(BeZero())
		Eventually(phaseOf).Should(Equal(shutdownv1alpha1.PhaseComplete))
	})
})
