//go:build envtest

package operator_test

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	shutdownv1alpha1 "github.com/giantswarm/pod-shutdown-operator/api/v1alpha1"
	"github.com/giantswarm/pod-shutdown-operator/internal/operator"
)

const apiToken = "test-token"

// startAPI starts an operator with the HTTP endpoint. It returns the base URL of the endpoint and the path of the token file.
// The token file holds the token. An empty token means that the token file does not exist.
func startAPI(c operator.Config, token string) (string, string) {
	c.APITokenFile = filepath.Join(GinkgoT().TempDir(), "token")
	if token != "" {
		// The kubelet writes the Secret value as it is, so a line end can follow the token.
		Expect(os.WriteFile(c.APITokenFile, []byte(token+"\n"), 0o600)).To(Succeed())
	}
	c.APIBindAddress = freeAddress()
	stop, _ := startOperator(c)
	DeferCleanup(stop)

	url := "http://" + c.APIBindAddress
	Eventually(func() error {
		resp, err := http.Get(url + "/progress")
		if err != nil {
			return err
		}
		return resp.Body.Close()
	}).Should(Succeed())
	return url, c.APITokenFile
}

// freeAddress returns a local address with a port that no process uses at the time of the call.
func freeAddress() string {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	Expect(err).NotTo(HaveOccurred())
	addr := listener.Addr().String()
	Expect(listener.Close()).To(Succeed())
	return addr
}

// standbyConfig returns the config of an operator that never gets the lease, because another pod holds it.
// The controller does not run, so a test can set the phase by hand.
func standbyConfig() operator.Config {
	lease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", GenerateName: "standby-"},
		Spec: coordinationv1.LeaseSpec{
			HolderIdentity:       ptr.To("other-pod"),
			LeaseDurationSeconds: ptr.To(int32(3600)),
			AcquireTime:          &metav1.MicroTime{Time: time.Now()},
			RenewTime:            &metav1.MicroTime{Time: time.Now()},
		},
	}
	Expect(k8sClient.Create(ctx, lease)).To(Succeed())
	c := leaderConfig("standby")
	c.Manager.LeaderElectionID = lease.Name
	return c
}

// call sends a request to the endpoint. An empty token sends no Authorization header.
// It returns the status code and the body.
func call(method, url, token string) (int, []byte) {
	req, err := http.NewRequestWithContext(ctx, method, url, nil)
	Expect(err).NotTo(HaveOccurred())
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	Expect(err).NotTo(HaveOccurred())
	body, err := io.ReadAll(resp.Body)
	Expect(err).NotTo(HaveOccurred())
	Expect(resp.Body.Close()).To(Succeed())
	return resp.StatusCode, body
}

// progressOf calls GET /progress with no token and returns the fields of the response.
func progressOf(url string) map[string]any {
	code, body := call(http.MethodGet, url+"/progress", "")
	Expect(code).To(Equal(http.StatusOK))
	var progress map[string]any
	Expect(json.Unmarshal(body, &progress)).To(Succeed())
	return progress
}

// progressInPhase waits until GET /progress shows the phase, because the endpoint reads from a cache.
// It returns the fields of the response.
func progressInPhase(url string, phase shutdownv1alpha1.Phase) map[string]any {
	var progress map[string]any
	Eventually(func() any {
		progress = progressOf(url)
		return progress["phase"]
	}).Should(Equal(string(phase)))
	return progress
}

// createRequestInPhase creates a ShutdownRequest and sets its phase, in the place of the operator.
func createRequestInPhase(action shutdownv1alpha1.Action, phase shutdownv1alpha1.Phase) *shutdownv1alpha1.ShutdownRequest {
	createShutdownRequest(action)
	if phase != "" {
		request := getRequest()
		request.Status.Phase = phase
		request.Status.ShutdownStartedAt = &metav1.Time{Time: time.Now()}
		Expect(k8sClient.Status().Update(ctx, request)).To(Succeed())
	}
	return getRequest()
}

func requestExists() bool {
	err := k8sClient.Get(ctx, client.ObjectKey{Name: shutdownv1alpha1.ShutdownRequestName}, &shutdownv1alpha1.ShutdownRequest{})
	if apierrors.IsNotFound(err) {
		return false
	}
	Expect(err).NotTo(HaveOccurred())
	return true
}

