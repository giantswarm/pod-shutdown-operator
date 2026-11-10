//go:build e2e

// Package e2e tests the operator in a kind cluster, through the Kubernetes API and the HTTP endpoint.
// Run `make test-e2e`: it builds the image, creates the cluster, and installs the chart with values.yaml.
package e2e

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	shutdownv1alpha1 "gitlab.opendefense.cloud/odc/shutdown-op/pod-shutdown-operator/api/v1alpha1"
	"gitlab.opendefense.cloud/odc/shutdown-op/pod-shutdown-operator/internal/operator"
)

// The settings of the installed chart, from values.yaml.
const (
	operatorNamespace = "pod-shutdown-operator"
	operatorName      = "pod-shutdown-operator"
	leaseName         = "pod-shutdown-operator.shutdown.giantswarm.io"
	operatorReplicas  = 2
	apiURL            = "http://127.0.0.1:30082"
	apiToken          = "e2e-token"
	fluxNamespace     = "flux-system"

	// kubeContext is the context of the kind cluster that E2E_CLUSTER in Makefile.custom.mk names.
	kubeContext = "kind-pod-shutdown-operator-e2e"

	// statefulSetScaleUpDelay is equal to operator.statefulSetScaleUpDelay in values.yaml.
	statefulSetScaleUpDelay = 20 * time.Second
)

var fluxControllers = []string{"kustomize-controller", "helm-controller"}

var (
	ctx       context.Context
	cancel    context.CancelFunc
	k8sClient client.Client
)

func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	// Real pods take time to pull, start, and end.
	SetDefaultEventuallyTimeout(3 * time.Minute)
	SetDefaultEventuallyPollingInterval(time.Second)
	RunSpecs(t, "E2E Suite")
}

var _ = BeforeSuite(func() {
	ctx, cancel = context.WithCancel(context.Background())

	// The suite deletes namespaces, so it uses the kind context and never the current context of the kubeconfig.
	cfg, err := config.GetConfigWithContext(kubeContext)
	Expect(err).NotTo(HaveOccurred(), "the kubeconfig must have the context %s, run make test-e2e", kubeContext)
	k8sClient, err = client.New(cfg, client.Options{Scheme: operator.NewScheme()})
	Expect(err).NotTo(HaveOccurred())

	// An earlier run can leave a request in any phase. A request in Restored lets a new shutdown start.
	restore()
	// An aborted run can leave test namespaces, and their applications would be part of each shutdown.
	var namespaces corev1.NamespaceList
	Expect(k8sClient.List(ctx, &namespaces)).To(Succeed())
	for _, ns := range namespaces.Items {
		if strings.HasPrefix(ns.Name, "e2e-") {
			deleteNamespace(ns.Name)
		}
	}

	// An aborted run can also leave these objects.
	Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, operatorApplication()))).To(Succeed())
	deleteNamespace(fluxNamespace)

	// The test plays the Flux controllers with Deployments of its own.
	createNamespace(fluxNamespace)
	for _, name := range fluxControllers {
		createDeployment(fluxNamespace, name, 1, fastStop)
	}
	for _, name := range fluxControllers {
		Eventually(readyReplicas(fluxNamespace, name)).Should(Equal(int32(1)))
	}
})

var _ = AfterSuite(func() {
	// A new run creates the namespace again, so the delete must end first.
	deleteNamespace(fluxNamespace)
	cancel()
})

// restore cancels an active request and waits for the phase Restored.
func restore() {
	phase, err := progressPhase()
	Expect(err).NotTo(HaveOccurred())
	if phase == "" || phase == shutdownv1alpha1.PhaseRestored {
		return
	}
	Expect(post("/cancel", apiToken)).To(Equal(http.StatusAccepted))
	waitForRestored()
}

// operatorApplication returns the ManagedApplication that lists the operator, as a wrong entry.
func operatorApplication() *shutdownv1alpha1.ManagedApplication {
	return &shutdownv1alpha1.ManagedApplication{ObjectMeta: metav1.ObjectMeta{Namespace: operatorNamespace, Name: "operator"}}
}

// operatorDeployment returns the Deployment of the operator.
func operatorDeployment() *appsv1.Deployment {
	d := &appsv1.Deployment{}
	Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: operatorNamespace, Name: operatorName}, d)).To(Succeed())
	return d
}
