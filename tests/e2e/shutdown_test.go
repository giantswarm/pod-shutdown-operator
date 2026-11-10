//go:build e2e

package e2e

import (
	"net/http"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	shutdownv1alpha1 "gitlab.opendefense.cloud/odc/shutdown-op/pod-shutdown-operator/api/v1alpha1"
)

var _ = Describe("A shutdown in a kind cluster", func() {
	var ns string

	BeforeEach(func() {
		ns = createNamespace("e2e-")
	})

	AfterEach(func() {
		// The next test needs a request in Restored, and all operator pods.
		restore()
		deleteNamespace(ns)
		Eventually(operatorPods).Should(HaveLen(operatorReplicas))
	})

	It("stops the applications, keeps the operator, and restores everything at a cancel", func() {
		createStatefulSet(ns, "db", 1, fastStop)
		createDeployment(ns, "web", 2, fastStop)
		createCronJob(ns, "report")
		createApplication(ns, "db", shutdownv1alpha1.ManagedApplicationSpec{
			Workloads: []shutdownv1alpha1.WorkloadReference{workload(shutdownv1alpha1.WorkloadKindStatefulSet, "db")},
		})
		createApplication(ns, "web", shutdownv1alpha1.ManagedApplicationSpec{
			Workloads: []shutdownv1alpha1.WorkloadReference{
				workload(shutdownv1alpha1.WorkloadKindDeployment, "web"),
				workload(shutdownv1alpha1.WorkloadKindCronJob, "report"),
			},
			DependsOn: []string{"db"},
		})
		// A wrong entry lists the operator. The operator must not stop itself.
		createApplication(operatorNamespace, "operator", shutdownv1alpha1.ManagedApplicationSpec{
			Workloads: []shutdownv1alpha1.WorkloadReference{workload(shutdownv1alpha1.WorkloadKindDeployment, operatorName)},
		})
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, operatorApplication())).To(Succeed()) })
		Eventually(statefulSetReady(ns, "db")).Should(Equal(int32(1)))
		Eventually(readyReplicas(ns, "web")).Should(Equal(int32(2)))

		By("refusing a call with no token or a wrong token")
		Expect(post("/shutdown", "")).To(Equal(http.StatusUnauthorized))
		Expect(post("/shutdown", "wrong")).To(Equal(http.StatusUnauthorized))

		By("stopping each application")
		Expect(post("/shutdown", apiToken)).To(Equal(http.StatusAccepted))
		Eventually(complete).Should(BeTrue())
		p, err := progress()
		Expect(err).NotTo(HaveOccurred())
		// Total counts applications, not workloads. The operator application counts, also with no recorded workload.
		Expect(p.Total).To(Equal(int32(3)))
		Expect(p.Stopped).To(Equal(p.Total))
		Expect(deploymentReplicas(ns, "web")()).To(BeZero())
		Expect(statefulSetReplicas(ns, "db")()).To(BeZero())
		// Complete means that no container runs. A pod with no running container can still exist for a short time.
		Expect(runningPods(ns, "web")()).To(BeEmpty())
		Expect(runningPods(ns, "db")()).To(BeEmpty())
		Expect(cronJobSuspended(ns, "report")()).To(BeTrue())
		for _, name := range fluxControllers {
			Expect(deploymentReplicas(fluxNamespace, name)()).To(BeZero())
		}
		expectOperatorUp()

		By("restoring each application and the Flux controllers")
		Expect(post("/cancel", apiToken)).To(Equal(http.StatusAccepted))
		waitForRestored()
		Expect(deploymentReplicas(ns, "web")()).To(Equal(int32(2)))
		Expect(statefulSetReplicas(ns, "db")()).To(Equal(int32(1)))
		Expect(cronJobSuspended(ns, "report")()).To(BeFalse())
		for _, name := range fluxControllers {
			Expect(deploymentReplicas(fluxNamespace, name)()).To(Equal(int32(1)))
		}
		Eventually(readyReplicas(ns, "web")).Should(Equal(int32(2)))
		Eventually(statefulSetReady(ns, "db")).Should(Equal(int32(1)))
		expectOperatorUp()
	})

	It("goes on with the shutdown in the other pod when the leader is killed", func() {
		createDeployment(ns, "slow", 1, slowStop)
		createApplication(ns, "slow", shutdownv1alpha1.ManagedApplicationSpec{
			Workloads:   []shutdownv1alpha1.WorkloadReference{workload(shutdownv1alpha1.WorkloadKindDeployment, "slow")},
			GracePeriod: &metav1.Duration{Duration: 2 * time.Minute},
		})
		Eventually(readyReplicas(ns, "slow")).Should(Equal(int32(1)))
		leader, err := leaseHolder()
		Expect(err).NotTo(HaveOccurred())
		Expect(leader).NotTo(BeEmpty())

		Expect(post("/shutdown", apiToken)).To(Equal(http.StatusAccepted))
		Eventually(deploymentReplicas(ns, "slow")).Should(BeZero())
		Expect(phase()).To(Equal(shutdownv1alpha1.PhaseShuttingDown))
		startedAt := shutdownRequest().Status.ShutdownStartedAt

		By("killing the leader while the application stops")
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: operatorNamespace, Name: leader}}
		Expect(k8sClient.Delete(ctx, pod, client.GracePeriodSeconds(0))).To(Succeed())
		Eventually(leaseHolder).ShouldNot(BeElementOf("", leader))

		By("completing the shutdown with no scale-up, because the boot ID did not change")
		Eventually(phase).Should(Equal(shutdownv1alpha1.PhaseComplete))
		Consistently(phase, 10*time.Second).Should(Equal(shutdownv1alpha1.PhaseComplete))
		request := shutdownRequest()
		Expect(request.Status.ShutdownStartedAt).To(Equal(startedAt))
		Expect(request.Status.ScaleUpStartedAt).To(BeNil())
		Expect(complete()).To(BeTrue())
		Expect(deploymentReplicas(ns, "slow")()).To(BeZero())

		Expect(post("/cancel", apiToken)).To(Equal(http.StatusAccepted))
		waitForRestored()
		Eventually(readyReplicas(ns, "slow")).Should(Equal(int32(1)))
	})

	It("deletes a stuck StatefulSet pod by force, and waits before the StatefulSet scale-up", func() {
		createStatefulSet(ns, "stuck", 1, noStop)
		createApplication(ns, "stuck", shutdownv1alpha1.ManagedApplicationSpec{
			Workloads:   []shutdownv1alpha1.WorkloadReference{workload(shutdownv1alpha1.WorkloadKindStatefulSet, "stuck")},
			GracePeriod: &metav1.Duration{Duration: 10 * time.Second},
		})
		Eventually(statefulSetReady(ns, "stuck")).Should(Equal(int32(1)))

		By("deleting the pod by force at the end of its grace period")
		Expect(post("/shutdown", apiToken)).To(Equal(http.StatusAccepted))
		// The pod ignores SIGTERM and has a long termination grace period. Only a forced delete ends it this early.
		Eventually(pods(ns, "stuck"), 2*time.Minute).Should(BeEmpty())
		goneAt := time.Now()
		By("completing the shutdown")
		Eventually(phase).Should(Equal(shutdownv1alpha1.PhaseComplete))

		By("keeping the StatefulSet at 0 for the delay after the forced delete")
		Expect(post("/cancel", apiToken)).To(Equal(http.StatusAccepted))
		// The forced delete came before goneAt. The margin covers the poll interval of the test.
		window := time.Until(goneAt.Add(statefulSetScaleUpDelay - 5*time.Second))
		// A short window proves nothing.
		Expect(window).To(BeNumerically(">", 5*time.Second))
		Consistently(statefulSetReplicas(ns, "stuck"), window).Should(BeZero())
		Eventually(statefulSetReplicas(ns, "stuck")).Should(Equal(int32(1)))
		waitForRestored()
	})
})
