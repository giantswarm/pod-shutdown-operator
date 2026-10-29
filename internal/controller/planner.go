package controller

import (
	"context"
	"fmt"
	"slices"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	shutdownv1alpha1 "github.com/giantswarm/pod-shutdown-operator/api/v1alpha1"
)

// maxDepth is the expected maximum depth of a dependency group, in tiers.
const maxDepth = 3

// planner reads the cluster and plans a shutdown. It holds only a reader, so it cannot change an object.
// A real shutdown and a ShutdownPlan use the same planner.
type planner struct {
	reader client.Reader

	// fluxControllers are the Flux controller Deployments that a shutdown scales to 0 first.
	fluxControllers []types.NamespacedName
}

// planner returns the planner of the reconciler.
func (r *ShutdownRequestReconciler) planner() planner {
	return planner{reader: r.Client, fluxControllers: r.FluxControllers}
}

// plan returns the plan of a shutdown with its findings, as a ShutdownPlan status. It changes nothing.
// While a ShutdownRequest is active, it plans from the record and the budget of that request.
// Else it plans from the live state and the default budget.
func (p planner) plan(ctx context.Context, fluxStopTimeout time.Duration, now time.Time) (shutdownv1alpha1.ShutdownPlanStatus, error) {
	request, err := p.activeRequest(ctx)
	if err != nil {
		return shutdownv1alpha1.ShutdownPlanStatus{}, err
	}
	var spec shutdownv1alpha1.ShutdownRequestSpec
	var record *shutdownv1alpha1.RecordedState
	source := "the live state"
	if request != nil {
		spec = request.Spec
		record = request.Status.RecordedState
		source = "the record of the active ShutdownRequest"
	} else if record, err = p.recordState(ctx); err != nil {
		return shutdownv1alpha1.ShutdownPlanStatus{}, err
	}

	apps := record.Applications
	deps, unknown, err := p.dependencies(ctx, apps)
	if err != nil {
		return shutdownv1alpha1.ShutdownPlanStatus{}, err
	}
	gracePeriods, err := p.gracePeriods(ctx, apps)
	if err != nil {
		return shutdownv1alpha1.ShutdownPlanStatus{}, err
	}
	keys := make([]string, 0, len(apps))
	for _, app := range apps {
		keys = append(keys, appKey(app.Namespace, app.Name))
	}

	// The dry run has no start time, so it plans from the zero time. A deadline is then the time after the start.
	var start time.Time
	fluxLimit, end := shutdownWindow(start, spec, fluxStopTimeout)
	// The groups start at the Flux limit, as in a real shutdown.
	groupsStart := start
	if len(record.FluxControllers) > 0 {
		groupsStart = fluxLimit
	}
	stops := planStopTimes(keys, deps, gracePeriods, groupsStart, end)
	groups := plannedGroups(apps, gracePeriods, stops, tiers(keys, deps), start)

	findings := cycleFindings(keys, deps)
	findings = append(findings, groupFindings(groups)...)
	findings = append(findings, unknownFindings(keys, unknown)...)
	missing, err := p.missingFindings(ctx, apps)
	if err != nil {
		return shutdownv1alpha1.ShutdownPlanStatus{}, err
	}
	findings = append(findings, missing...)

	status := shutdownv1alpha1.ShutdownPlanStatus{
		ComputedAt: &metav1.Time{Time: now},
		Result:     result(findings),
		Groups:     groups,
		Findings:   findings,
	}
	status.Report = report(status, source, spec, groupsStart.Sub(start))
	return status, nil
}

// activeRequest returns the ShutdownRequest when it has a record that a scale-up has not yet restored. Else it returns nil.
func (p planner) activeRequest(ctx context.Context) (*shutdownv1alpha1.ShutdownRequest, error) {
	var request shutdownv1alpha1.ShutdownRequest
	err := p.reader.Get(ctx, client.ObjectKey{Name: shutdownv1alpha1.ShutdownRequestName}, &request)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get the ShutdownRequest: %w", err)
	}
	if request.Status.RecordedState == nil || request.Status.Phase == shutdownv1alpha1.PhaseRestored {
		return nil, nil
	}
	return &request, nil
}

