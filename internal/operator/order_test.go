//go:build envtest

package operator_test

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	shutdownv1alpha1 "github.com/giantswarm/pod-shutdown-operator/api/v1alpha1"
	"github.com/giantswarm/pod-shutdown-operator/internal/operator"
)

var _ = Describe("The dependency order", func() {
	var (
		ns, otherNS string
		stop        func()
	)

	BeforeEach(func() {
		ns, otherNS = newNamespace(), newNamespace()
		stop, _ = startOperator(operator.Config{})
	})

	AfterEach(func() {
		stop()
		cleanUp(ns, otherNS)
	})

	Context("at a shutdown", func() {
		It("stops dependents before the applications they depend on, also across namespaces", func() {
			web := createDeployment(ns, "web", 1)
			api := createDeployment(ns, "api", 1)
			db := createStatefulSet(otherNS, "db", 1)
			createPod(ns, "web-0", appLabels("web"))
			createPod(ns, "api-0", appLabels("api"))
			createPod(otherNS, "db-0", appLabels("db"))
			createApplication(ns, "frontend", workload(shutdownv1alpha1.WorkloadKindDeployment, "web"))
			createApplication(ns, "backend", workload(shutdownv1alpha1.WorkloadKindDeployment, "api"))
			createApplication(otherNS, "database", workload(shutdownv1alpha1.WorkloadKindStatefulSet, "db"))
			setDependsOn(ns, "frontend", "backend")
			setDependsOn(ns, "backend", otherNS+"/database")

			createShutdownRequest(shutdownv1alpha1.ActionShutdown)

			Eventually(replicasOf(web)).Should(BeZero())
			Consistently(replicasOf(api), time.Second).Should(Equal(int32(1)))
			Expect(replicasOf(db)()).To(Equal(int32(1)))

			deletePod(ns, "web-0")
			Eventually(replicasOf(api)).Should(BeZero())
			Consistently(replicasOf(db), time.Second).Should(Equal(int32(1)))

			deletePod(ns, "api-0")
			Eventually(replicasOf(db)).Should(BeZero())
			Expect(phaseOf()).To(Equal(shutdownv1alpha1.PhaseShuttingDown))

			deletePod(otherNS, "db-0")
			Eventually(phaseOf).Should(Equal(shutdownv1alpha1.PhaseComplete))
		})

		It("stops an application as soon as all its dependents have stopped, with no wait at a tier boundary", func() {
			createDeployment(ns, "web", 1)
			createDeployment(ns, "worker", 1)
			cache := createDeployment(ns, "cache", 1)
			db := createStatefulSet(ns, "db", 1)
			createPod(ns, "web-0", appLabels("web"))
			createPod(ns, "worker-0", appLabels("worker"))
			createPod(ns, "cache-0", appLabels("cache"))
			createApplication(ns, "frontend", workload(shutdownv1alpha1.WorkloadKindDeployment, "web"))
			createApplication(ns, "worker", workload(shutdownv1alpha1.WorkloadKindDeployment, "worker"))
			createApplication(ns, "cache", workload(shutdownv1alpha1.WorkloadKindDeployment, "cache"))
			createApplication(ns, "database", workload(shutdownv1alpha1.WorkloadKindStatefulSet, "db"))
			// frontend and worker are in tier 1, cache and database are in tier 2.
			setDependsOn(ns, "frontend", "cache", "database")
			setDependsOn(ns, "worker", "database")

			createShutdownRequest(shutdownv1alpha1.ActionShutdown)

			// The worker pod stays. cache does not wait for it, because the worker does not depend on cache.
			deletePod(ns, "web-0")
			Eventually(replicasOf(cache)).Should(BeZero())
			Consistently(replicasOf(db), time.Second).Should(Equal(int32(1)))

			deletePod(ns, "worker-0")
			Eventually(replicasOf(db)).Should(BeZero())
		})

		It("stops two dependency groups in parallel, so that a slow group does not delay the other", func() {
			createDeployment(ns, "slow-web", 1)
			slowDB := createStatefulSet(ns, "slow-db", 1)
			createDeployment(otherNS, "web", 1)
			createDeployment(otherNS, "api", 1)
			db := createStatefulSet(otherNS, "db", 1)
			createPod(ns, "slow-web-0", appLabels("slow-web"))
			createPod(otherNS, "web-0", appLabels("web"))
			createPod(otherNS, "api-0", appLabels("api"))
			createApplication(ns, "slow-frontend", workload(shutdownv1alpha1.WorkloadKindDeployment, "slow-web"))
			createApplication(ns, "slow-database", workload(shutdownv1alpha1.WorkloadKindStatefulSet, "slow-db"))
			createApplication(otherNS, "frontend", workload(shutdownv1alpha1.WorkloadKindDeployment, "web"))
			createApplication(otherNS, "backend", workload(shutdownv1alpha1.WorkloadKindDeployment, "api"))
			createApplication(otherNS, "database", workload(shutdownv1alpha1.WorkloadKindStatefulSet, "db"))
			setDependsOn(ns, "slow-frontend", "slow-database")
			setDependsOn(otherNS, "frontend", "backend")
			setDependsOn(otherNS, "backend", "database")

			createShutdownRequest(shutdownv1alpha1.ActionShutdown)

			// The slow-web pod stays. The other group goes past tier 2 while slow-database waits in tier 2.
			deletePod(otherNS, "web-0")
			deletePod(otherNS, "api-0")
			Eventually(replicasOf(db)).Should(BeZero())
			Eventually(stoppedOf).Should(Equal(int32(3)))
			Consistently(replicasOf(slowDB), time.Second).Should(Equal(int32(1)))

			deletePod(ns, "slow-web-0")
			Eventually(replicasOf(slowDB)).Should(BeZero())
			Eventually(phaseOf).Should(Equal(shutdownv1alpha1.PhaseComplete))
		})
	})

	Context("at a scale-up", func() {
		It("starts dependencies before their dependents, also across namespaces", func() {
			web := createDeployment(ns, "web", 2)
			db := createStatefulSet(otherNS, "db", 1)
			createApplication(ns, "frontend", workload(shutdownv1alpha1.WorkloadKindDeployment, "web"))
			createApplication(otherNS, "database", workload(shutdownv1alpha1.WorkloadKindStatefulSet, "db"))
			setDependsOn(ns, "frontend", otherNS+"/database")
			completeShutdown()

			setAction(shutdownv1alpha1.ActionCancel)

			Eventually(replicasOf(db)).Should(Equal(int32(1)))
			Consistently(replicasOf(web), time.Second).Should(BeZero())

			markReady(db, 1)
			Eventually(replicasOf(web)).Should(Equal(int32(2)))
			Expect(phaseOf()).To(Equal(shutdownv1alpha1.PhaseScalingUp))

			markReady(web, 2)
			Eventually(phaseOf).Should(Equal(shutdownv1alpha1.PhaseRestored))
			Expect(getRequest().Status.Degraded).To(BeEmpty())
		})

		It("starts a dependent when its dependency is Degraded", func() {
			web := createDeployment(ns, "web", 2)
			db := createStatefulSet(ns, "db", 1)
			createApplication(ns, "frontend", workload(shutdownv1alpha1.WorkloadKindDeployment, "web"))
			createApplication(ns, "database", workload(shutdownv1alpha1.WorkloadKindStatefulSet, "db"))
			setDependsOn(ns, "frontend", "database")
			setReadyTimeout(ns, "database", 2*time.Second)
			completeShutdown()

			setAction(shutdownv1alpha1.ActionCancel)

			Eventually(replicasOf(db)).Should(Equal(int32(1)))
			Consistently(replicasOf(web), time.Second).Should(BeZero())
			Eventually(func() []string { return getRequest().Status.Degraded }).Should(ConsistOf(ns + "/database"))
			Eventually(replicasOf(web)).Should(Equal(int32(2)))

			markReady(web, 2)
			Eventually(phaseOf).Should(Equal(shutdownv1alpha1.PhaseRestored))
			Expect(getRequest().Status.Degraded).To(ConsistOf(ns + "/database"))
		})

		It("starts the readyTimeout of a dependent when the dependent starts", func() {
			web := createDeployment(ns, "web", 2)
			db := createStatefulSet(ns, "db", 1)
			createApplication(ns, "frontend", workload(shutdownv1alpha1.WorkloadKindDeployment, "web"))
			createApplication(ns, "database", workload(shutdownv1alpha1.WorkloadKindStatefulSet, "db"))
			setDependsOn(ns, "frontend", "database")
			setReadyTimeout(ns, "frontend", time.Second)
			completeShutdown()

			setAction(shutdownv1alpha1.ActionCancel)

			// The database takes longer than the readyTimeout of frontend.
			Eventually(replicasOf(db)).Should(Equal(int32(1)))
			Consistently(replicasOf(web), 1500*time.Millisecond).Should(BeZero())
			markReady(db, 1)
			Eventually(replicasOf(web)).Should(Equal(int32(2)))
			markReady(web, 2)

			Eventually(phaseOf).Should(Equal(shutdownv1alpha1.PhaseRestored))
			Expect(getRequest().Status.Degraded).To(BeEmpty())
		})

		It("starts in order after a cancel in ShuttingDown, when a dependency has not stopped", func() {
			web := createDeployment(ns, "web", 2)
			db := createStatefulSet(ns, "db", 1)
			createPod(ns, "web-0", appLabels("web"))
			createApplication(ns, "frontend", workload(shutdownv1alpha1.WorkloadKindDeployment, "web"))
			createApplication(ns, "database", workload(shutdownv1alpha1.WorkloadKindStatefulSet, "db"))
			setDependsOn(ns, "frontend", "database")
			createShutdownRequest(shutdownv1alpha1.ActionShutdown)
			Eventually(replicasOf(web)).Should(BeZero())
			Expect(replicasOf(db)()).To(Equal(int32(1)))

			setAction(shutdownv1alpha1.ActionCancel)

			// The database still runs, but its status is not ready, so frontend waits.
			Eventually(phaseOf).Should(Equal(shutdownv1alpha1.PhaseScalingUp))
			Consistently(replicasOf(web), time.Second).Should(BeZero())
			Expect(replicasOf(db)()).To(Equal(int32(1)))

			markReady(db, 1)
			Eventually(replicasOf(web)).Should(Equal(int32(2)))
		})

		It("starts two dependency groups in parallel, so that a slow group does not delay the other", func() {
			slowWeb := createDeployment(ns, "slow-web", 1)
			createStatefulSet(ns, "slow-db", 1)
			web := createDeployment(otherNS, "web", 1)
			db := createStatefulSet(otherNS, "db", 1)
			createApplication(ns, "slow-frontend", workload(shutdownv1alpha1.WorkloadKindDeployment, "slow-web"))
			createApplication(ns, "slow-database", workload(shutdownv1alpha1.WorkloadKindStatefulSet, "slow-db"))
			createApplication(otherNS, "frontend", workload(shutdownv1alpha1.WorkloadKindDeployment, "web"))
			createApplication(otherNS, "database", workload(shutdownv1alpha1.WorkloadKindStatefulSet, "db"))
			setDependsOn(ns, "slow-frontend", "slow-database")
			setDependsOn(otherNS, "frontend", "database")
			completeShutdown()

			setAction(shutdownv1alpha1.ActionCancel)

			// slow-database never becomes ready and keeps slow-frontend at 0.
			Eventually(replicasOf(db)).Should(Equal(int32(1)))
			markReady(db, 1)
			Eventually(replicasOf(web)).Should(Equal(int32(1)))
			Consistently(replicasOf(slowWeb), time.Second).Should(BeZero())
			Expect(phaseOf()).To(Equal(shutdownv1alpha1.PhaseScalingUp))
		})
	})
})
