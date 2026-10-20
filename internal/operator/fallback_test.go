//go:build envtest

package operator_test

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	shutdownv1alpha1 "github.com/giantswarm/pod-shutdown-operator/api/v1alpha1"
	"github.com/giantswarm/pod-shutdown-operator/internal/operator"
)

// createGuardedDeployment creates a Deployment with 3 replicas and a policy that makes the API server
// reject a scale to 0 of it. It waits until the API server applies the policy.
func createGuardedDeployment(ns, deployment string) client.Object {
	policy := &admissionregistrationv1.ValidatingAdmissionPolicy{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "reject-scale-to-zero-"},
		Spec: admissionregistrationv1.ValidatingAdmissionPolicySpec{
			FailurePolicy: ptr.To(admissionregistrationv1.Fail),
			MatchConstraints: &admissionregistrationv1.MatchResources{
				ResourceRules: []admissionregistrationv1.NamedRuleWithOperations{{
					RuleWithOperations: admissionregistrationv1.RuleWithOperations{
						Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.Update},
						Rule: admissionregistrationv1.Rule{
							APIGroups:   []string{"apps"},
							APIVersions: []string{"v1"},
							Resources:   []string{"deployments"},
						},
					},
				}},
			},
			Validations: []admissionregistrationv1.Validation{{
				Expression: `object.metadata.name != "` + deployment + `" || object.spec.replicas != 0`,
				Message:    "scale to 0 is not allowed",
			}},
		},
	}
	Expect(k8sClient.Create(ctx, policy)).To(Succeed())
	DeferCleanup(func() { Expect(k8sClient.Delete(ctx, policy)).To(Succeed()) })

	binding := &admissionregistrationv1.ValidatingAdmissionPolicyBinding{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "reject-scale-to-zero-"},
		Spec: admissionregistrationv1.ValidatingAdmissionPolicyBindingSpec{
			PolicyName:        policy.Name,
			ValidationActions: []admissionregistrationv1.ValidationAction{admissionregistrationv1.Deny},
			MatchResources: &admissionregistrationv1.MatchResources{
				NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": ns}},
			},
		},
	}
	Expect(k8sClient.Create(ctx, binding)).To(Succeed())
	DeferCleanup(func() { Expect(k8sClient.Delete(ctx, binding)).To(Succeed()) })

	d := createDeployment(ns, deployment, 3)
	scaleToZero := client.RawPatch(types.MergePatchType, []byte(`{"spec":{"replicas":0}}`))
	Eventually(func() error {
		return k8sClient.Patch(ctx, d, scaleToZero, client.DryRunAll)
	}).Should(MatchError(ContainSubstring("scale to 0 is not allowed")))
	return d
}

