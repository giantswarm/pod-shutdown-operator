package controller

import (
	"context"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	shutdownv1alpha1 "github.com/giantswarm/pod-shutdown-operator/api/v1alpha1"
)

// The defaults are equal to the CRD defaults. They apply to an object that has no value,
// for example a ManagedApplication that no longer exists.
const (
	defaultGracePeriod  = 30 * time.Second
	defaultBudget       = 10 * time.Minute
	defaultSafetyMargin = 30 * time.Second
)

// stopTimes is the time plan of one application at a shutdown.
type stopTimes struct {
	// deadline is the latest time of the forced stop.
	deadline time.Time

	// gracePeriod is the shrunk gracePeriod: the time from the start of the application to its forced stop.
	gracePeriod time.Duration
}

// stopPlan is the time plan of a shutdown.
type stopPlan struct {
	// times holds the stop times of each application by key.
	times map[string]stopTimes

	// group holds the dependency group of each application by key, as an index.
	group map[string]int

	// longestChain holds the gracePeriod sum of the longest chain of each group, before the shrink.
	longestChain map[int]time.Duration

	// available is the time of each group: from the start of the groups to the end.
	available time.Duration
}

// shutdownWindow returns the time limit of the Flux step and the end of the budget minus the safety margin.
func shutdownWindow(startedAt time.Time, spec shutdownv1alpha1.ShutdownRequestSpec, fluxStopTimeout time.Duration) (fluxLimit, end time.Time) {
	// No validation stops a negative safety margin, which would move the end past the budget.
	end = startedAt.Add(ptr.Deref(spec.Budget, metav1.Duration{Duration: defaultBudget}).Duration -
		max(ptr.Deref(spec.SafetyMargin, metav1.Duration{Duration: defaultSafetyMargin}).Duration, 0))
	// The Flux step must not use time past the end of the budget.
	fluxLimit = startedAt.Add(fluxStopTimeout)
	if fluxLimit.After(end) {
		fluxLimit = end
	}
	return fluxLimit, end
}

// planStopTimes returns the time plan of a shutdown. It is planning code and changes nothing.
func planStopTimes(keys []string, deps map[string][]string, gracePeriods map[string]time.Duration, groupsStart, end time.Time) stopPlan {
	// The applications of a dependency cycle are one parallel step, so the chain counts the cycle one time.
	steps := cyclesAsSteps(deps)

	// below holds the gracePeriod sum of the longest chain below each application, before the shrink.
	below := map[string]time.Duration{}
	var chainBelow func(string) time.Duration
	chainBelow = func(key string) time.Duration {
		if sum, found := below[key]; found {
			return sum
		}
		// The longest chain below an application goes through the dependency with the largest sum.
		var longest time.Duration
		for _, dep := range steps[key] {
			longest = max(longest, gracePeriods[dep]+chainBelow(dep))
		}
		below[key] = longest
		return longest
	}

	// The longest chain of a group starts at one of its applications and goes down.
	group := groups(keys, deps)
	longestChain := map[int]time.Duration{}
	for _, key := range keys {
		longestChain[group[key]] = max(longestChain[group[key]], gracePeriods[key]+chainBelow(key))
	}

	// Each group gets the time from groupsStart to end.
	available := max(end.Sub(groupsStart), 0)
	times := map[string]stopTimes{}
	for _, key := range keys {
		// When the longest chain does not fit, every gracePeriod in the group shrinks by the same ratio.
		ratio := 1.0
		if longest := longestChain[group[key]]; longest > available {
			ratio = float64(available) / float64(longest)
		}
		times[key] = stopTimes{
			// The deadline leaves time for the shrunk chain below the application.
			deadline:    end.Add(-shrink(below[key], ratio)),
			gracePeriod: shrink(gracePeriods[key], ratio),
		}
	}
	return stopPlan{times: times, group: group, longestChain: longestChain, available: available}
}

func shrink(d time.Duration, ratio float64) time.Duration {
	return time.Duration(float64(d) * ratio)
}

// groups returns the dependency group of each application by key, as an index.
func groups(keys []string, deps map[string][]string) map[string]int {
	// A dependency group is a set of applications that dependsOn links together, in either direction.
	links := map[string][]string{}
	for app, appDeps := range deps {
		for _, dep := range appDeps {
			links[app] = append(links[app], dep)
			links[dep] = append(links[dep], app)
		}
	}

	// mark gives the group to each application that the key links to.
	group := map[string]int{}
	var mark func(key string, g int)
	mark = func(key string, g int) {
		if _, found := group[key]; found {
			return
		}
		group[key] = g
		for _, linked := range links[key] {
			mark(linked, g)
		}
	}
	// An application with no group yet starts a new group.
	for g, key := range keys {
		mark(key, g)
	}
	return group
}

// gracePeriods returns the gracePeriod of each recorded application by key.
// It reads the ManagedApplications at each call.
func (p planner) gracePeriods(ctx context.Context, recorded []shutdownv1alpha1.RecordedApplication) (map[string]time.Duration, error) {
	var apps shutdownv1alpha1.ManagedApplicationList
	if err := p.reader.List(ctx, &apps); err != nil {
		return nil, fmt.Errorf("list ManagedApplications: %w", err)
	}

	gracePeriods := map[string]time.Duration{}
	for _, app := range recorded {
		gracePeriods[appKey(app.Namespace, app.Name)] = defaultGracePeriod
	}
	for _, app := range apps.Items {
		key := appKey(app.Namespace, app.Name)
		if _, inRecord := gracePeriods[key]; inRecord && app.Spec.GracePeriod != nil {
			// No validation stops a negative value.
			gracePeriods[key] = max(app.Spec.GracePeriod.Duration, 0)
		}
	}
	return gracePeriods, nil
}
