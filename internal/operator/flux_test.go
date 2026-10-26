//go:build envtest

package operator_test

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	shutdownv1alpha1 "github.com/giantswarm/pod-shutdown-operator/api/v1alpha1"
	"github.com/giantswarm/pod-shutdown-operator/internal/operator"
)

// scaleUpStartedAt waits for the start of the scale-up and returns status.scaleUpStartedAt.
// The global time limit comes from this stored time, which has a precision of one second.
func scaleUpStartedAt() time.Time {
	Eventually(func() *metav1.Time { return getRequest().Status.ScaleUpStartedAt }).ShouldNot(BeNil())
	return getRequest().Status.ScaleUpStartedAt.Time
}

var _ = Describe("The Flux controllers", func() {
	var (
		ns, fluxNS      string
		kustomize, helm *appsv1.Deployment
		stop            func()
	)

	// startWithFlux starts an operator that scales the two Flux controllers of the test.
	// The setting also lists a Flux controller that does not exist, which the operator skips.
	startWithFlux := func(c operator.Config) {
		c.FluxControllers = []types.NamespacedName{
			{Namespace: fluxNS, Name: "kustomize-controller"},
			{Namespace: fluxNS, Name: "helm-controller"},
			{Namespace: fluxNS, Name: "source-controller"},
		}
		stop, _ = startOperator(c)
	}

	BeforeEach(func() {
		ns = newNamespace()
		fluxNS = newNamespace()
		kustomize = createDeployment(fluxNS, "kustomize-controller", 1)
		helm = createDeployment(fluxNS, "helm-controller", 2)
	})

	AfterEach(func() {
		stop()
		cleanUp(ns, fluxNS)
	})

	It("records the Flux replicas and scales Flux to 0 before any application changes", func() {
		createPod(fluxNS, "kustomize-controller-0", appLabels("kustomize-controller"))
		web := createDeployment(ns, "web", 2)
		createApplication(ns, "shop", workload(shutdownv1alpha1.WorkloadKindDeployment, "web"))
		startWithFlux(operator.Config{FluxStopTimeout: time.Minute})

		createShutdownRequest(shutdownv1alpha1.ActionShutdown)

		Eventually(replicasOf(kustomize)).Should(BeZero())
		Eventually(replicasOf(helm)).Should(BeZero())
		Expect(getRequest().Status.RecordedState.FluxControllers).To(ConsistOf(
			shutdownv1alpha1.RecordedFluxController{Namespace: fluxNS, Name: "kustomize-controller", Replicas: 1},
			shutdownv1alpha1.RecordedFluxController{Namespace: fluxNS, Name: "helm-controller", Replicas: 2},
		))
		// A Flux pod has not ended, so no application changes.
		Consistently(replicasOf(web), time.Second).Should(Equal(int32(2)))

		deletePod(fluxNS, "kustomize-controller-0")

		Eventually(replicasOf(web)).Should(BeZero())
		Eventually(phaseOf).Should(Equal(shutdownv1alpha1.PhaseComplete))
	})

	It("goes on with the shutdown when the Flux time limit passes, and does not delete the Flux pods", func() {
		createPod(fluxNS, "kustomize-controller-0", appLabels("kustomize-controller"))
		web := createDeployment(ns, "web", 2)
		createApplication(ns, "shop", workload(shutdownv1alpha1.WorkloadKindDeployment, "web"))
		startWithFlux(operator.Config{FluxStopTimeout: 2 * time.Second})

		createShutdownRequest(shutdownv1alpha1.ActionShutdown)
		start := startedAt()

		Eventually(replicasOf(kustomize)).Should(BeZero())
		Consistently(replicasOf(web), until(start.Add(1500*time.Millisecond))).Should(Equal(int32(2)))
		Eventually(replicasOf(web)).Should(BeZero())
		Eventually(phaseOf).Should(Equal(shutdownv1alpha1.PhaseComplete))
		Expect(podExists(fluxNS, "kustomize-controller-0")()).To(BeTrue())
	})

	It("gives the groups the time from the end of the Flux time limit to the end of the budget", func() {
		createPod(fluxNS, "kustomize-controller-0", appLabels("kustomize-controller"))
		web := createDeployment(ns, "web", 1)
		db := createStatefulSet(ns, "db", 1)
		createOwnedPod(ns, "web-0", web)
		createOwnedPod(ns, "db-0", db)
		createApplication(ns, "frontend", workload(shutdownv1alpha1.WorkloadKindDeployment, "web"))
		createApplication(ns, "database", workload(shutdownv1alpha1.WorkloadKindStatefulSet, "db"))
		setDependsOn(ns, "frontend", "database")
		setGracePeriod(ns, "frontend", 8*time.Second)
		setGracePeriod(ns, "database", 8*time.Second)
		startWithFlux(operator.Config{FluxStopTimeout: 2 * time.Second})

		// The time available is 8s minus the Flux limit of 2s, and the chain is 16s, so each gracePeriod shrinks to 3s.
		createShutdownRequestWithBudget(9*time.Second, time.Second)
		start := startedAt()

		// With the groups at the start of the shutdown, each gracePeriod shrinks to 4s, and the deadline of frontend is 4s after the start.
		Consistently(podExists(ns, "web-0"), until(start.Add(4500*time.Millisecond))).Should(BeTrue())
		Eventually(podExists(ns, "web-0")).Should(BeFalse())
		Expect(time.Since(start)).To(BeNumerically("<", 6*time.Second))
		Eventually(phaseOf).Should(Equal(shutdownv1alpha1.PhaseComplete))
	})

	It("ends the Flux step at the end of the budget when the Flux time limit is longer", func() {
		createPod(fluxNS, "kustomize-controller-0", appLabels("kustomize-controller"))
		web := createDeployment(ns, "web", 2)
		createApplication(ns, "shop", workload(shutdownv1alpha1.WorkloadKindDeployment, "web"))
		startWithFlux(operator.Config{FluxStopTimeout: time.Minute})

		// The end of the budget is 2s after the start.
		createShutdownRequestWithBudget(3*time.Second, time.Second)
		start := startedAt()

		Eventually(replicasOf(web)).Should(BeZero())
		Expect(time.Since(start)).To(BeNumerically("<", 5*time.Second))
		Eventually(phaseOf).Should(Equal(shutdownv1alpha1.PhaseComplete))
	})

	It("scales Flux back to the recorded replicas after all groups are ready, and then sets Restored", func() {
		db := createDeployment(ns, "db", 1)
		web := createDeployment(ns, "web", 2)
		createApplication(ns, "db", workload(shutdownv1alpha1.WorkloadKindDeployment, "db"))
		createApplication(ns, "web", workload(shutdownv1alpha1.WorkloadKindDeployment, "web"))
		setDependsOn(ns, "web", "db")
		startWithFlux(operator.Config{FluxStopTimeout: time.Minute})
		completeShutdown()
		Expect(replicasOf(kustomize)()).To(BeZero())
		fluxBack := watchFirst(&appsv1.DeploymentList{}, fluxNS, func(o runtime.Object) bool {
			d := o.(*appsv1.Deployment)
			return d.Name == helm.Name && ptr.Deref(d.Spec.Replicas, 0) == 2
		})
		restored := watchFirst(&shutdownv1alpha1.ShutdownRequestList{}, "", func(o runtime.Object) bool {
			return o.(*shutdownv1alpha1.ShutdownRequest).Status.Phase == shutdownv1alpha1.PhaseRestored
		})

		setAction(shutdownv1alpha1.ActionCancel)

		Eventually(replicasOf(db)).Should(Equal(int32(1)))
		markReady(db, 1)
		Eventually(replicasOf(web)).Should(Equal(int32(2)))
		Consistently(replicasOf(kustomize), time.Second).Should(BeZero())
		Expect(replicasOf(helm)()).To(BeZero())

		markReady(web, 2)

		Eventually(replicasOf(kustomize)).Should(Equal(int32(1)))
		Eventually(replicasOf(helm)).Should(Equal(int32(2)))
		Eventually(phaseOf).Should(Equal(shutdownv1alpha1.PhaseRestored))
		var fluxBackRV, restoredRV uint64
		Eventually(fluxBack).Should(Receive(&fluxBackRV))
		Eventually(restored).Should(Receive(&restoredRV))
		Expect(fluxBackRV).To(BeNumerically("<", restoredRV))
	})

	It("does not keep Flux at 0 for an application that is not ready at its readyTimeout", func() {
		createDeployment(ns, "slow", 1)
		createApplication(ns, "slow", workload(shutdownv1alpha1.WorkloadKindDeployment, "slow"))
		setReadyTimeout(ns, "slow", time.Second)
		startWithFlux(operator.Config{FluxStopTimeout: time.Minute})
		completeShutdown()

		setAction(shutdownv1alpha1.ActionCancel)

		Eventually(phaseOf).Should(Equal(shutdownv1alpha1.PhaseRestored))
		Expect(getRequest().Status.Degraded).To(ConsistOf(ns + "/slow"))
		Expect(replicasOf(kustomize)()).To(Equal(int32(1)))
		Expect(replicasOf(helm)()).To(Equal(int32(2)))
	})

	It("starts every remaining application and scales Flux back at the global scale-up time limit", func() {
		db := createDeployment(ns, "db", 1)
		web := createDeployment(ns, "web", 2)
		createApplication(ns, "db", workload(shutdownv1alpha1.WorkloadKindDeployment, "db"))
		createApplication(ns, "web", workload(shutdownv1alpha1.WorkloadKindDeployment, "web"))
		setDependsOn(ns, "web", "db")
		startWithFlux(operator.Config{FluxStopTimeout: time.Minute, ScaleUpTimeout: 3 * time.Second})
		completeShutdown()

		// db does not become ready, and its readyTimeout is the default of 5 minutes.
		setAction(shutdownv1alpha1.ActionCancel)
		start := scaleUpStartedAt()

		Eventually(replicasOf(db)).Should(Equal(int32(1)))
		webAndFlux := func() []int32 { return []int32{replicasOf(web)(), replicasOf(kustomize)()} }
		Consistently(webAndFlux, until(start.Add(2500*time.Millisecond))).Should(Equal([]int32{0, 0}))

		Eventually(replicasOf(web)).Should(Equal(int32(2)))
		Eventually(replicasOf(kustomize)).Should(Equal(int32(1)))
		Eventually(phaseOf).Should(Equal(shutdownv1alpha1.PhaseRestored))
		Expect(getRequest().Status.Degraded).To(ConsistOf(ns+"/db", ns+"/web"))
	})

	It("scales Flux back on a cancel during the Flux step, and does not change the applications", func() {
		createPod(fluxNS, "kustomize-controller-0", appLabels("kustomize-controller"))
		web := createDeployment(ns, "web", 2)
		createApplication(ns, "shop", workload(shutdownv1alpha1.WorkloadKindDeployment, "web"))
		webScaled := watchFirst(&appsv1.DeploymentList{}, ns, func(o runtime.Object) bool {
			d := o.(*appsv1.Deployment)
			return d.Name == web.Name && ptr.Deref(d.Spec.Replicas, 1) != 2
		})
		startWithFlux(operator.Config{FluxStopTimeout: time.Minute})
		createShutdownRequest(shutdownv1alpha1.ActionShutdown)
		Eventually(replicasOf(kustomize)).Should(BeZero())

		setAction(shutdownv1alpha1.ActionCancel)

		Eventually(phaseOf).Should(Equal(shutdownv1alpha1.PhaseScalingUp))
		markReady(web, 2)
		Eventually(phaseOf).Should(Equal(shutdownv1alpha1.PhaseRestored))
		Expect(webScaled).NotTo(Receive())
		Expect(replicasOf(kustomize)()).To(Equal(int32(1)))
		Expect(replicasOf(helm)()).To(Equal(int32(2)))
	})
})
