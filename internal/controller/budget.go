package controller

import (
	"context"
	"fmt"
	"time"

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

// planStopTimes returns the stop times of each application by key. It is planning code and changes nothing.
//
// Each dependency group gets the time from groupsStart to end. When the gracePeriod sum of the longest
// chain in a group is larger, every gracePeriod in the group shrinks by the same ratio, so that the
// longest chain fits. The deadline of an application is end minus the shrunk gracePeriod sum of the
// longest chain below it. The applications of a dependency cycle are one parallel step.
func planStopTimes(keys []string, deps map[string][]string, gracePeriods map[string]time.Duration, groupsStart, end time.Time) map[string]stopTimes {
	steps := cyclesAsSteps(deps)

	// below holds the gracePeriod sum of the longest chain below each application, before the shrink.
	below := map[string]time.Duration{}
	var chainBelow func(string) time.Duration
	chainBelow = func(key string) time.Duration {
		if sum, found := below[key]; found {
			return sum
		}
		var longest time.Duration
		for _, dep := range steps[key] {
			longest = max(longest, gracePeriods[dep]+chainBelow(dep))
		}
		below[key] = longest
		return longest
	}

	group := groups(keys, deps)
	longestChain := map[int]time.Duration{}
	for _, key := range keys {
		longestChain[group[key]] = max(longestChain[group[key]], gracePeriods[key]+chainBelow(key))
	}

	available := max(end.Sub(groupsStart), 0)
	times := map[string]stopTimes{}
	for _, key := range keys {
		ratio := 1.0
		if longest := longestChain[group[key]]; longest > available {
			ratio = float64(available) / float64(longest)
		}
		times[key] = stopTimes{
			deadline:    end.Add(-shrink(below[key], ratio)),
			gracePeriod: shrink(gracePeriods[key], ratio),
		}
	}
	return times
}

func shrink(d time.Duration, ratio float64) time.Duration {
	return time.Duration(float64(d) * ratio)
}

// groups returns the dependency group of each application by key, as an index.
// A dependency group is a set of applications that dependsOn links together.
func groups(keys []string, deps map[string][]string) map[string]int {
	links := map[string][]string{}
	for app, appDeps := range deps {
		for _, dep := range appDeps {
			links[app] = append(links[app], dep)
			links[dep] = append(links[dep], app)
		}
	}

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
	for g, key := range keys {
		mark(key, g)
	}
	return group
}

// gracePeriods returns the gracePeriod of each recorded application by key.
// It reads the ManagedApplications at each call.
func (r *ShutdownRequestReconciler) gracePeriods(ctx context.Context, recorded []shutdownv1alpha1.RecordedApplication) (map[string]time.Duration, error) {
	var apps shutdownv1alpha1.ManagedApplicationList
	if err := r.Client.List(ctx, &apps); err != nil {
		return nil, fmt.Errorf("list ManagedApplications: %w", err)
	}

	gracePeriods := map[string]time.Duration{}
	for _, app := range recorded {
		gracePeriods[appKey(app.Namespace, app.Name)] = defaultGracePeriod
	}
	for _, app := range apps.Items {
		key := appKey(app.Namespace, app.Name)
		if _, inRecord := gracePeriods[key]; inRecord && app.Spec.GracePeriod != nil {
			gracePeriods[key] = app.Spec.GracePeriod.Duration
		}
	}
	return gracePeriods, nil
}
