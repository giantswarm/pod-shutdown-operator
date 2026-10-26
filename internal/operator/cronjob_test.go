//go:build envtest

package operator_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	shutdownv1alpha1 "github.com/giantswarm/pod-shutdown-operator/api/v1alpha1"
	"github.com/giantswarm/pod-shutdown-operator/internal/operator"
)

func createCronJob(ns, name string, suspend bool) *batchv1.CronJob {
	cj := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: batchv1.CronJobSpec{
			Schedule: "*/5 * * * *",
			Suspend:  ptr.To(suspend),
			JobTemplate: batchv1.JobTemplateSpec{
				Spec: batchv1.JobSpec{Template: podTemplate(name)},
			},
		},
	}
	cj.Spec.JobTemplate.Spec.Template.Spec.RestartPolicy = "Never"
	Expect(k8sClient.Create(ctx, cj)).To(Succeed())
	return cj
}

// suspendOf returns a function that reads spec.suspend of a CronJob.
func suspendOf(cj *batchv1.CronJob) func() bool {
	return func() bool {
		current := &batchv1.CronJob{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(cj), current)).To(Succeed())
		return ptr.Deref(current.Spec.Suspend, false)
	}
}

var _ = Describe("CronJobs", func() {
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

	It("suspends each CronJob and keeps a CronJob that was suspended on purpose suspended after the restore", func() {
		report := createCronJob(ns, "report", false)
		paused := createCronJob(ns, "paused", true)
		createApplication(ns, "jobs",
			workload(shutdownv1alpha1.WorkloadKindCronJob, "report"),
			workload(shutdownv1alpha1.WorkloadKindCronJob, "paused"),
		)

		createShutdownRequest(shutdownv1alpha1.ActionShutdown)

		Eventually(suspendOf(report)).Should(BeTrue())
		Eventually(phaseOf).Should(Equal(shutdownv1alpha1.PhaseComplete))
		Expect(suspendOf(paused)()).To(BeTrue())
		Expect(getRequest().Status.RecordedState.Applications[0].Workloads).To(Equal([]shutdownv1alpha1.RecordedWorkload{
			{Kind: shutdownv1alpha1.WorkloadKindCronJob, Name: "report", Suspend: ptr.To(false)},
			{Kind: shutdownv1alpha1.WorkloadKindCronJob, Name: "paused", Suspend: ptr.To(true)},
		}))

		setAction(shutdownv1alpha1.ActionCancel)

		Eventually(suspendOf(report)).Should(BeFalse())
		Eventually(phaseOf).Should(Equal(shutdownv1alpha1.PhaseRestored))
		Expect(suspendOf(paused)()).To(BeTrue())
		Expect(getRequest().Status.Degraded).To(BeEmpty())
	})
})
