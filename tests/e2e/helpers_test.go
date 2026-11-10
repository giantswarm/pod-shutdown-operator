//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	shutdownv1alpha1 "gitlab.opendefense.cloud/odc/shutdown-op/pod-shutdown-operator/api/v1alpha1"
	"gitlab.opendefense.cloud/odc/shutdown-op/pod-shutdown-operator/internal/api"
)

const image = "gsoci.azurecr.io/giantswarm/busybox:1.37.0"

// stopBehavior is the reaction of a test container to SIGTERM.
type stopBehavior struct {
	script                        string
	terminationGracePeriodSeconds int64
}

var (
	// fastStop ends at once.
	fastStop = stopBehavior{script: `trap "exit 0" TERM; while true; do sleep 1; done`, terminationGracePeriodSeconds: 10}
	// slowStop ends 40 seconds after SIGTERM, inside its termination grace period.
	slowStop = stopBehavior{script: `trap "sleep 40; exit 0" TERM; while true; do sleep 1; done`, terminationGracePeriodSeconds: 120}
	// noStop ignores SIGTERM, so only the kubelet or a forced delete ends it.
	noStop = stopBehavior{script: `trap "" TERM; while true; do sleep 1; done`, terminationGracePeriodSeconds: 600}
)

func labels(name string) map[string]string {
	return map[string]string{"app": name}
}

func podTemplate(name string, stop stopBehavior) corev1.PodTemplateSpec {
	return corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: labels(name)},
		Spec: corev1.PodSpec{
			TerminationGracePeriodSeconds: ptr.To(stop.terminationGracePeriodSeconds),
			Containers: []corev1.Container{{
				Name:    "app",
				Image:   image,
				Command: []string{"sh", "-c", stop.script},
			}},
		},
	}
}

// createNamespace creates a namespace. It returns the name, which gets a suffix when name ends with "-".
func createNamespace(name string) string {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if strings.HasSuffix(name, "-") {
		ns = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: name}}
	}
	Expect(k8sClient.Create(ctx, ns)).To(Succeed())
	return ns.Name
}

// deleteNamespace deletes a namespace and waits until it is gone. A ManagedApplication that stays in a namespace
// in deletion is part of the next shutdown. It deletes the pods by force, because some ignore SIGTERM.
func deleteNamespace(name string) {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, ns))).To(Succeed())
	Eventually(func(g Gomega) error {
		g.Expect(k8sClient.DeleteAllOf(ctx, &corev1.Pod{}, client.InNamespace(name), client.GracePeriodSeconds(0))).To(Succeed())
		return k8sClient.Get(ctx, client.ObjectKeyFromObject(ns), ns)
	}).Should(Satisfy(apierrors.IsNotFound))
}

func createDeployment(ns, name string, replicas int32, stop stopBehavior) {
	Expect(k8sClient.Create(ctx, &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(replicas),
			Selector: &metav1.LabelSelector{MatchLabels: labels(name)},
			Template: podTemplate(name, stop),
		},
	})).To(Succeed())
}

func createStatefulSet(ns, name string, replicas int32, stop stopBehavior) {
	Expect(k8sClient.Create(ctx, &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: appsv1.StatefulSetSpec{
			Replicas:    ptr.To(replicas),
			ServiceName: name,
			Selector:    &metav1.LabelSelector{MatchLabels: labels(name)},
			Template:    podTemplate(name, stop),
		},
	})).To(Succeed())
}

func createCronJob(ns, name string) {
	Expect(k8sClient.Create(ctx, &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: batchv1.CronJobSpec{
			Schedule: "0 0 1 1 *",
			JobTemplate: batchv1.JobTemplateSpec{Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				RestartPolicy: corev1.RestartPolicyNever,
				Containers:    []corev1.Container{{Name: "job", Image: image, Command: []string{"true"}}},
			}}}},
		},
	})).To(Succeed())
}

func workload(kind shutdownv1alpha1.WorkloadKind, name string) shutdownv1alpha1.WorkloadReference {
	return shutdownv1alpha1.WorkloadReference{Kind: kind, Name: name}
}

func createApplication(ns, name string, spec shutdownv1alpha1.ManagedApplicationSpec) {
	Expect(k8sClient.Create(ctx, &shutdownv1alpha1.ManagedApplication{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       spec,
	})).To(Succeed())
}

// The polled helpers return an error and do not fail the test, so that Eventually tries again after a short failure.
// Expect checks that the error is nil.

// replicas returns spec.replicas of a Deployment or a StatefulSet.
func replicas(obj client.Object, ns, name string) func() (int32, error) {
	return func() (int32, error) {
		if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, obj); err != nil {
			return 0, err
		}
		switch o := obj.(type) {
		case *appsv1.Deployment:
			return ptr.Deref(o.Spec.Replicas, 1), nil
		case *appsv1.StatefulSet:
			return ptr.Deref(o.Spec.Replicas, 1), nil
		}
		return 0, fmt.Errorf("no replicas in %T", obj)
	}
}

func deploymentReplicas(ns, name string) func() (int32, error) {
	return replicas(&appsv1.Deployment{}, ns, name)
}

