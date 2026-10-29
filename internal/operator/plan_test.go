//go:build envtest

package operator_test

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	shutdownv1alpha1 "github.com/giantswarm/pod-shutdown-operator/api/v1alpha1"
	"github.com/giantswarm/pod-shutdown-operator/internal/operator"
)

// createPlan creates a ShutdownPlan and returns its status.
func createPlan() shutdownv1alpha1.ShutdownPlanStatus {
	plan := &shutdownv1alpha1.ShutdownPlan{ObjectMeta: metav1.ObjectMeta{GenerateName: "plan-"}}
	Expect(k8sClient.Create(ctx, plan)).To(Succeed())
	Eventually(func() *metav1.Time {
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(plan), plan)).To(Succeed())
		return plan.Status.ComputedAt
	}).ShouldNot(BeNil())
	return plan.Status
}

// plannedApplication returns the tier and the plan of an application.
func plannedApplication(status shutdownv1alpha1.ShutdownPlanStatus, ns, name string) (int32, shutdownv1alpha1.PlannedApplication) {
	for _, group := range status.Groups {
		for _, tier := range group.Tiers {
			for _, app := range tier.Applications {
				if app.Namespace == ns && app.Name == name {
					return tier.Tier, app
				}
			}
		}
	}
	Fail("the plan does not hold " + ns + "/" + name)
	return 0, shutdownv1alpha1.PlannedApplication{}
}

// groupOf returns the group of an application.
func groupOf(status shutdownv1alpha1.ShutdownPlanStatus, ns, name string) shutdownv1alpha1.PlannedGroup {
	for _, group := range status.Groups {
		for _, tier := range group.Tiers {
			for _, app := range tier.Applications {
				if app.Namespace == ns && app.Name == name {
					return group
				}
			}
		}
	}
	Fail("the plan does not hold " + ns + "/" + name)
	return shutdownv1alpha1.PlannedGroup{}
}

// findingsWith returns the findings with a reason.
func findingsWith(status shutdownv1alpha1.ShutdownPlanStatus, reason string) []shutdownv1alpha1.Finding {
	var found []shutdownv1alpha1.Finding
	for _, f := range status.Findings {
		if f.Reason == reason {
			found = append(found, f)
		}
	}
	return found
}

func duration(d time.Duration) metav1.Duration {
	return metav1.Duration{Duration: d}
}

func cleanUpPlans() {
	Expect(k8sClient.DeleteAllOf(ctx, &shutdownv1alpha1.ShutdownPlan{})).To(Succeed())
}

