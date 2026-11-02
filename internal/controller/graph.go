package controller

import (
	"context"
	"fmt"
	"slices"
	"strings"

	shutdownv1alpha1 "github.com/giantswarm/pod-shutdown-operator/api/v1alpha1"
)

// appKey returns the namespace/name key of an application.
func appKey(namespace, name string) string {
	return namespace + "/" + name
}

// dependencies returns the dependencies of each recorded application by key.
// It reads dependsOn from the ManagedApplications at each call.
// It ignores a dependsOn name that is not in the record, and returns these names by key as unknown.
func (p planner) dependencies(ctx context.Context, recorded []shutdownv1alpha1.RecordedApplication) (deps, unknown map[string][]string, err error) {
	var apps shutdownv1alpha1.ManagedApplicationList
	if err := p.reader.List(ctx, &apps); err != nil {
		return nil, nil, fmt.Errorf("list ManagedApplications: %w", err)
	}

	inRecord := map[string]bool{}
	for _, app := range recorded {
		inRecord[appKey(app.Namespace, app.Name)] = true
	}

	deps = map[string][]string{}
	unknown = map[string][]string{}
	for _, app := range apps.Items {
		key := appKey(app.Namespace, app.Name)
		if !inRecord[key] {
			continue
		}
		for _, name := range app.Spec.DependsOn {
			dep := name
			if !strings.Contains(name, "/") {
				dep = appKey(app.Namespace, name)
			}
			if inRecord[dep] {
				deps[key] = append(deps[key], dep)
			} else {
				unknown[key] = append(unknown[key], dep)
			}
		}
	}
	return deps, unknown, nil
}

// dependents returns the reverse of a dependency map: the applications that depend on each application.
func dependents(deps map[string][]string) map[string][]string {
	reverse := map[string][]string{}
	for app, appDeps := range deps {
		for _, dep := range appDeps {
			reverse[dep] = append(reverse[dep], app)
		}
	}
	return reverse
}

// allIn returns true when each key is in the set.
func allIn(keys []string, set map[string]bool) bool {
	for _, key := range keys {
		if !set[key] {
			return false
		}
	}
	return true
}

// tiers returns the tier of each application by key. Tier 1 has no dependents and stops first.
func tiers(keys []string, deps map[string][]string) map[string]int {
	reverse := dependents(cyclesAsSteps(deps))
	tier := map[string]int{}
	var tierOf func(string) int
	tierOf = func(key string) int {
		if t, found := tier[key]; found {
			return t
		}
		// One tier below the highest dependent. The members of a cycle have the same dependents, so they share a tier.
		t := 1
		for _, dependent := range reverse[key] {
			t = max(t, tierOf(dependent)+1)
		}
		tier[key] = t
		return t
	}
	for _, key := range keys {
		tierOf(key)
	}
	return tier
}

// cyclesAsSteps returns the dependencies with each dependency cycle in one parallel step. The result has no cycle.
func cyclesAsSteps(deps map[string][]string) map[string][]string {
	// The applications of a cycle share one component. Each other application has a component of its own.
	component := components(deps)
	members := map[int][]string{}
	for key, c := range component {
		members[c] = append(members[c], key)
	}

	steps := map[string][]string{}
	for app, appDeps := range deps {
		for _, dep := range appDeps {
			// An edge inside a cycle goes away, so that the applications of the cycle stop and start together.
			if component[dep] == component[app] {
				continue
			}
			// Each member of the cycle of app depends on each member of the cycle of dep.
			// Thus a cycle waits as one step, and the applications on the other side wait for the whole cycle.
			for _, member := range members[component[app]] {
				steps[member] = append(steps[member], members[component[dep]]...)
			}
		}
	}
	// Two edges into one cycle add its members two times.
	for app := range steps {
		slices.Sort(steps[app])
		steps[app] = slices.Compact(steps[app])
	}
	return steps
}

// components returns the strongly connected component of each application in the dependency map, as an index.
// It uses the algorithm of Tarjan.
func components(deps map[string][]string) map[string]int {
	// index is the order in which the search finds each application.
	index := map[string]int{}
	// low is the lowest index that an application can reach through the applications on the stack.
	low := map[string]int{}
	// The stack holds the applications that the search found and that have no component yet.
	onStack := map[string]bool{}
	var stack []string
	component := map[string]int{}
	count := 0

	var visit func(string)
	visit = func(app string) {
		index[app] = len(index)
		low[app] = index[app]
		stack = append(stack, app)
		onStack[app] = true
		for _, dep := range deps[app] {
			if _, seen := index[dep]; !seen {
				// A new application: search from it, then take what it can reach.
				visit(dep)
				low[app] = min(low[app], low[dep])
			} else if onStack[dep] {
				// An edge back to an application on the stack closes a cycle.
				low[app] = min(low[app], index[dep])
			}
		}
		// An application that can reach an earlier one on the stack is in the component of that application.
		if low[app] != index[app] {
			return
		}
		// app is the first application of its component. The applications above it on the stack are the other members.
		for {
			member := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			onStack[member] = false
			component[member] = count
			if member == app {
				break
			}
		}
		count++
	}
	// An application that is only a dependency is found from the application that depends on it.
	for app := range deps {
		if _, seen := index[app]; !seen {
			visit(app)
		}
	}
	return component
}