// plannedGroups returns the dependency groups with their tiers. The groups and the applications keep the order of the record.
func plannedGroups(apps []shutdownv1alpha1.RecordedApplication, gracePeriods map[string]time.Duration, stops stopPlan,
	tier map[string]int, start time.Time) []shutdownv1alpha1.PlannedGroup {
	var groups []shutdownv1alpha1.PlannedGroup
	// position holds the position in groups of each group index.
	position := map[int]int{}
	for _, app := range apps {
		key := appKey(app.Namespace, app.Name)
		g := stops.group[key]
		pos, found := position[g]
		if !found {
			pos = len(groups)
			position[g] = pos
			groups = append(groups, shutdownv1alpha1.PlannedGroup{
				LongestChain: metav1.Duration{Duration: stops.longestChain[g]},
				Available:    metav1.Duration{Duration: stops.available},
			})
		}
		// Each tier above 1 has a dependent in the tier before it, so no tier stays empty.
		for len(groups[pos].Tiers) < tier[key] {
			groups[pos].Tiers = append(groups[pos].Tiers, shutdownv1alpha1.PlannedTier{Tier: int32(len(groups[pos].Tiers) + 1)}) //nolint:gosec // The depth is small.
		}
		planned := shutdownv1alpha1.PlannedApplication{
			Namespace:         app.Namespace,
			Name:              app.Name,
			GracePeriod:       metav1.Duration{Duration: gracePeriods[key]},
			ShrunkGracePeriod: metav1.Duration{Duration: stops.times[key].gracePeriod},
			Deadline:          metav1.Duration{Duration: stops.times[key].deadline.Sub(start)},
		}
		for _, w := range app.Workloads {
			planned.Workloads = append(planned.Workloads, shutdownv1alpha1.PlannedWorkload{Kind: w.Kind, Name: w.Name, Action: actionOf(w.Kind)})
		}
		t := &groups[pos].Tiers[tier[key]-1]
		t.Applications = append(t.Applications, planned)
	}
	return groups
}

// actionOf returns the action of a real shutdown on a workload kind.
func actionOf(kind shutdownv1alpha1.WorkloadKind) string {
	switch kind {
	case shutdownv1alpha1.WorkloadKindVirtualMachine:
		return "Halt"
	case shutdownv1alpha1.WorkloadKindCronJob:
		return "Suspend"
	}
	return "ScaleToZero"
}

// applicationsOf returns the keys of the applications of a group.
func applicationsOf(group shutdownv1alpha1.PlannedGroup) []string {
	var keys []string
	for _, tier := range group.Tiers {
		for _, app := range tier.Applications {
			keys = append(keys, appKey(app.Namespace, app.Name))
		}
	}
	return keys
}

// cycleFindings returns an Error for each dependency cycle.
func cycleFindings(keys []string, deps map[string][]string) []shutdownv1alpha1.Finding {
	component := components(deps)
	// The keys are sorted, so the members of each cycle are sorted.
	members := map[int][]string{}
	for _, key := range keys {
		if c, found := component[key]; found {
			members[c] = append(members[c], key)
		}
	}

	var findings []shutdownv1alpha1.Finding
	reported := map[int]bool{}
	for _, key := range keys {
		c, found := component[key]
		if !found || reported[c] {
			continue
		}
		// An application that depends on itself is a cycle of one.
		if len(members[c]) > 1 || slices.Contains(deps[key], key) {
			reported[c] = true
			findings = append(findings, shutdownv1alpha1.Finding{
				Severity:     shutdownv1alpha1.SeverityError,
				Reason:       shutdownv1alpha1.ReasonCycle,
				Applications: members[c],
				Message:      "The applications depend on each other in a cycle.",
				Fallback:     "Stop the applications of the cycle in one parallel step, after their dependents and before their dependencies.",
			})
		}
	}
	return findings
}

