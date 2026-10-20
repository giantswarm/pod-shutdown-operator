//go:build envtest

package operator_test

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	shutdownv1alpha1 "github.com/giantswarm/pod-shutdown-operator/api/v1alpha1"
	"github.com/giantswarm/pod-shutdown-operator/internal/operator"
)

// startedAt waits for the start of the shutdown and returns status.shutdownStartedAt.
// The deadlines come from this stored time, which has a precision of one second.
func startedAt() time.Time {
	Eventually(func() *metav1.Time { return getRequest().Status.ShutdownStartedAt }).ShouldNot(BeNil())
	return getRequest().Status.ShutdownStartedAt.Time
}

var _ = Describe("The time budget", func() {
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

	It("deletes the remaining pods with a grace period of 0 at the start time plus the gracePeriod", func() {
		createDeployment(ns, "web", 1)
		createScheduledPod(ns, "web-0", appLabels("web"))
		createApplication(ns, "shop", workload(shutdownv1alpha1.WorkloadKindDeployment, "web"))
		setGracePeriod(ns, "shop", 2*time.Second)

		createShutdownRequest(shutdownv1alpha1.ActionShutdown)

		Consistently(podExists(ns, "web-0"), 1500*time.Millisecond).Should(BeTrue())
		// A delete with a grace period above 0 leaves a scheduled pod in Terminating.
		Eventually(podExists(ns, "web-0")).Should(BeFalse())
		Eventually(phaseOf).Should(Equal(shutdownv1alpha1.PhaseComplete))
		Expect(stoppedOf()).To(Equal(int32(1)))
	})

	It("shrinks the gracePeriod values of a group that exceed the time available, so that the longest chain fits", func() {
		createDeployment(ns, "web", 1)
		db := createStatefulSet(ns, "db", 1)
		createScheduledPod(ns, "web-0", appLabels("web"))
		createScheduledPod(ns, "db-0", appLabels("db"))
		createApplication(ns, "frontend", workload(shutdownv1alpha1.WorkloadKindDeployment, "web"))
		createApplication(ns, "database", workload(shutdownv1alpha1.WorkloadKindStatefulSet, "db"))
		setDependsOn(ns, "frontend", "database")
		setGracePeriod(ns, "frontend", 8*time.Second)
		setGracePeriod(ns, "database", 8*time.Second)

		// The time available is 8s and the chain is 16s, so each gracePeriod shrinks to 4s.
		createShutdownRequestWithBudget(9*time.Second, time.Second)
		start := startedAt()

		// With no shrink, the deadline of frontend is the start of the shutdown.
		Consistently(podExists(ns, "web-0"), time.Until(start.Add(3500*time.Millisecond))).Should(BeTrue())
		Eventually(podExists(ns, "web-0")).Should(BeFalse())
		Expect(time.Since(start)).To(BeNumerically("<", 5*time.Second))

		Eventually(replicasOf(db)).Should(BeZero())
		Consistently(podExists(ns, "db-0"), time.Until(start.Add(7500*time.Millisecond))).Should(BeTrue())
		Eventually(podExists(ns, "db-0")).Should(BeFalse())
		Eventually(phaseOf).Should(Equal(shutdownv1alpha1.PhaseComplete))
	})

	It("uses only the absolute deadline after an operator restart, and keeps the safety margin free", func() {
		web := createDeployment(ns, "web", 1)
		createScheduledPod(ns, "web-0", appLabels("web"))
		createApplication(ns, "shop", workload(shutdownv1alpha1.WorkloadKindDeployment, "web"))
		setGracePeriod(ns, "shop", 2*time.Second)

		// The deadline is 6s after the start: the budget of 8s minus the safety margin of 2s.
		createShutdownRequestWithBudget(8*time.Second, 2*time.Second)
		start := startedAt()
		Eventually(replicasOf(web)).Should(BeZero())

		stop()
		stop, _ = startOperator(operator.Config{})

		// The start time of the application is lost, so the forced stop does not come 2s after the restart.
		Consistently(podExists(ns, "web-0"), time.Until(start.Add(5500*time.Millisecond))).Should(BeTrue())
		Eventually(podExists(ns, "web-0")).Should(BeFalse())
		Expect(time.Since(start)).To(BeNumerically("<", 7*time.Second))
		Eventually(phaseOf).Should(Equal(shutdownv1alpha1.PhaseComplete))
	})

	It("forces the stop at once when no time is available", func() {
		createDeployment(ns, "web", 1)
		createScheduledPod(ns, "web-0", appLabels("web"))
		createApplication(ns, "shop", workload(shutdownv1alpha1.WorkloadKindDeployment, "web"))

		createShutdownRequestWithBudget(time.Second, time.Second)

		Eventually(phaseOf).Should(Equal(shutdownv1alpha1.PhaseComplete))
		Expect(podExists(ns, "web-0")()).To(BeFalse())
	})
})

var _ = Describe("A StatefulSet scale-up after a forced delete", func() {
	var (
		ns   string
		stop func()
	)

	BeforeEach(func() {
		ns = newNamespace()
		stop, _ = startOperator(operator.Config{StatefulSetScaleUpDelay: 3 * time.Second})
	})

	AfterEach(func() {
		stop()
		cleanUp(ns)
	})

	It("waits for the delay, and a Deployment or a StatefulSet with no forced delete does not wait", func() {
		db := createStatefulSet(ns, "db", 1)
		cache := createStatefulSet(ns, "cache", 1)
		web := createDeployment(ns, "web", 2)
		createScheduledPod(ns, "db-0", appLabels("db"))
		createScheduledPod(ns, "web-0", appLabels("web"))
		createApplication(ns, "database", workload(shutdownv1alpha1.WorkloadKindStatefulSet, "db"))
		createApplication(ns, "cache", workload(shutdownv1alpha1.WorkloadKindStatefulSet, "cache"))
		createApplication(ns, "frontend", workload(shutdownv1alpha1.WorkloadKindDeployment, "web"))
		setGracePeriod(ns, "database", time.Second)
		setGracePeriod(ns, "frontend", time.Second)
		completeShutdown()
		Expect(podExists(ns, "db-0")()).To(BeFalse())

		setAction(shutdownv1alpha1.ActionCancel)

		Eventually(replicasOf(web), time.Second).Should(Equal(int32(2)))
		Eventually(replicasOf(cache), time.Second).Should(Equal(int32(1)))
		Consistently(replicasOf(db), 1500*time.Millisecond).Should(BeZero())
		Eventually(replicasOf(db), 5*time.Second).Should(Equal(int32(1)))
	})
})
