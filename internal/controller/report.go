package controller

import (
	"fmt"
	"slices"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	shutdownv1alpha1 "github.com/giantswarm/pod-shutdown-operator/api/v1alpha1"
)

// report returns the plan as text. It lists each application with its group, tier, deadline, and findings,
// so that a team finds its own application.
func report(status shutdownv1alpha1.ShutdownPlanStatus, source string, spec shutdownv1alpha1.ShutdownRequestSpec, groupsStart time.Duration) string {
	var b strings.Builder
	fmt.Fprintf(&b, "ShutdownPlan computed at %s from %s.\n", status.ComputedAt.UTC().Format(time.RFC3339), source)
	b.WriteString("The plan shows the worst case from the configured gracePeriod values. It does not predict a stop time.\n")
	fmt.Fprintf(&b, "Budget %s, safety margin %s. The groups start %s after the start of the shutdown.\n",
		ptr.Deref(spec.Budget, metav1.Duration{Duration: defaultBudget}).Duration,
		ptr.Deref(spec.SafetyMargin, metav1.Duration{Duration: defaultSafetyMargin}).Duration,
		groupsStart)
	fmt.Fprintf(&b, "Result: %s\n", status.Result)

	for i, group := range status.Groups {
		fmt.Fprintf(&b, "\nGroup %d: longest chain %s, available %s\n", i+1, group.LongestChain.Duration, group.Available.Duration)
		for _, tier := range group.Tiers {
			fmt.Fprintf(&b, "  Tier %d\n", tier.Tier)
			for _, app := range tier.Applications {
				key := appKey(app.Namespace, app.Name)
				fmt.Fprintf(&b, "    %s: gracePeriod %s", key, app.GracePeriod.Duration)
				if app.ShrunkGracePeriod != app.GracePeriod {
					fmt.Fprintf(&b, ", shrunk to %s", app.ShrunkGracePeriod.Duration)
				}
				fmt.Fprintf(&b, ", deadline %s after the start\n", app.Deadline.Duration)
				for _, w := range app.Workloads {
					fmt.Fprintf(&b, "      %s %s: %s\n", w.Kind, w.Name, w.Action)
				}
				for _, f := range status.Findings {
					if slices.Contains(f.Applications, key) {
						fmt.Fprintf(&b, "      %s %s: %s Fallback: %s\n", f.Severity, f.Reason, f.Message, f.Fallback)
					}
				}
			}
		}
	}
	return b.String()
}
