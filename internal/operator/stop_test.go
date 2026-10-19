//go:build envtest

package operator_test

import (
	"context"
	"strconv"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	shutdownv1alpha1 "github.com/giantswarm/pod-shutdown-operator/api/v1alpha1"
	"github.com/giantswarm/pod-shutdown-operator/internal/operator"
)

// watchFirst returns a channel that gets the resource version of the first watched object
// for which match returns true. The watch starts before the function returns.
func watchFirst(list client.ObjectList, namespace string, match func(runtime.Object) bool) <-chan uint64 {
	watchCtx, watchCancel := context.WithCancel(ctx)
	DeferCleanup(watchCancel)
	w, err := watchClient.Watch(watchCtx, list, client.InNamespace(namespace))
	Expect(err).NotTo(HaveOccurred())

	found := make(chan uint64, 1)
	go func() {
		defer GinkgoRecover()
		defer w.Stop()
		for event := range w.ResultChan() {
			// An ERROR event, for example at the end of the watch, holds a Status and not the watched object.
			if event.Type != watch.Added && event.Type != watch.Modified {
				continue
			}
			if match(event.Object) {
				rv, err := strconv.ParseUint(event.Object.(client.Object).GetResourceVersion(), 10, 64)
				Expect(err).NotTo(HaveOccurred())
				found <- rv
				return
			}
		}
	}()
	return found
}

