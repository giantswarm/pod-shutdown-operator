//go:build envtest

package operator_test

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	shutdownv1alpha1 "github.com/giantswarm/pod-shutdown-operator/api/v1alpha1"
	"github.com/giantswarm/pod-shutdown-operator/internal/operator"
)

// startOperator starts an operator against the test API server. The stop function stops it and waits.
func startOperator(c operator.Config) (func(), ctrl.Manager) {
	c.Manager.Metrics = metricsserver.Options{BindAddress: "0"}
	c.Manager.HealthProbeBindAddress = "0"
	// Each test starts new managers in one process, so the controller name repeats.
	c.Manager.Controller = config.Controller{SkipNameValidation: ptr.To(true)}
	if c.PollInterval == 0 {
		c.PollInterval = 100 * time.Millisecond
	}
	if c.FieldOwner == "" {
		c.FieldOwner = "test-operator"
	}

	mgr, err := operator.NewManager(cfg, c)
	Expect(err).NotTo(HaveOccurred())

	mgrCtx, mgrCancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer GinkgoRecover()
		defer close(done)
		Expect(mgr.Start(mgrCtx)).To(Succeed())
	}()
	return func() { mgrCancel(); <-done }, mgr
}

func newNamespace() string {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-"}}
	Expect(k8sClient.Create(ctx, ns)).To(Succeed())
	return ns.Name
}

func appLabels(name string) map[string]string {
	return map[string]string{"app": name}
}

func podTemplate(name string) corev1.PodTemplateSpec {
	return corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: appLabels(name)},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "busybox"}}},
	}
}

func createDeployment(ns, name string, replicas int32) *appsv1.Deployment {
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(replicas),
			Selector: &metav1.LabelSelector{MatchLabels: appLabels(name)},
			Template: podTemplate(name),
		},
	}
	Expect(k8sClient.Create(ctx, d)).To(Succeed())
	return d
}

func createStatefulSet(ns, name string, replicas int32) *appsv1.StatefulSet {
	s := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: appsv1.StatefulSetSpec{
			Replicas:    ptr.To(replicas),
			ServiceName: name,
			Selector:    &metav1.LabelSelector{MatchLabels: appLabels(name)},
			Template:    podTemplate(name),
		},
	}
	Expect(k8sClient.Create(ctx, s)).To(Succeed())
	return s
}

// createPod creates a Pod in the place of the Deployment, StatefulSet, and kubelet controllers,
// which do not run in envtest.
func createPod(ns, name string, labels map[string]string) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: labels},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "busybox"}}},
	}
	Expect(k8sClient.Create(ctx, pod)).To(Succeed())
}

// deletePod ends a Pod. A Pod with no node is deleted at once.
func deletePod(ns, name string) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}
	Expect(k8sClient.Delete(ctx, pod, client.GracePeriodSeconds(0))).To(Succeed())
}

func workload(kind shutdownv1alpha1.WorkloadKind, name string) shutdownv1alpha1.WorkloadReference {
	return shutdownv1alpha1.WorkloadReference{Kind: kind, Name: name}
}

func createApplication(ns, name string, workloads ...shutdownv1alpha1.WorkloadReference) {
	app := &shutdownv1alpha1.ManagedApplication{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       shutdownv1alpha1.ManagedApplicationSpec{Workloads: workloads},
	}
	Expect(k8sClient.Create(ctx, app)).To(Succeed())
}

func createShutdownRequest(action shutdownv1alpha1.Action) {
	request := &shutdownv1alpha1.ShutdownRequest{
		ObjectMeta: metav1.ObjectMeta{Name: shutdownv1alpha1.ShutdownRequestName},
		Spec:       shutdownv1alpha1.ShutdownRequestSpec{Action: action},
	}
	Expect(k8sClient.Create(ctx, request)).To(Succeed())
}

func getRequest() *shutdownv1alpha1.ShutdownRequest {
	request := &shutdownv1alpha1.ShutdownRequest{}
	Expect(k8sClient.Get(ctx, client.ObjectKey{Name: shutdownv1alpha1.ShutdownRequestName}, request)).To(Succeed())
	return request
}

func phaseOf() shutdownv1alpha1.Phase {
	return getRequest().Status.Phase
}

func stoppedOf() int32 {
	return getRequest().Status.Stopped
}

// replicasOf returns a function that reads spec.replicas of a Deployment or a StatefulSet.
func replicasOf(obj client.Object) func() int32 {
	return func() int32 {
		current := obj.DeepCopyObject().(client.Object)
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(obj), current)).To(Succeed())
		switch o := current.(type) {
		case *appsv1.Deployment:
			return *o.Spec.Replicas
		case *appsv1.StatefulSet:
			return *o.Spec.Replicas
		}
		Fail("replicasOf supports only Deployments and StatefulSets")
		return 0
	}
}

// cleanUp deletes the ShutdownRequest and the ManagedApplications of the namespaces.
// The operator lists ManagedApplications in all namespaces, so a test must not leave them.
// envtest runs no namespace controller, so a namespace delete does not delete its objects.
func cleanUp(namespaces ...string) {
	request := &shutdownv1alpha1.ShutdownRequest{ObjectMeta: metav1.ObjectMeta{Name: shutdownv1alpha1.ShutdownRequestName}}
	Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, request))).To(Succeed())
	Eventually(func() bool {
		return apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKeyFromObject(request), request))
	}).Should(BeTrue())
	for _, ns := range namespaces {
		Expect(k8sClient.DeleteAllOf(ctx, &shutdownv1alpha1.ManagedApplication{}, client.InNamespace(ns))).To(Succeed())
	}
}