// groupFindings returns an Error for each group with a depth above maxDepth, and a Warning for each group that shrinks.
func groupFindings(groups []shutdownv1alpha1.PlannedGroup) []shutdownv1alpha1.Finding {
	var findings []shutdownv1alpha1.Finding
	for _, group := range groups {
		if depth := len(group.Tiers); depth > maxDepth {
			findings = append(findings, shutdownv1alpha1.Finding{
				Severity:     shutdownv1alpha1.SeverityError,
				Reason:       shutdownv1alpha1.ReasonDepthAboveLimit,
				Applications: applicationsOf(group),
				Message:      fmt.Sprintf("The group has a depth of %d tiers, which is above the limit of %d.", depth, maxDepth),
				Fallback:     "Use the real depth: stop all tiers in order. Do not refuse the shutdown.",
			})
		}
		if longest, available := group.LongestChain.Duration, group.Available.Duration; longest > available {
			findings = append(findings, shutdownv1alpha1.Finding{
				Severity:     shutdownv1alpha1.SeverityWarning,
				Reason:       shutdownv1alpha1.ReasonGracePeriodShrink,
				Applications: applicationsOf(group),
				Message:      fmt.Sprintf("The longest chain of the group needs %s, but only %s is available.", longest, available),
				Fallback: fmt.Sprintf("Shrink each gracePeriod in the group by the ratio %.2f, so that the longest chain fits.",
					float64(available)/float64(longest)),
			})
		}
	}
	return findings
}

// unknownFindings returns an Error for each dependsOn name that is not in the plan.
func unknownFindings(keys []string, unknown map[string][]string) []shutdownv1alpha1.Finding {
	var findings []shutdownv1alpha1.Finding
	for _, key := range keys {
		for _, dep := range unknown[key] {
			findings = append(findings, shutdownv1alpha1.Finding{
				Severity:     shutdownv1alpha1.SeverityError,
				Reason:       shutdownv1alpha1.ReasonUnknownDependency,
				Applications: []string{key},
				Message:      fmt.Sprintf("dependsOn names %s, which is not an application in the plan.", dep),
				Fallback:     "Ignore the edge.",
			})
		}
	}
	return findings
}

// missingFindings returns an Error for each listed workload that is not in the record and does not exist.
func (p planner) missingFindings(ctx context.Context, recorded []shutdownv1alpha1.RecordedApplication) ([]shutdownv1alpha1.Finding, error) {
	var apps shutdownv1alpha1.ManagedApplicationList
	if err := p.reader.List(ctx, &apps); err != nil {
		return nil, fmt.Errorf("list ManagedApplications: %w", err)
	}
	specs := map[string]shutdownv1alpha1.ManagedApplicationSpec{}
	for _, app := range apps.Items {
		specs[appKey(app.Namespace, app.Name)] = app.Spec
	}

	var findings []shutdownv1alpha1.Finding
	for _, app := range recorded {
		key := appKey(app.Namespace, app.Name)
		for _, ref := range specs[key].Workloads {
			if slices.ContainsFunc(app.Workloads, func(w shutdownv1alpha1.RecordedWorkload) bool {
				return w.Kind == ref.Kind && w.Name == ref.Name
			}) {
				continue
			}
			// A workload that a team adds during a shutdown is not in the record, but it exists.
			_, exists, err := p.recordWorkload(ctx, app.Namespace, ref)
			if err != nil {
				return nil, err
			}
			if exists {
				continue
			}
			findings = append(findings, shutdownv1alpha1.Finding{
				Severity:     shutdownv1alpha1.SeverityError,
				Reason:       shutdownv1alpha1.ReasonMissingWorkload,
				Applications: []string{key},
				Message:      fmt.Sprintf("The listed workload %s %s does not exist.", ref.Kind, ref.Name),
				Fallback:     "Skip the workload.",
			})
		}
	}
	return findings, nil
}

// result returns the worst severity of the findings.
func result(findings []shutdownv1alpha1.Finding) shutdownv1alpha1.PlanResult {
	res := shutdownv1alpha1.PlanResultOK
	for _, f := range findings {
		if f.Severity == shutdownv1alpha1.SeverityError {
			return shutdownv1alpha1.PlanResultErrors
		}
		res = shutdownv1alpha1.PlanResultWarnings
	}
	return res
}