var _ = Describe("A basic shutdown", func() {
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

	It("scales the listed workloads to 0 and leaves unlisted workloads alone", func() {
		web := createDeployment(ns, "web", 3)
		db := createStatefulSet(ns, "db", 2)
		unlisted := createDeployment(ns, "unlisted", 4)
		createApplication(ns, "shop",
			workload(shutdownv1alpha1.WorkloadKindDeployment, "web"),
			workload(shutdownv1alpha1.WorkloadKindStatefulSet, "db"),
		)

		createShutdownRequest(shutdownv1alpha1.ActionShutdown)

		Eventually(replicasOf(web)).Should(BeZero())
		Eventually(replicasOf(db)).Should(BeZero())
		Eventually(phaseOf).Should(Equal(shutdownv1alpha1.PhaseComplete))
		Consistently(replicasOf(unlisted), time.Second).Should(Equal(int32(4)))
	})

	It("writes the record before it changes any workload", func() {
		web := createDeployment(ns, "web", 3)
		createApplication(ns, "shop", workload(shutdownv1alpha1.WorkloadKindDeployment, "web"))
		recordWritten := watchFirst(&shutdownv1alpha1.ShutdownRequestList{}, "", func(o runtime.Object) bool {
			return o.(*shutdownv1alpha1.ShutdownRequest).Status.RecordedState != nil
		})
		webScaled := watchFirst(&appsv1.DeploymentList{}, ns, func(o runtime.Object) bool {
			d := o.(*appsv1.Deployment)
			return d.Name == web.Name && ptr.Deref(d.Spec.Replicas, 1) == 0
		})

		createShutdownRequest(shutdownv1alpha1.ActionShutdown)

		var recordRV, scaleRV uint64
		Eventually(recordWritten).Should(Receive(&recordRV))
		Eventually(webScaled).Should(Receive(&scaleRV))
		// etcd orders resource versions in envtest. The API contract does not promise
		// this order, so this check is valid for tests only.
		Expect(recordRV).To(BeNumerically("<", scaleRV))
	})

	It("counts the applications whose pods have ended and never changes the record", func() {
		createDeployment(ns, "web", 2)
		createPod(ns, "web-0", appLabels("web"))
		createPod(ns, "web-1", appLabels("web"))
		createStatefulSet(ns, "db", 1)
		createPod(ns, "db-0", appLabels("db"))
		createApplication(ns, "frontend", workload(shutdownv1alpha1.WorkloadKindDeployment, "web"))
		createApplication(ns, "database", workload(shutdownv1alpha1.WorkloadKindStatefulSet, "db"))

		createShutdownRequest(shutdownv1alpha1.ActionShutdown)

		Eventually(phaseOf).Should(Equal(shutdownv1alpha1.PhaseShuttingDown))
		first := getRequest().Status
		Expect(first.Total).To(Equal(int32(2)))
		Consistently(stoppedOf, time.Second).Should(BeZero())

		deletePod(ns, "db-0")
		Eventually(stoppedOf).Should(Equal(int32(1)))
		Expect(phaseOf()).To(Equal(shutdownv1alpha1.PhaseShuttingDown))

		deletePod(ns, "web-0")
		Consistently(stoppedOf, time.Second).Should(Equal(int32(1)))

		deletePod(ns, "web-1")
		Eventually(phaseOf).Should(Equal(shutdownv1alpha1.PhaseComplete))

		final := getRequest().Status
		Expect(final.Stopped).To(Equal(int32(2)))
		Expect(final.RecordedState).To(Equal(first.RecordedState))
		Expect(final.ShutdownStartedAt.Equal(first.ShutdownStartedAt)).To(BeTrue())
	})

	It("completes at once when no ManagedApplication exists", func() {
		createShutdownRequest(shutdownv1alpha1.ActionShutdown)

		Eventually(phaseOf).Should(Equal(shutdownv1alpha1.PhaseComplete))
		Expect(getRequest().Status.Total).To(BeZero())
	})

	It("counts an application with no recorded workload as stopped", func() {
		createApplication(ns, "legacy",
			workload(shutdownv1alpha1.WorkloadKindDeployment, "missing"),
			workload(shutdownv1alpha1.WorkloadKindVirtualMachine, "vm"),
		)

		createShutdownRequest(shutdownv1alpha1.ActionShutdown)

		Eventually(phaseOf).Should(Equal(shutdownv1alpha1.PhaseComplete))
		status := getRequest().Status
		Expect(status.Total).To(Equal(int32(1)))
		Expect(status.Stopped).To(Equal(int32(1)))
	})

	It("records a workload at 0 replicas as 0 and waits for its pods", func() {
		createDeployment(ns, "idle", 0)
		createPod(ns, "idle-0", appLabels("idle"))
		createApplication(ns, "batch", workload(shutdownv1alpha1.WorkloadKindDeployment, "idle"))

		createShutdownRequest(shutdownv1alpha1.ActionShutdown)

		Eventually(phaseOf).Should(Equal(shutdownv1alpha1.PhaseShuttingDown))
		Expect(getRequest().Status.RecordedState.Applications[0].Workloads[0].Replicas).To(Equal(ptr.To[int32](0)))
		Consistently(phaseOf, time.Second).Should(Equal(shutdownv1alpha1.PhaseShuttingDown))

		deletePod(ns, "idle-0")
		Eventually(phaseOf).Should(Equal(shutdownv1alpha1.PhaseComplete))
	})

	It("counts a Pod in a terminal phase as ended", func() {
		// The scale to 0 does not delete a Failed Pod (for example an evicted Pod)
		// or a Succeeded Pod. They have no running container.
		createDeployment(ns, "web", 2)
		createPod(ns, "web-0", appLabels("web"))
		setPodPhase(ns, "web-0", corev1.PodFailed)
		createPod(ns, "web-1", appLabels("web"))
		setPodPhase(ns, "web-1", corev1.PodSucceeded)
		createApplication(ns, "shop", workload(shutdownv1alpha1.WorkloadKindDeployment, "web"))

		createShutdownRequest(shutdownv1alpha1.ActionShutdown)

		Eventually(phaseOf).Should(Equal(shutdownv1alpha1.PhaseComplete))
	})

	It("ignores pods with matching labels in another namespace", func() {
		createDeployment(ns, "web", 1)
		createPod(otherNS, "web-0", appLabels("web"))
		createApplication(ns, "shop", workload(shutdownv1alpha1.WorkloadKindDeployment, "web"))

		createShutdownRequest(shutdownv1alpha1.ActionShutdown)

		Eventually(phaseOf).Should(Equal(shutdownv1alpha1.PhaseComplete))
	})
})