func statefulSetReplicas(ns, name string) func() (int32, error) {
	return replicas(&appsv1.StatefulSet{}, ns, name)
}

// readyReplicas returns status.readyReplicas of a Deployment.
func readyReplicas(ns, name string) func() (int32, error) {
	return func() (int32, error) {
		d := &appsv1.Deployment{}
		err := k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, d)
		return d.Status.ReadyReplicas, err
	}
}

// statefulSetReady returns status.readyReplicas of a StatefulSet.
func statefulSetReady(ns, name string) func() (int32, error) {
	return func() (int32, error) {
		s := &appsv1.StatefulSet{}
		err := k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, s)
		return s.Status.ReadyReplicas, err
	}
}

func cronJobSuspended(ns, name string) func() (bool, error) {
	return func() (bool, error) {
		c := &batchv1.CronJob{}
		err := k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, c)
		return ptr.Deref(c.Spec.Suspend, false), err
	}
}

// pods returns the pods with the label app=name.
func pods(ns, name string) func() ([]corev1.Pod, error) {
	return func() ([]corev1.Pod, error) {
		var list corev1.PodList
		err := k8sClient.List(ctx, &list, client.InNamespace(ns), client.MatchingLabels(labels(name)))
		return list.Items, err
	}
}

// runningPods returns the pods with the label app=name that are not in a terminal phase, as the operator counts them.
// After its container ends, a pod in deletion stays for a short time in a terminal phase.
func runningPods(ns, name string) func() ([]corev1.Pod, error) {
	return func() ([]corev1.Pod, error) {
		list, err := pods(ns, name)()
		return slices.DeleteFunc(list, func(pod corev1.Pod) bool {
			return pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed
		}), err
	}
}

// operatorPods returns the running pods of the operator that are not in deletion.
func operatorPods() ([]corev1.Pod, error) {
	var list corev1.PodList
	if err := k8sClient.List(ctx, &list, client.InNamespace(operatorNamespace),
		client.MatchingLabels{"app.kubernetes.io/name": operatorName}); err != nil {
		return nil, err
	}
	var running []corev1.Pod
	for _, pod := range list.Items {
		if pod.DeletionTimestamp == nil && pod.Status.Phase == corev1.PodRunning {
			running = append(running, pod)
		}
	}
	return running, nil
}

// leaseHolder returns the name of the pod that holds the leader election lease.
// The identity of controller-runtime is the pod name, an underscore, and a UID.
func leaseHolder() (string, error) {
	lease := &coordinationv1.Lease{}
	err := k8sClient.Get(ctx, client.ObjectKey{Namespace: operatorNamespace, Name: leaseName}, lease)
	if apierrors.IsNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	pod, _, _ := strings.Cut(ptr.Deref(lease.Spec.HolderIdentity, ""), "_")
	return pod, nil
}

func shutdownRequest() *shutdownv1alpha1.ShutdownRequest {
	request := &shutdownv1alpha1.ShutdownRequest{}
	Expect(k8sClient.Get(ctx, client.ObjectKey{Name: shutdownv1alpha1.ShutdownRequestName}, request)).To(Succeed())
	return request
}

func phase() (shutdownv1alpha1.Phase, error) {
	request := &shutdownv1alpha1.ShutdownRequest{}
	err := k8sClient.Get(ctx, client.ObjectKey{Name: shutdownv1alpha1.ShutdownRequestName}, request)
	return request.Status.Phase, err
}

// httpClient has a timeout, so that a call that hangs cannot block the test.
var httpClient = &http.Client{Timeout: 10 * time.Second}

// post sends a call to the HTTP endpoint and returns the status code. An empty token sends no Authorization header.
func post(path, token string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL+path, nil)
	if err != nil {
		return 0, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode, nil
}

// progress returns the response of GET /progress.
func progress() (api.Progress, error) {
	var p api.Progress
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL+"/progress", nil)
	if err != nil {
		return p, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return p, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return p, fmt.Errorf("GET /progress returned %d", resp.StatusCode)
	}
	err = json.NewDecoder(resp.Body).Decode(&p)
	return p, err
}

// complete returns the complete flag of GET /progress.
func complete() (bool, error) {
	p, err := progress()
	return p.Complete, err
}

// progressPhase returns the phase of GET /progress.
func progressPhase() (shutdownv1alpha1.Phase, error) {
	p, err := progress()
	return p.Phase, err
}

// waitForRestored waits for the phase Restored after a cancel.
func waitForRestored() {
	Eventually(progressPhase, 5*time.Minute).Should(Equal(shutdownv1alpha1.PhaseRestored))
}

// expectOperatorUp checks that the operator keeps all its pods, and that they run on a control-plane node.
func expectOperatorUp() {
	GinkgoHelper()
	Expect(ptr.Deref(operatorDeployment().Spec.Replicas, 1)).To(Equal(int32(operatorReplicas)))
	running, err := operatorPods()
	Expect(err).NotTo(HaveOccurred())
	Expect(running).To(HaveLen(operatorReplicas))
	for _, pod := range running {
		node := &corev1.Node{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Name: pod.Spec.NodeName}, node)).To(Succeed())
		Expect(node.Labels).To(HaveKey("node-role.kubernetes.io/control-plane"))
	}
}
