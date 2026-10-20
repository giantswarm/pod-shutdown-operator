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
// It ignores a dependsOn name that is not in the record.
func (r *ShutdownRequestReconciler) dependencies(ctx context.Context, recorded []shutdownv1alpha1.RecordedApplication) (map[string][]string, error) {
	var apps shutdownv1alpha1.ManagedApplicationList
	if err := r.Client.List(ctx, &apps); err != nil {
		return nil, fmt.Errorf("list ManagedApplications: %w", err)
	}

	inRecord := map[string]bool{}
	for _, app := range recorded {
		inRecord[appKey(app.Namespace, app.Name)] = true
	}

	deps := map[string][]string{}
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
			}
		}
	}
	return deps, nil
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

// cyclesAsSteps returns the dependencies with each dependency cycle in one parallel step.
// An application in a cycle depends on all dependencies of the cycle, and an application that depends
// on one application of a cycle depends on all applications of the cycle. The result has no cycle.
func cyclesAsSteps(deps map[string][]string) map[string][]string {
	component := components(deps)
	members := map[int][]string{}
	for key, c := range component {
		members[c] = append(members[c], key)
	}

	steps := map[string][]string{}
	for app, appDeps := range deps {
		for _, dep := range appDeps {
			if component[dep] == component[app] {
				continue
			}
			for _, member := range members[component[app]] {
				steps[member] = append(steps[member], members[component[dep]]...)
			}
		}
	}
	for app := range steps {
		slices.Sort(steps[app])
		steps[app] = slices.Compact(steps[app])
	}
	return steps
}

// components returns the strongly connected component of each application in the dependency map, as an index.
// The applications of a dependency cycle share one component. It uses the algorithm of Tarjan.
func components(deps map[string][]string) map[string]int {
	index := map[string]int{}
	low := map[string]int{}
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
				visit(dep)
				low[app] = min(low[app], low[dep])
			} else if onStack[dep] {
				low[app] = min(low[app], index[dep])
			}
		}
		if low[app] != index[app] {
			return
		}
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
	for app := range deps {
		if _, seen := index[app]; !seen {
			visit(app)
		}
	}
	return component
}