var _ = Describe("The fallback rules in a real shutdown", func() {
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

	It("ignores a dependsOn edge that names an unknown application", func() {
		web := createDeployment(ns, "web", 1)
		createApplication(ns, "frontend", workload(shutdownv1alpha1.WorkloadKindDeployment, "web"))
		setDependsOn(ns, "frontend", "missing", "other-namespace/missing")

		completeShutdown()

		Expect(replicasOf(web)()).To(BeZero())
		Expect(stoppedOf()).To(Equal(int32(1)))
	})

	It("stops the applications of a cycle in one parallel step, after their dependents and before their dependencies", func() {
		web := createDeployment(ns, "web", 1)
		a := createDeployment(ns, "a", 1)
		b := createDeployment(ns, "b", 1)
		db := createStatefulSet(ns, "db", 1)
		for _, name := range []string{"web", "a", "b", "db"} {
			createPod(ns, name+"-0", appLabels(name))
		}
		createApplication(ns, "frontend", workload(shutdownv1alpha1.WorkloadKindDeployment, "web"))
		createApplication(ns, "a", workload(shutdownv1alpha1.WorkloadKindDeployment, "a"))
		createApplication(ns, "b", workload(shutdownv1alpha1.WorkloadKindDeployment, "b"))
		createApplication(ns, "database", workload(shutdownv1alpha1.WorkloadKindStatefulSet, "db"))
		setDependsOn(ns, "frontend", "a")
		setDependsOn(ns, "a", "b")
		setDependsOn(ns, "b", "a", "database")

		createShutdownRequest(shutdownv1alpha1.ActionShutdown)

		Eventually(replicasOf(web)).Should(BeZero())
		Consistently(replicasOf(a), time.Second).Should(Equal(int32(1)))
		Expect(replicasOf(b)()).To(Equal(int32(1)))

		deletePod(ns, "web-0")
		Eventually(replicasOf(a)).Should(BeZero())
		Eventually(replicasOf(b)).Should(BeZero())

		deletePod(ns, "a-0")
		Consistently(replicasOf(db), time.Second).Should(Equal(int32(1)))

		deletePod(ns, "b-0")
		Eventually(replicasOf(db)).Should(BeZero())
		deletePod(ns, "db-0")
		Eventually(phaseOf).Should(Equal(shutdownv1alpha1.PhaseComplete))
	})

	It("uses the real depth when the depth is above 3", func() {
		names := []string{"a1", "a2", "a3", "a4", "a5"}
		deployments := map[string]client.Object{}
		for i, name := range names {
			deployments[name] = createDeployment(ns, name, 1)
			createPod(ns, name+"-0", appLabels(name))
			createApplication(ns, name, workload(shutdownv1alpha1.WorkloadKindDeployment, name))
			if i > 0 {
				setDependsOn(ns, names[i-1], name)
			}
		}

		createShutdownRequest(shutdownv1alpha1.ActionShutdown)

		for _, name := range names[:3] {
			Eventually(replicasOf(deployments[name])).Should(BeZero())
			deletePod(ns, name+"-0")
		}
		Eventually(replicasOf(deployments["a4"])).Should(BeZero())
		Consistently(replicasOf(deployments["a5"]), time.Second).Should(Equal(int32(1)))

		deletePod(ns, "a4-0")
		Eventually(replicasOf(deployments["a5"])).Should(BeZero())
		deletePod(ns, "a5-0")
		Eventually(phaseOf).Should(Equal(shutdownv1alpha1.PhaseComplete))
	})

	It("skips a listed workload that does not exist", func() {
		web := createDeployment(ns, "web", 1)
		createPod(ns, "web-0", appLabels("web"))
		createApplication(ns, "shop",
			workload(shutdownv1alpha1.WorkloadKindDeployment, "missing"),
			workload(shutdownv1alpha1.WorkloadKindDeployment, "web"),
		)

		createShutdownRequest(shutdownv1alpha1.ActionShutdown)

		Eventually(replicasOf(web)).Should(BeZero())
		deletePod(ns, "web-0")
		Eventually(phaseOf).Should(Equal(shutdownv1alpha1.PhaseComplete))
		Expect(stoppedOf()).To(Equal(int32(1)))
	})

	It("reports an application with a rejected patch in status.failed and goes on", func() {
		guarded := createGuardedDeployment(ns, "guarded")
		db := createStatefulSet(ns, "db", 1)
		other := createDeployment(ns, "other", 1)
		createApplication(ns, "guarded", workload(shutdownv1alpha1.WorkloadKindDeployment, "guarded"))
		createApplication(ns, "database", workload(shutdownv1alpha1.WorkloadKindStatefulSet, "db"))
		createApplication(ns, "other", workload(shutdownv1alpha1.WorkloadKindDeployment, "other"))
		setDependsOn(ns, "guarded", "database")

		completeShutdown()

		status := getRequest().Status
		Expect(status.Failed).To(ConsistOf(ns + "/guarded"))
		Expect(status.Total).To(Equal(int32(3)))
		Expect(status.Stopped).To(Equal(int32(2)))
		Expect(replicasOf(db)()).To(BeZero())
		Expect(replicasOf(other)()).To(BeZero())
		Expect(replicasOf(guarded)()).To(Equal(int32(3)))
	})
})

var _ = Describe("A cycle at a scale-up", func() {
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

	It("starts the applications of a cycle in one parallel step, before their dependents", func() {
		web := createDeployment(ns, "web", 1)
		a := createDeployment(ns, "a", 1)
		b := createDeployment(ns, "b", 1)
		createApplication(ns, "frontend", workload(shutdownv1alpha1.WorkloadKindDeployment, "web"))
		createApplication(ns, "a", workload(shutdownv1alpha1.WorkloadKindDeployment, "a"))
		createApplication(ns, "b", workload(shutdownv1alpha1.WorkloadKindDeployment, "b"))
		setDependsOn(ns, "frontend", "a")
		setDependsOn(ns, "a", "b")
		setDependsOn(ns, "b", "a")
		completeShutdown()

		setAction(shutdownv1alpha1.ActionCancel)

		Eventually(replicasOf(a)).Should(Equal(int32(1)))
		Eventually(replicasOf(b)).Should(Equal(int32(1)))
		Consistently(replicasOf(web), time.Second).Should(BeZero())

		markReady(a, 1)
		markReady(b, 1)
		Eventually(replicasOf(web)).Should(Equal(int32(1)))
		markReady(web, 1)
		Eventually(phaseOf).Should(Equal(shutdownv1alpha1.PhaseRestored))
	})
})
