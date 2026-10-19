//go:build envtest

package operator_test

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"

	shutdownv1alpha1 "github.com/giantswarm/pod-shutdown-operator/api/v1alpha1"
	"github.com/giantswarm/pod-shutdown-operator/internal/operator"
)

// statusWriters returns the field managers that wrote the status of the ShutdownRequest.
func statusWriters() []string {
	var owners []string
	for _, entry := range getRequest().ManagedFields {
		if entry.Subresource == "status" {
			owners = append(owners, entry.Manager)
		}
	}
	return owners
}

func leaderConfig(fieldOwner string) operator.Config {
	return operator.Config{
		FieldOwner: fieldOwner,
		Manager: ctrl.Options{
			LeaderElection:                true,
			LeaderElectionID:              "leader-test",
			LeaderElectionNamespace:       "default",
			LeaderElectionReleaseOnCancel: true,
			LeaseDuration:                 ptr.To(2 * time.Second),
			RenewDeadline:                 ptr.To(time.Second),
			RetryPeriod:                   ptr.To(200 * time.Millisecond),
		},
	}
}

var _ = Describe("Leader election", func() {
	It("lets only the lease holder write status", func() {
		ns := newNamespace()
		DeferCleanup(cleanUp, ns)

		stopA, mgrA := startOperator(leaderConfig("manager-a"))
		Eventually(mgrA.Elected(), 10*time.Second).Should(BeClosed())
		stopB, mgrB := startOperator(leaderConfig("manager-b"))
		DeferCleanup(stopB)

		createDeployment(ns, "web", 1)
		createPod(ns, "web-0", appLabels("web"))
		createApplication(ns, "shop", workload(shutdownv1alpha1.WorkloadKindDeployment, "web"))
		createShutdownRequest(shutdownv1alpha1.ActionShutdown)

		Eventually(phaseOf).Should(Equal(shutdownv1alpha1.PhaseShuttingDown))
		Consistently(mgrB.Elected(), 2*time.Second).ShouldNot(BeClosed())
		Expect(statusWriters()).To(ConsistOf("manager-a"))

		stopA()
		Eventually(mgrB.Elected(), 10*time.Second).Should(BeClosed())
		deletePod(ns, "web-0")

		Eventually(phaseOf).Should(Equal(shutdownv1alpha1.PhaseComplete))
		Expect(statusWriters()).To(ContainElement("manager-b"))
	})
})
