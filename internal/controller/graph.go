package controller

import (
	"context"
	"fmt"
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
