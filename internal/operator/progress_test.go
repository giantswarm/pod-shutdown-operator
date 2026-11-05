//go:build envtest

package operator_test

import (
	"net/http"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	shutdownv1alpha1 "github.com/giantswarm/pod-shutdown-operator/api/v1alpha1"
	"github.com/giantswarm/pod-shutdown-operator/internal/operator"
)

// startWithAPI starts an operator with the HTTP endpoint and stops it at the test cleanup.
// It returns the stop function and the base URL of the endpoint.
func startWithAPI(c operator.Config) (func(), string) {
	c.APIBindAddress = freeAddress()
	stop := start(c)
	url := "http://" + c.APIBindAddress
	Eventually(func() (int, error) {
		resp, err := http.Get(url + "/progress")
		if err != nil {
			return 0, err
		}
		return resp.StatusCode, resp.Body.Close()
	}).Should(Equal(http.StatusOK))
	return stop, url
}

// leaseHolder returns the holder identity of the leader election Lease of leaderConfig.
func leaseHolder() string {
	c := leaderConfig("")
	var lease coordinationv1.Lease
	key := client.ObjectKey{Namespace: c.Manager.LeaderElectionNamespace, Name: c.Manager.LeaderElectionID}
	Expect(k8sClient.Get(ctx, key, &lease)).To(Succeed())
	return ptr.Deref(lease.Spec.HolderIdentity, "")
}

// bootCheckedBy returns the marker of the boot ID check in the request.
func bootCheckedBy() string {
	return getRequest().Status.BootCheckedBy
}

// completeOf returns the complete flag of GET /progress.
func completeOf(url string) func() any {
	return func() any { return progressOf(url)["complete"] }
}

var _ = Describe("The complete flag of GET /progress", func() {
	var web *appsv1.Deployment

	BeforeEach(func() {
		ns := newNamespace()
		DeferCleanup(cleanUp, ns)
		web = createDeployment(ns, "web", 2)
		createApplication(ns, "shop", workload(shutdownv1alpha1.WorkloadKindDeployment, "web"))
	})

	// leader returns the config of an operator that takes the Lease, with a boot ID.
	leader := func(bootID string) operator.Config {
		c := leaderConfig("leader")
		c.BootIDFile = bootIDFile(bootID)
		return c
	}

	It("is true after the leader that ran the shutdown completes it", func() {
		_, url := startWithAPI(leader(operatorBootID))
		completeShutdown()

		Eventually(completeOf(url)).Should(BeTrue())
		Expect(bootCheckedBy()).To(Equal(leaseHolder()))
	})

	It("is false after a leader change until the new leader checks the boot ID, and true after an equal boot ID", func() {
		stop, url := startWithAPI(leader(operatorBootID))
		completeShutdown()
		Eventually(completeOf(url)).Should(BeTrue())
		oldLeader := bootCheckedBy()
		stop()

		_, url = startWithAPI(leader(operatorBootID))

		// A new process is a new leader, so the marker of the old leader does not count.
		Eventually(leaseHolder).ShouldNot(BeElementOf("", oldLeader))
		Eventually(bootCheckedBy).Should(Equal(leaseHolder()))
		Eventually(completeOf(url)).Should(BeTrue())
		Expect(phaseOf()).To(Equal(shutdownv1alpha1.PhaseComplete))
		Expect(actionOf()).To(Equal(shutdownv1alpha1.ActionShutdown))
	})

	It("is false after the new leader finds a power cycle", func() {
		stop, url := startWithAPI(leader(operatorBootID))
		completeShutdown()
		Eventually(completeOf(url)).Should(BeTrue())
		oldLeader := bootCheckedBy()
		stop()

		_, url = startWithAPI(leader("boot-2"))

		Eventually(actionOf).Should(Equal(shutdownv1alpha1.ActionCancel))
		Expect(completeOf(url)()).To(BeFalse())
		Eventually(replicasOf(web)).Should(Equal(int32(2)))
		markReady(web, 2)
		Eventually(phaseOf).Should(Equal(shutdownv1alpha1.PhaseRestored))
		// The new leader writes no marker after a power cycle.
		Expect(bootCheckedBy()).To(Equal(oldLeader))
	})

	It("uses the identity of the process with no leader election", func() {
		stop, url := startWithAPI(operator.Config{})
		completeShutdown()
		Eventually(completeOf(url)).Should(BeTrue())
		oldProcess := bootCheckedBy()
		stop()

		// A restart is a new process, so it checks the boot ID again.
		_, url = startWithAPI(operator.Config{})
		Eventually(bootCheckedBy).ShouldNot(Equal(oldProcess))
		Eventually(completeOf(url)).Should(BeTrue())
	})
})