var _ = Describe("A ShutdownPlan", func() {
	var (
		ns   string
		stop func()
	)

	BeforeEach(func() {
		ns = newNamespace()
	})

	AfterEach(func() {
		stop()
		cleanUpPlans()
		cleanUp(ns)
	})

	It("writes the groups with their tiers, applications, actions, and deadlines from the configured gracePeriod values", func() {
		stop, _ = startOperator(operator.Config{})
		createDeployment(ns, "web", 2)
		createStatefulSet(ns, "db", 1)
		createCronJob(ns, "report", false)
		createApplication(ns, "frontend", workload(shutdownv1alpha1.WorkloadKindDeployment, "web"))
		createApplication(ns, "database", workload(shutdownv1alpha1.WorkloadKindStatefulSet, "db"))
		createApplication(ns, "reports", workload(shutdownv1alpha1.WorkloadKindCronJob, "report"))
		setDependsOn(ns, "frontend", "database")
		setGracePeriod(ns, "frontend", 10*time.Second)
		setGracePeriod(ns, "database", 20*time.Second)

		status := createPlan()

		Expect(status.Result).To(Equal(shutdownv1alpha1.PlanResultOK))
		Expect(status.Findings).To(BeEmpty())
		Expect(status.Groups).To(HaveLen(2))

		// The budget is 10m and the safety margin is 30s, so the groups end 9m30s after the start.
		group := groupOf(status, ns, "frontend")
		Expect(group.LongestChain).To(Equal(duration(30 * time.Second)))
		Expect(group.Available).To(Equal(duration(9*time.Minute + 30*time.Second)))

		tier, frontend := plannedApplication(status, ns, "frontend")
		Expect(tier).To(Equal(int32(1)))
		Expect(frontend.GracePeriod).To(Equal(duration(10 * time.Second)))
		Expect(frontend.ShrunkGracePeriod).To(Equal(duration(10 * time.Second)))
		// The deadline leaves the gracePeriod of database.
		Expect(frontend.Deadline).To(Equal(duration(9*time.Minute + 10*time.Second)))
		Expect(frontend.Workloads).To(ConsistOf(shutdownv1alpha1.PlannedWorkload{
			Kind: shutdownv1alpha1.WorkloadKindDeployment, Name: "web", Action: "ScaleToZero",
		}))

		tier, database := plannedApplication(status, ns, "database")
		Expect(tier).To(Equal(int32(2)))
		Expect(database.Deadline).To(Equal(duration(9*time.Minute + 30*time.Second)))

		tier, reports := plannedApplication(status, ns, "reports")
		Expect(tier).To(Equal(int32(1)))
		Expect(reports.Workloads).To(ConsistOf(shutdownv1alpha1.PlannedWorkload{
			Kind: shutdownv1alpha1.WorkloadKindCronJob, Name: "report", Action: "Suspend",
		}))

		Expect(status.Report).To(ContainSubstring("Result: OK"))
		Expect(status.Report).To(ContainSubstring(ns + "/frontend"))
		Expect(status.Report).To(ContainSubstring("deadline 9m10s"))
	})

	It("leaves every other object unchanged, and starts the groups after the time limit of the Flux step", func() {
		fluxNS := newNamespace()
		DeferCleanup(cleanUp, fluxNS)
		kustomize := createDeployment(fluxNS, "kustomize-controller", 1)
		stop, _ = startOperator(operator.Config{
			FluxControllers: []types.NamespacedName{{Namespace: fluxNS, Name: "kustomize-controller"}},
			FluxStopTimeout: time.Minute,
		})
		web := createDeployment(ns, "web", 2)
		cronJob := createCronJob(ns, "report", false)
		createApplication(ns, "shop",
			workload(shutdownv1alpha1.WorkloadKindDeployment, "web"),
			workload(shutdownv1alpha1.WorkloadKindCronJob, "report"),
		)
		app := &shutdownv1alpha1.ManagedApplication{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "shop"}, app)).To(Succeed())
		objects := []client.Object{kustomize, web, cronJob, app}
		versions := map[client.Object]string{}
		for _, obj := range objects {
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(obj), obj)).To(Succeed())
			versions[obj] = obj.GetResourceVersion()
		}

		status := createPlan()

		Expect(groupOf(status, ns, "shop").Available).To(Equal(duration(8*time.Minute + 30*time.Second)))
		Consistently(func() map[client.Object]string {
			current := map[client.Object]string{}
			for _, obj := range objects {
				Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(obj), obj)).To(Succeed())
				current[obj] = obj.GetResourceVersion()
			}
			return current
		}, time.Second).Should(Equal(versions))
		err := k8sClient.Get(ctx, client.ObjectKey{Name: shutdownv1alpha1.ShutdownRequestName}, &shutdownv1alpha1.ShutdownRequest{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
	})

	It("plans from the record of the active ShutdownRequest during a real shutdown", func() {
		stop, _ = startOperator(operator.Config{})
		web := createDeployment(ns, "web", 2)
		createPod(ns, "web-0", appLabels("web"))
		createApplication(ns, "shop", workload(shutdownv1alpha1.WorkloadKindDeployment, "web"))
		createShutdownRequestWithBudget(5*time.Minute, 10*time.Second)
		// A pod remains, so the shutdown stays in ShuttingDown.
		Eventually(replicasOf(web)).Should(BeZero())
		// The record does not hold an application that comes after the start of the shutdown.
		createDeployment(ns, "late", 1)
		createApplication(ns, "late", workload(shutdownv1alpha1.WorkloadKindDeployment, "late"))
		// A workload that exists but comes after the record is not a missing workload.
		createDeployment(ns, "extra", 1)
		app := &shutdownv1alpha1.ManagedApplication{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "shop"}}
		addWorkload := client.RawPatch(types.MergePatchType,
			[]byte(`{"spec":{"workloads":[{"kind":"Deployment","name":"web"},{"kind":"Deployment","name":"extra"}]}}`))
		Expect(k8sClient.Patch(ctx, app, addWorkload)).To(Succeed())

		status := createPlan()

		Expect(phaseOf()).To(Equal(shutdownv1alpha1.PhaseShuttingDown))
		Expect(status.Result).To(Equal(shutdownv1alpha1.PlanResultOK))
		_, shop := plannedApplication(status, ns, "shop")
		Expect(shop.Workloads).To(HaveLen(1))
		Expect(shop.Deadline).To(Equal(duration(4*time.Minute + 50*time.Second)))
		Expect(status.Groups).To(HaveLen(1))
		Expect(status.Report).To(ContainSubstring("record"))
	})

	It("deletes a plan after its time to live", func() {
		stop, _ = startOperator(operator.Config{ShutdownPlanTTL: 2 * time.Second})
		plan := &shutdownv1alpha1.ShutdownPlan{ObjectMeta: metav1.ObjectMeta{GenerateName: "plan-"}}
		Expect(k8sClient.Create(ctx, plan)).To(Succeed())

		Eventually(func() *metav1.Time {
			Expect(client.IgnoreNotFound(k8sClient.Get(ctx, client.ObjectKeyFromObject(plan), plan))).To(Succeed())
			return plan.Status.ComputedAt
		}).ShouldNot(BeNil())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKeyFromObject(plan), plan))
		}).Should(BeTrue())
	})
})

