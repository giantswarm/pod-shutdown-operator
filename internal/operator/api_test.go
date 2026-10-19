//go:build envtest

package operator_test

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	shutdownv1alpha1 "github.com/giantswarm/pod-shutdown-operator/api/v1alpha1"
)

var _ = Describe("API validation", func() {
	var namespace string

	BeforeEach(func() {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "api-"}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		namespace = ns.Name
	})

	AfterEach(func() {
		request := &shutdownv1alpha1.ShutdownRequest{ObjectMeta: metav1.ObjectMeta{Name: shutdownv1alpha1.ShutdownRequestName}}
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, request))).To(Succeed())
		// The operator lists ManagedApplications in all namespaces, so a test must not leave them.
		Expect(k8sClient.DeleteAllOf(ctx, &shutdownv1alpha1.ManagedApplication{}, client.InNamespace(namespace))).To(Succeed())
	})

	It("rejects a ShutdownRequest with a name other than cluster", func() {
		request := &shutdownv1alpha1.ShutdownRequest{
			ObjectMeta: metav1.ObjectMeta{Name: "other"},
			Spec:       shutdownv1alpha1.ShutdownRequestSpec{Action: shutdownv1alpha1.ActionShutdown},
		}
		err := k8sClient.Create(ctx, request)
		Expect(apierrors.IsInvalid(err)).To(BeTrue(), "error: %v", err)
		Expect(err.Error()).To(ContainSubstring("the name must be cluster"))
	})

	It("rejects a ShutdownRequest with an unknown action", func() {
		request := &shutdownv1alpha1.ShutdownRequest{
			ObjectMeta: metav1.ObjectMeta{Name: shutdownv1alpha1.ShutdownRequestName},
			Spec:       shutdownv1alpha1.ShutdownRequestSpec{Action: "Reboot"},
		}
		Expect(apierrors.IsInvalid(k8sClient.Create(ctx, request))).To(BeTrue())
	})

	It("sets the defaults of a ShutdownRequest", func() {
		request := &shutdownv1alpha1.ShutdownRequest{
			ObjectMeta: metav1.ObjectMeta{Name: shutdownv1alpha1.ShutdownRequestName},
			Spec:       shutdownv1alpha1.ShutdownRequestSpec{Action: shutdownv1alpha1.ActionShutdown},
		}
		Expect(k8sClient.Create(ctx, request)).To(Succeed())
		Expect(request.Spec.Budget).To(Equal(&metav1.Duration{Duration: 10 * time.Minute}))
		Expect(request.Spec.SafetyMargin).To(Equal(&metav1.Duration{Duration: 30 * time.Second}))
	})

	It("sets the defaults of a ManagedApplication", func() {
		app := &shutdownv1alpha1.ManagedApplication{
			ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "shop"},
			Spec: shutdownv1alpha1.ManagedApplicationSpec{Workloads: []shutdownv1alpha1.WorkloadReference{
				{Kind: shutdownv1alpha1.WorkloadKindDeployment, Name: "web"},
			}},
		}
		Expect(k8sClient.Create(ctx, app)).To(Succeed())
		Expect(app.Spec.GracePeriod).To(Equal(&metav1.Duration{Duration: 30 * time.Second}))
		Expect(app.Spec.ReadyTimeout).To(Equal(&metav1.Duration{Duration: 5 * time.Minute}))
	})

	It("rejects a ManagedApplication with no workloads", func() {
		app := &shutdownv1alpha1.ManagedApplication{
			ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "empty"},
			Spec:       shutdownv1alpha1.ManagedApplicationSpec{Workloads: []shutdownv1alpha1.WorkloadReference{}},
		}
		Expect(apierrors.IsInvalid(k8sClient.Create(ctx, app))).To(BeTrue())
	})

	It("rejects a ManagedApplication with an unknown workload kind", func() {
		app := &shutdownv1alpha1.ManagedApplication{
			ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "daemon"},
			Spec: shutdownv1alpha1.ManagedApplicationSpec{Workloads: []shutdownv1alpha1.WorkloadReference{
				{Kind: "DaemonSet", Name: "agent"},
			}},
		}
		Expect(apierrors.IsInvalid(k8sClient.Create(ctx, app))).To(BeTrue())
	})
})
