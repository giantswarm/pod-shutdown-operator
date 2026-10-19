//go:build envtest

package operator_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/giantswarm/pod-shutdown-operator/internal/operator"
)

var (
	ctx         context.Context
	cancel      context.CancelFunc
	testEnv     *envtest.Environment
	cfg         *rest.Config
	k8sClient   client.Client
	watchClient client.WithWatch
)

func TestOperator(t *testing.T) {
	RegisterFailHandler(Fail)
	// The first use of a resource type in a new API server takes some seconds:
	// the informer must sync. The default timeout of 1s is too short for that.
	SetDefaultEventuallyTimeout(10 * time.Second)
	RunSpecs(t, "Operator Suite")
}

var _ = BeforeSuite(func() {
	logf.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true)))
	ctx, cancel = context.WithCancel(context.Background())

	testEnv = &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	if dir := firstEnvtestBinaryDir(); dir != "" {
		testEnv.BinaryAssetsDirectory = dir
	}

	var err error
	cfg, err = testEnv.Start()
	Expect(err).NotTo(HaveOccurred())

	k8sClient, err = client.New(cfg, client.Options{Scheme: operator.NewScheme()})
	Expect(err).NotTo(HaveOccurred())
	watchClient, err = client.NewWithWatch(cfg, client.Options{Scheme: operator.NewScheme()})
	Expect(err).NotTo(HaveOccurred())
})

var _ = AfterSuite(func() {
	cancel()
	Expect(testEnv.Stop()).To(Succeed())
})

// firstEnvtestBinaryDir returns the first envtest binary directory in bin/k8s.
// It lets an IDE run the tests with no KUBEBUILDER_ASSETS. Run `make test-envtest` once to fill bin/k8s.
func firstEnvtestBinaryDir() string {
	basePath := filepath.Join("..", "..", "bin", "k8s")
	entries, err := os.ReadDir(basePath)
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		if entry.IsDir() {
			return filepath.Join(basePath, entry.Name())
		}
	}
	return ""
}