var _ = Describe("The findings of a ShutdownPlan", func() {
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
		cleanUpPlans()
		cleanUp(ns)
	})

	It("reports a dependency cycle as an Error, and puts the cycle in one tier", func() {
		for _, name := range []string{"frontend", "a", "b"} {
			createApplication(ns, name, workload(shutdownv1alpha1.WorkloadKindDeployment, name))
			createDeployment(ns, name, 1)
		}
		setDependsOn(ns, "frontend", "a")
		setDependsOn(ns, "a", "b")
		setDependsOn(ns, "b", "a")

		status := createPlan()

		Expect(status.Result).To(Equal(shutdownv1alpha1.PlanResultErrors))
		cycles := findingsWith(status, shutdownv1alpha1.ReasonCycle)
		Expect(cycles).To(HaveLen(1))
		Expect(cycles[0].Severity).To(Equal(shutdownv1alpha1.SeverityError))
		Expect(cycles[0].Applications).To(ConsistOf(ns+"/a", ns+"/b"))
		Expect(cycles[0].Fallback).To(ContainSubstring("one parallel step"))
		tierA, _ := plannedApplication(status, ns, "a")
		tierB, _ := plannedApplication(status, ns, "b")
		Expect(tierA).To(Equal(int32(2)))
		Expect(tierB).To(Equal(int32(2)))
		Expect(status.Report).To(ContainSubstring("Cycle"))
	})

	It("reports an application that depends on itself as a cycle", func() {
		createDeployment(ns, "web", 1)
		createApplication(ns, "frontend", workload(shutdownv1alpha1.WorkloadKindDeployment, "web"))
		setDependsOn(ns, "frontend", "frontend")

		status := createPlan()

		cycles := findingsWith(status, shutdownv1alpha1.ReasonCycle)
		Expect(cycles).To(HaveLen(1))
		Expect(cycles[0].Applications).To(ConsistOf(ns + "/frontend"))
		tier, _ := plannedApplication(status, ns, "frontend")
		Expect(tier).To(Equal(int32(1)))
	})

	It("reports a depth above 3 as an Error, and a depth of 3 as no finding", func() {
		chain := func(prefix string, length int) {
			for i := range length {
				name := prefix + string(rune('1'+i))
				createApplication(ns, name, workload(shutdownv1alpha1.WorkloadKindDeployment, name))
				createDeployment(ns, name, 1)
				if i > 0 {
					setDependsOn(ns, prefix+string(rune('1'+i-1)), name)
				}
			}
		}
		chain("deep", 4)
		chain("flat", 3)

		status := createPlan()

		Expect(status.Result).To(Equal(shutdownv1alpha1.PlanResultErrors))
		depth := findingsWith(status, shutdownv1alpha1.ReasonDepthAboveLimit)
		Expect(depth).To(HaveLen(1))
		Expect(depth[0].Severity).To(Equal(shutdownv1alpha1.SeverityError))
		Expect(depth[0].Applications).To(ConsistOf(ns+"/deep1", ns+"/deep2", ns+"/deep3", ns+"/deep4"))
		Expect(depth[0].Fallback).To(ContainSubstring("real depth"))
		tier, _ := plannedApplication(status, ns, "deep4")
		Expect(tier).To(Equal(int32(4)))
	})

	It("reports a dependsOn name that does not exist as an Error", func() {
		createDeployment(ns, "web", 1)
		createApplication(ns, "frontend", workload(shutdownv1alpha1.WorkloadKindDeployment, "web"))
		setDependsOn(ns, "frontend", "missing")

		status := createPlan()

		Expect(status.Result).To(Equal(shutdownv1alpha1.PlanResultErrors))
		unknown := findingsWith(status, shutdownv1alpha1.ReasonUnknownDependency)
		Expect(unknown).To(HaveLen(1))
		Expect(unknown[0].Severity).To(Equal(shutdownv1alpha1.SeverityError))
		Expect(unknown[0].Applications).To(ConsistOf(ns + "/frontend"))
		Expect(unknown[0].Message).To(ContainSubstring(ns + "/missing"))
		Expect(unknown[0].Fallback).To(ContainSubstring("Ignore"))
	})

	It("reports a listed workload that does not exist as an Error, and leaves it out of the plan", func() {
		createDeployment(ns, "web", 1)
		createApplication(ns, "shop",
			workload(shutdownv1alpha1.WorkloadKindDeployment, "web"),
			workload(shutdownv1alpha1.WorkloadKindStatefulSet, "missing"),
		)

		status := createPlan()

		Expect(status.Result).To(Equal(shutdownv1alpha1.PlanResultErrors))
		missing := findingsWith(status, shutdownv1alpha1.ReasonMissingWorkload)
		Expect(missing).To(HaveLen(1))
		Expect(missing[0].Severity).To(Equal(shutdownv1alpha1.SeverityError))
		Expect(missing[0].Applications).To(ConsistOf(ns + "/shop"))
		Expect(missing[0].Message).To(ContainSubstring("StatefulSet missing"))
		Expect(missing[0].Fallback).To(ContainSubstring("Skip"))
		_, shop := plannedApplication(status, ns, "shop")
		Expect(shop.Workloads).To(ConsistOf(shutdownv1alpha1.PlannedWorkload{
			Kind: shutdownv1alpha1.WorkloadKindDeployment, Name: "web", Action: "ScaleToZero",
		}))
	})

	It("reports a group whose longest chain exceeds the time available as a Warning, and shows the shrink", func() {
		createDeployment(ns, "web", 1)
		createStatefulSet(ns, "db", 1)
		createApplication(ns, "frontend", workload(shutdownv1alpha1.WorkloadKindDeployment, "web"))
		createApplication(ns, "database", workload(shutdownv1alpha1.WorkloadKindStatefulSet, "db"))
		setDependsOn(ns, "frontend", "database")
		setGracePeriod(ns, "frontend", 8*time.Minute)
		setGracePeriod(ns, "database", 11*time.Minute)

		status := createPlan()

		Expect(status.Result).To(Equal(shutdownv1alpha1.PlanResultWarnings))
		shrinks := findingsWith(status, shutdownv1alpha1.ReasonGracePeriodShrink)
		Expect(shrinks).To(HaveLen(1))
		Expect(shrinks[0].Severity).To(Equal(shutdownv1alpha1.SeverityWarning))
		Expect(shrinks[0].Applications).To(ConsistOf(ns+"/frontend", ns+"/database"))
		Expect(shrinks[0].Fallback).To(ContainSubstring("Shrink"))

		// The chain is 19m and 9m30s is available, so each gracePeriod shrinks to one half.
		group := groupOf(status, ns, "frontend")
		Expect(group.LongestChain).To(Equal(duration(19 * time.Minute)))
		_, frontend := plannedApplication(status, ns, "frontend")
		Expect(frontend.GracePeriod).To(Equal(duration(8 * time.Minute)))
		Expect(frontend.ShrunkGracePeriod).To(Equal(duration(4 * time.Minute)))
		Expect(frontend.Deadline).To(Equal(duration(4 * time.Minute)))
		_, database := plannedApplication(status, ns, "database")
		Expect(database.ShrunkGracePeriod).To(Equal(duration(5*time.Minute + 30*time.Second)))
		Expect(status.Report).To(ContainSubstring("shrunk to 4m0s"))
	})
})