var _ = Describe("The HTTP endpoint", func() {
	Context("in a standby pod", func() {
		var url, tokenFile string

		BeforeEach(func() {
			url, tokenFile = startAPI(standbyConfig(), apiToken)
			DeferCleanup(func() { cleanUp() })
		})

		Describe("the token", func() {
			It("refuses POST /shutdown with no token", func() {
				code, _ := call(http.MethodPost, url+"/shutdown", "")
				Expect(code).To(Equal(http.StatusUnauthorized))
				Consistently(requestExists, time.Second).Should(BeFalse())
			})

			It("refuses POST /shutdown with a wrong token", func() {
				code, _ := call(http.MethodPost, url+"/shutdown", "wrong-token")
				Expect(code).To(Equal(http.StatusUnauthorized))
				Consistently(requestExists, time.Second).Should(BeFalse())
			})

			It("refuses POST /cancel with a wrong token", func() {
				before := createRequestInPhase(shutdownv1alpha1.ActionShutdown, shutdownv1alpha1.PhaseShuttingDown)

				code, _ := call(http.MethodPost, url+"/cancel", "wrong-token")

				Expect(code).To(Equal(http.StatusUnauthorized))
				Expect(getRequest().ResourceVersion).To(Equal(before.ResourceVersion))
			})

			It("accepts the token from the token file", func() {
				code, _ := call(http.MethodPost, url+"/shutdown", apiToken)
				Expect(code).To(Equal(http.StatusAccepted))
				Expect(getRequest().Spec.Action).To(Equal(shutdownv1alpha1.ActionShutdown))
			})

			It("accepts the scheme bearer in lower case", func() {
				req, err := http.NewRequestWithContext(ctx, http.MethodPost, url+"/shutdown", nil)
				Expect(err).NotTo(HaveOccurred())
				req.Header.Set("Authorization", "bearer "+apiToken)
				resp, err := http.DefaultClient.Do(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.Body.Close()).To(Succeed())
				Expect(resp.StatusCode).To(Equal(http.StatusAccepted))
			})

			It("reads the token file at each call", func() {
				Expect(os.WriteFile(tokenFile, []byte("new-token"), 0o600)).To(Succeed())

				code, _ := call(http.MethodPost, url+"/shutdown", apiToken)
				Expect(code).To(Equal(http.StatusUnauthorized))
				code, _ = call(http.MethodPost, url+"/shutdown", "new-token")
				Expect(code).To(Equal(http.StatusAccepted))
			})

			It("refuses every call when the token file holds only white space", func() {
				Expect(os.WriteFile(tokenFile, []byte(" \n"), 0o600)).To(Succeed())

				code, _ := call(http.MethodPost, url+"/shutdown", "")
				Expect(code).To(Equal(http.StatusUnauthorized))
				code, _ = call(http.MethodPost, url+"/shutdown", apiToken)
				Expect(code).To(Equal(http.StatusUnauthorized))
				Consistently(requestExists, time.Second).Should(BeFalse())
			})
		})

		Describe("POST /shutdown", func() {
			It("creates a ShutdownRequest when none exists", func() {
				code, _ := call(http.MethodPost, url+"/shutdown", apiToken)

				Expect(code).To(Equal(http.StatusAccepted))
				request := getRequest()
				Expect(request.Spec.Action).To(Equal(shutdownv1alpha1.ActionShutdown))
				Expect(request.Status.Phase).To(BeEmpty())
			})

			DescribeTable("does nothing in a request with the action Shutdown",
				func(phase shutdownv1alpha1.Phase) {
					before := createRequestInPhase(shutdownv1alpha1.ActionShutdown, phase)

					code, _ := call(http.MethodPost, url+"/shutdown", apiToken)

					Expect(code).To(Equal(http.StatusAccepted))
					Expect(getRequest().ResourceVersion).To(Equal(before.ResourceVersion))
				},
				Entry("with no phase", shutdownv1alpha1.Phase("")),
				Entry("in ShuttingDown", shutdownv1alpha1.PhaseShuttingDown),
				Entry("in Complete", shutdownv1alpha1.PhaseComplete),
				Entry("in ScalingUp", shutdownv1alpha1.PhaseScalingUp),
			)

			// In ShuttingDown and Complete, the action Cancel waits for the leader. The latest call wins.
			DescribeTable("sets the action Shutdown in a request with the action Cancel and keeps the request",
				func(phase shutdownv1alpha1.Phase) {
					before := createRequestInPhase(shutdownv1alpha1.ActionCancel, phase)

					code, _ := call(http.MethodPost, url+"/shutdown", apiToken)

					Expect(code).To(Equal(http.StatusAccepted))
					request := getRequest()
					Expect(request.UID).To(Equal(before.UID))
					Expect(request.Spec.Action).To(Equal(shutdownv1alpha1.ActionShutdown))
					Expect(request.Status).To(Equal(before.Status))
				},
				Entry("in ShuttingDown", shutdownv1alpha1.PhaseShuttingDown),
				Entry("in Complete", shutdownv1alpha1.PhaseComplete),
				Entry("in ScalingUp", shutdownv1alpha1.PhaseScalingUp),
			)

			DescribeTable("creates a new request after Restored",
				func(action shutdownv1alpha1.Action) {
					before := createRequestInPhase(action, shutdownv1alpha1.PhaseRestored)

					code, _ := call(http.MethodPost, url+"/shutdown", apiToken)

					Expect(code).To(Equal(http.StatusAccepted))
					request := getRequest()
					Expect(request.UID).NotTo(Equal(before.UID))
					Expect(request.Spec.Action).To(Equal(shutdownv1alpha1.ActionShutdown))
					Expect(request.Status.Phase).To(BeEmpty())
				},
				Entry("with the action Cancel", shutdownv1alpha1.ActionCancel),
				Entry("with the action Shutdown", shutdownv1alpha1.ActionShutdown),
			)
		})

		Describe("POST /cancel", func() {
			It("does nothing when no request exists", func() {
				code, _ := call(http.MethodPost, url+"/cancel", apiToken)

				Expect(code).To(Equal(http.StatusAccepted))
				Consistently(requestExists, time.Second).Should(BeFalse())
			})

			// In ScalingUp, the action Shutdown waits for the leader. The latest call wins.
			DescribeTable("sets the action Cancel",
				func(phase shutdownv1alpha1.Phase) {
					before := createRequestInPhase(shutdownv1alpha1.ActionShutdown, phase)

					code, _ := call(http.MethodPost, url+"/cancel", apiToken)

					Expect(code).To(Equal(http.StatusAccepted))
					request := getRequest()
					Expect(request.Spec.Action).To(Equal(shutdownv1alpha1.ActionCancel))
					Expect(request.Status).To(Equal(before.Status))
				},
				Entry("in ShuttingDown", shutdownv1alpha1.PhaseShuttingDown),
				Entry("in Complete", shutdownv1alpha1.PhaseComplete),
				Entry("in ScalingUp", shutdownv1alpha1.PhaseScalingUp),
			)

			DescribeTable("does nothing",
				func(action shutdownv1alpha1.Action, phase shutdownv1alpha1.Phase) {
					before := createRequestInPhase(action, phase)

					code, _ := call(http.MethodPost, url+"/cancel", apiToken)

					Expect(code).To(Equal(http.StatusAccepted))
					Expect(getRequest().ResourceVersion).To(Equal(before.ResourceVersion))
				},
				Entry("with no phase", shutdownv1alpha1.ActionShutdown, shutdownv1alpha1.Phase("")),
				Entry("in ScalingUp with the action Cancel", shutdownv1alpha1.ActionCancel, shutdownv1alpha1.PhaseScalingUp),
				Entry("in Restored", shutdownv1alpha1.ActionShutdown, shutdownv1alpha1.PhaseRestored),
			)
		})

		Describe("GET /progress", func() {
			It("needs no token and shows no request as an empty phase", func() {
				Expect(progressOf(url)).To(Equal(map[string]any{
					"phase":    "",
					"complete": false,
					"stopped":  float64(0),
					"total":    float64(0),
				}))
			})

			It("shows the phase, the counts, and the budget remainder, and no application names", func() {
				createShutdownRequestWithBudget(10*time.Minute, 30*time.Second)
				request := getRequest()
				request.Status = shutdownv1alpha1.ShutdownRequestStatus{
					Phase:             shutdownv1alpha1.PhaseShuttingDown,
					ShutdownStartedAt: &metav1.Time{Time: time.Now().Add(-time.Minute)},
					Stopped:           2,
					Total:             3,
					Failed:            []string{"shop/db"},
					Unmanaged:         []shutdownv1alpha1.UnmanagedWorkload{{Namespace: "shop", Kind: "Deployment", Name: "cache"}},
				}
				Expect(k8sClient.Status().Update(ctx, request)).To(Succeed())
				progressInPhase(url, shutdownv1alpha1.PhaseShuttingDown)

				code, body := call(http.MethodGet, url+"/progress", "")

				Expect(code).To(Equal(http.StatusOK))
				Expect(string(body)).NotTo(ContainSubstring("shop"))
				var progress map[string]any
				Expect(json.Unmarshal(body, &progress)).To(Succeed())
				Expect(progress).To(HaveLen(5))
				Expect(progress).To(HaveKeyWithValue("phase", "ShuttingDown"))
				Expect(progress).To(HaveKeyWithValue("complete", false))
				Expect(progress).To(HaveKeyWithValue("stopped", float64(2)))
				Expect(progress).To(HaveKeyWithValue("total", float64(3)))
				// The remainder is the budget minus the time since the start. It ignores the safety margin.
				Expect(progress).To(HaveKeyWithValue("budgetRemainingSeconds", BeNumerically("~", 540, 5)))
			})

			It("shows a budget remainder of 0 after the end of the budget", func() {
				createShutdownRequestWithBudget(time.Minute, 0)
				request := getRequest()
				request.Status.Phase = shutdownv1alpha1.PhaseComplete
				request.Status.ShutdownStartedAt = &metav1.Time{Time: time.Now().Add(-2 * time.Minute)}
				Expect(k8sClient.Status().Update(ctx, request)).To(Succeed())

				Expect(progressInPhase(url, shutdownv1alpha1.PhaseComplete)).To(HaveKeyWithValue("budgetRemainingSeconds", float64(0)))
			})

			DescribeTable("shows complete as true only in the phase Complete",
				func(phase shutdownv1alpha1.Phase, complete bool) {
					createRequestInPhase(shutdownv1alpha1.ActionShutdown, phase)
					Expect(progressInPhase(url, phase)).To(HaveKeyWithValue("complete", complete))
				},
				Entry("with no phase", shutdownv1alpha1.Phase(""), false),
				Entry("in ShuttingDown", shutdownv1alpha1.PhaseShuttingDown, false),
				Entry("in Complete", shutdownv1alpha1.PhaseComplete, true),
				Entry("in ScalingUp", shutdownv1alpha1.PhaseScalingUp, false),
				Entry("in Restored", shutdownv1alpha1.PhaseRestored, false),
			)

			DescribeTable("shows no budget remainder outside ShuttingDown and Complete",
				func(phase shutdownv1alpha1.Phase) {
					createRequestInPhase(shutdownv1alpha1.ActionShutdown, phase)
					Expect(progressInPhase(url, phase)).NotTo(HaveKey("budgetRemainingSeconds"))
				},
				Entry("with no phase", shutdownv1alpha1.Phase("")),
				Entry("in ScalingUp", shutdownv1alpha1.PhaseScalingUp),
				Entry("in Restored", shutdownv1alpha1.PhaseRestored),
			)
		})
	})

	Context("with no token file", func() {
		It("refuses every call that needs a token", func() {
			url, _ := startAPI(standbyConfig(), "")
			DeferCleanup(func() { cleanUp() })

			code, _ := call(http.MethodPost, url+"/shutdown", "")
			Expect(code).To(Equal(http.StatusUnauthorized))
			code, _ = call(http.MethodPost, url+"/shutdown", apiToken)
			Expect(code).To(Equal(http.StatusUnauthorized))
			Consistently(requestExists, time.Second).Should(BeFalse())

			before := createRequestInPhase(shutdownv1alpha1.ActionShutdown, shutdownv1alpha1.PhaseShuttingDown)
			code, _ = call(http.MethodPost, url+"/cancel", apiToken)
			Expect(code).To(Equal(http.StatusUnauthorized))
			Expect(getRequest().ResourceVersion).To(Equal(before.ResourceVersion))
		})
	})

	Context("in the leader", func() {
		It("runs a shutdown and a cancel through the endpoint", func() {
			ns := newNamespace()
			DeferCleanup(cleanUp, ns)
			url, _ := startAPI(operator.Config{}, apiToken)
			web := createDeployment(ns, "web", 2)
			createApplication(ns, "shop", workload(shutdownv1alpha1.WorkloadKindDeployment, "web"))

			code, _ := call(http.MethodPost, url+"/shutdown", apiToken)
			Expect(code).To(Equal(http.StatusAccepted))
			Eventually(func() any { return progressOf(url)["complete"] }).Should(BeTrue())
			Expect(replicasOf(web)()).To(BeZero())
			Expect(progressOf(url)).To(And(
				HaveKeyWithValue("stopped", float64(1)),
				HaveKeyWithValue("total", float64(1)),
			))

			code, _ = call(http.MethodPost, url+"/cancel", apiToken)
			Expect(code).To(Equal(http.StatusAccepted))
			Eventually(replicasOf(web)).Should(Equal(int32(2)))
			markReady(web, 2)
			Eventually(func() any { return progressOf(url)["phase"] }).Should(Equal("Restored"))
		})
	})
})
