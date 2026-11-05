package controller

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	shutdownv1alpha1 "github.com/giantswarm/pod-shutdown-operator/api/v1alpha1"
)

// clusterFindings returns the findings that come from the live cluster and not from the dependency graph.
func (p planner) clusterFindings(ctx context.Context, record *shutdownv1alpha1.RecordedState,
	gracePeriods map[string]time.Duration) ([]shutdownv1alpha1.Finding, error) {
	findings, err := p.patchFindings(ctx, record)
	if err != nil {
		return nil, err
	}
	terminations, err := p.terminationFindings(ctx, record.Applications, gracePeriods)
	if err != nil {
		return nil, err
	}
	findings = append(findings, terminations...)
	unmanaged, err := p.unmanaged(ctx)
	if err != nil {
		return nil, err
	}
	findings = append(findings, unmanagedFindings(unmanaged)...)
	return append(findings, p.bootIDFindings()...), nil
}

// patchFindings sends each patch of a real shutdown as a server-side dry run.
// It returns an Error for each patch that the API server rejects or fails with a server error.
func (p planner) patchFindings(ctx context.Context, record *shutdownv1alpha1.RecordedState) ([]shutdownv1alpha1.Finding, error) {
	var findings []shutdownv1alpha1.Finding
	// check sends the patch that stops one workload. what names the patch in the message of the finding.
	// failedFallback is the fallback after a server error.
	check := func(namespace string, w shutdownv1alpha1.RecordedWorkload, what string, finding shutdownv1alpha1.Finding,
		failedFallback string) error {
		obj, patch, err := p.stopPatch(ctx, namespace, w)
		if err != nil || patch == nil {
			return err
		}
		// A real shutdown also sends a patch again after an error.
		err = retryOnce(func() error { return p.dryRun.Patch(ctx, obj, patch) })
		// A real shutdown counts a workload that no longer exists as stopped.
		if err == nil || isAbsent(err) {
			return nil
		}
		finding.Severity = shutdownv1alpha1.SeverityError
		switch {
		case isRejected(err):
			finding.Reason = shutdownv1alpha1.ReasonPatchRejected
			finding.Message = fmt.Sprintf("The API server rejected the dry run of %s: %v.", what, err)
		case isServerError(err):
			// For example, an admission webhook that fails. It can fail at each later plan too.
			finding.Reason = shutdownv1alpha1.ReasonPatchFailed
			finding.Message = fmt.Sprintf("The dry run of %s failed with a server error: %v.", what, err)
			finding.Fallback = failedFallback
		default:
			// A later plan can avoid the other errors: no response, a timeout, a conflict, too many requests.
			return fmt.Errorf("send the dry run of %s: %w", what, err)
		}
		findings = append(findings, finding)
		return nil
	}

	for _, c := range record.FluxControllers {
		fallback := "Try again at each poll up to the time limit of the Flux step. Then stop the applications while Flux runs."
		err := check(c.Namespace, shutdownv1alpha1.RecordedWorkload{Kind: shutdownv1alpha1.WorkloadKindDeployment, Name: c.Name},
			"the scale to 0 of the Flux controller "+appKey(c.Namespace, c.Name),
			shutdownv1alpha1.Finding{Fallback: fallback}, fallback)
		if err != nil {
			return nil, err
		}
	}
	for _, app := range record.Applications {
		for _, w := range app.Workloads {
			err := check(app.Namespace, w, fmt.Sprintf("the %s patch of %s %s", actionOf(w.Kind), w.Kind, w.Name),
				shutdownv1alpha1.Finding{
					Applications: []string{appKey(app.Namespace, app.Name)},
					Fallback:     "Add the application to status.failed. Then continue with its dependencies.",
				},
				"Try again at each poll up to the forced stop. Then add the application to status.failed, and continue with its dependencies.")
			if err != nil {
				return nil, err
			}
		}
	}
	return findings, nil
}

// isServerError returns true when the API server answered with a server error (5xx).
func isServerError(err error) bool {
	var status apierrors.APIStatus
	return errors.As(err, &status) && status.Status().Code >= http.StatusInternalServerError
}

// stopPatch returns the object and the patch that a real shutdown sends to stop a workload. As a real shutdown,
// it reads the live object. It returns a nil patch when a real shutdown sends no patch: the workload does not
// exist, or it is already stopped.
func (p planner) stopPatch(ctx context.Context, namespace string, w shutdownv1alpha1.RecordedWorkload) (client.Object, client.Patch, error) {
	switch w.Kind {
	case shutdownv1alpha1.WorkloadKindVirtualMachine:
		return p.vmStopPatch(ctx, namespace, w.Name)
	case shutdownv1alpha1.WorkloadKindCronJob:
		// A real shutdown suspends a CronJob also when it is already suspended.
		return &batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: w.Name}}, suspendPatch(true), nil
	}
	obj, ok := newScalable(w.Kind)
	if !ok {
		return nil, nil, nil
	}
	err := p.reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: w.Name}, obj)
	if isAbsent(err) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("get %s %s/%s: %w", w.Kind, namespace, w.Name, err)
	}
	if replicas, _ := replicasAndSelector(obj); ptr.Deref(replicas, 1) == 0 {
		return nil, nil, nil
	}
	return obj, scaleToZero, nil
}

// vmStopPatch returns the VM and the patch that a real shutdown sends to halt it, with the rules of stopVM.
func (p planner) vmStopPatch(ctx context.Context, namespace, name string) (client.Object, client.Patch, error) {
	kind := shutdownv1alpha1.WorkloadKindVirtualMachine
	vm := newKubeVirtObject(string(kind), namespace, name)
	err := p.reader.Get(ctx, client.ObjectKeyFromObject(vm), vm)
	if isAbsent(err) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("get %s %s/%s: %w", kind, namespace, name, err)
	}
	strategy, halted, usesRunning := haltState(vm)
	if halted {
		return nil, nil, nil
	}
	// A real shutdown keeps the field of a VM that does not run, when the run strategy does not tell this.
	if !usesRunning && !strategyTellsState(strategy) {
		vmi, err := p.runningVMI(ctx, namespace, name)
		if err != nil || vmi == nil || stoppedByUser(strategy, vmi) {
			return nil, nil, err
		}
	}
	return vm, haltPatch(usesRunning), nil
}

// terminationFindings returns a Warning for each workload whose terminationGracePeriodSeconds is longer
// or shorter than the gracePeriod of its application.
func (p planner) terminationFindings(ctx context.Context, apps []shutdownv1alpha1.RecordedApplication,
	gracePeriods map[string]time.Duration) ([]shutdownv1alpha1.Finding, error) {
	var findings []shutdownv1alpha1.Finding
	for _, app := range apps {
		key := appKey(app.Namespace, app.Name)
		gracePeriod := gracePeriods[key]
		for _, w := range app.Workloads {
			// A real shutdown stops no pod of a workload that did not run at the start.
			if !recordedRunning(w) {
				continue
			}
			termination, found, err := p.terminationGracePeriod(ctx, app.Namespace, w)
			if err != nil {
				return nil, err
			}
			if !found || termination == gracePeriod {
				continue
			}
			finding := shutdownv1alpha1.Finding{
				Severity:     shutdownv1alpha1.SeverityWarning,
				Reason:       shutdownv1alpha1.ReasonTerminationGracePeriodLonger,
				Applications: []string{key},
				Message: fmt.Sprintf("The terminationGracePeriodSeconds of %s %s is %s, which is longer than the gracePeriod %s.",
					w.Kind, w.Name, termination, gracePeriod),
				Fallback: "Force the stop at the end of the gracePeriod or at the deadline, with a grace period of 0.",
			}
			if termination < gracePeriod {
				finding.Reason = shutdownv1alpha1.ReasonTerminationGracePeriodShorter
				finding.Message = fmt.Sprintf("The terminationGracePeriodSeconds of %s %s is %s, which is shorter than the gracePeriod %s.",
					w.Kind, w.Name, termination, gracePeriod)
				finding.Fallback = "None. The workload gets only its terminationGracePeriodSeconds to stop."
			}
			findings = append(findings, finding)
		}
	}
	return findings, nil
}

// terminationGracePeriod returns the terminationGracePeriodSeconds of the pod template of a Deployment or a
// StatefulSet, or of a VM. It returns false for a CronJob and for a workload that does not exist.
func (p planner) terminationGracePeriod(ctx context.Context, namespace string, w shutdownv1alpha1.RecordedWorkload) (time.Duration, bool, error) {
	// The API server sets this default in a pod template. KubeVirt sets no default, but uses the same value.
	seconds := int64(corev1.DefaultTerminationGracePeriodSeconds)
	switch w.Kind {
	case shutdownv1alpha1.WorkloadKindCronJob:
		// A suspend does not stop the pods of a CronJob.
		return 0, false, nil
	case shutdownv1alpha1.WorkloadKindVirtualMachine:
		vm := newKubeVirtObject(string(w.Kind), namespace, w.Name)
		err := p.reader.Get(ctx, client.ObjectKeyFromObject(vm), vm)
		if isAbsent(err) {
			return 0, false, nil
		}
		if err != nil {
			return 0, false, fmt.Errorf("get %s %s/%s: %w", w.Kind, namespace, w.Name, err)
		}
		// The check uses the VM value. The virt-launcher pod value is 30 seconds longer.
		if value, found, _ := unstructured.NestedInt64(vm.Object, "spec", "template", "spec", "terminationGracePeriodSeconds"); found {
			seconds = value
		}
	default:
		obj, ok := newScalable(w.Kind)
		if !ok {
			return 0, false, nil
		}
		err := p.reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: w.Name}, obj)
		if isAbsent(err) {
			return 0, false, nil
		}
		if err != nil {
			return 0, false, fmt.Errorf("get %s %s/%s: %w", w.Kind, namespace, w.Name, err)
		}
		seconds = ptr.Deref(podTemplate(obj).Spec.TerminationGracePeriodSeconds, seconds)
	}
	return time.Duration(seconds) * time.Second, true, nil
}

// recordedRunning returns true when the record tells that a workload runs: a Deployment or a StatefulSet with
// replicas above 0, or a VM that must run. A CronJob counts as running.
func recordedRunning(w shutdownv1alpha1.RecordedWorkload) bool {
	switch w.Kind {
	case shutdownv1alpha1.WorkloadKindVirtualMachine:
		return mustRun(w)
	case shutdownv1alpha1.WorkloadKindDeployment, shutdownv1alpha1.WorkloadKindStatefulSet:
		return ptr.Deref(w.Replicas, 1) != 0
	}
	return true
}

// unmanaged returns the Deployments and StatefulSets with replicas above 0, and the VMs that run, that no
// ManagedApplication lists. It skips the Flux controllers, because a real shutdown scales them,
// and the Deployment of the operator, because it must run.
// The field that a VM uses tells if it runs. Only when the run strategy does not tell this, the VMI tells it.
func (p planner) unmanaged(ctx context.Context) ([]shutdownv1alpha1.UnmanagedWorkload, error) {
	var apps shutdownv1alpha1.ManagedApplicationList
	if err := p.reader.List(ctx, &apps); err != nil {
		return nil, fmt.Errorf("list ManagedApplications: %w", err)
	}
	listed := map[shutdownv1alpha1.UnmanagedWorkload]bool{}
	for _, app := range apps.Items {
		for _, ref := range app.Spec.Workloads {
			listed[shutdownv1alpha1.UnmanagedWorkload{Namespace: app.Namespace, Kind: ref.Kind, Name: ref.Name}] = true
		}
	}
	for _, c := range p.fluxControllers {
		listed[shutdownv1alpha1.UnmanagedWorkload{Namespace: c.Namespace, Kind: shutdownv1alpha1.WorkloadKindDeployment, Name: c.Name}] = true
	}
	operator := p.operatorDeployment
	listed[shutdownv1alpha1.UnmanagedWorkload{Namespace: operator.Namespace, Kind: shutdownv1alpha1.WorkloadKindDeployment, Name: operator.Name}] = true

	var unmanaged []shutdownv1alpha1.UnmanagedWorkload
	add := func(namespace string, kind shutdownv1alpha1.WorkloadKind, name string, runs bool) {
		w := shutdownv1alpha1.UnmanagedWorkload{Namespace: namespace, Kind: kind, Name: name}
		if runs && !listed[w] {
			unmanaged = append(unmanaged, w)
		}
	}

	var deployments appsv1.DeploymentList
	if err := p.reader.List(ctx, &deployments); err != nil {
		return nil, fmt.Errorf("list Deployments: %w", err)
	}
	for _, d := range deployments.Items {
		add(d.Namespace, shutdownv1alpha1.WorkloadKindDeployment, d.Name, ptr.Deref(d.Spec.Replicas, 1) > 0)
	}
	var statefulSets appsv1.StatefulSetList
	if err := p.reader.List(ctx, &statefulSets); err != nil {
		return nil, fmt.Errorf("list StatefulSets: %w", err)
	}
	for _, s := range statefulSets.Items {
		add(s.Namespace, shutdownv1alpha1.WorkloadKindStatefulSet, s.Name, ptr.Deref(s.Spec.Replicas, 1) > 0)
	}

	vms := &unstructured.UnstructuredList{}
	vms.SetGroupVersionKind(schema.GroupVersionKind{Group: "kubevirt.io", Version: "v1", Kind: "VirtualMachineList"})
	// A cluster with no KubeVirt API has no VM.
	if err := p.reader.List(ctx, vms); err != nil && !meta.IsNoMatchError(err) {
		return nil, fmt.Errorf("list VirtualMachines: %w", err)
	}
	for _, vm := range vms.Items {
		kind := shutdownv1alpha1.WorkloadKindVirtualMachine
		// Only an unlisted VM needs the read of its VMI.
		if listed[shutdownv1alpha1.UnmanagedWorkload{Namespace: vm.GetNamespace(), Kind: kind, Name: vm.GetName()}] {
			continue
		}
		strategy, halted, usesRunning := haltState(&vm)
		runs := !halted
		// The read of the VMI is slow, because the client does not cache it. Thus only these VMs get it.
		if !usesRunning && !strategyTellsState(strategy) {
			vmi, err := p.runningVMI(ctx, vm.GetNamespace(), vm.GetName())
			if err != nil {
				return nil, err
			}
			// A VMI in deletion stops.
			runs = vmi != nil && vmi.GetDeletionTimestamp() == nil
		}
		add(vm.GetNamespace(), kind, vm.GetName(), runs)
	}

	slices.SortFunc(unmanaged, func(a, b shutdownv1alpha1.UnmanagedWorkload) int {
		return cmp.Or(cmp.Compare(a.Namespace, b.Namespace), cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.Name, b.Name))
	})
	return unmanaged, nil
}

// unmanagedFindings returns a Warning for each unmanaged workload.
func unmanagedFindings(unmanaged []shutdownv1alpha1.UnmanagedWorkload) []shutdownv1alpha1.Finding {
	var findings []shutdownv1alpha1.Finding
	for _, w := range unmanaged {
		findings = append(findings, shutdownv1alpha1.Finding{
			Severity: shutdownv1alpha1.SeverityWarning,
			Reason:   shutdownv1alpha1.ReasonUnmanagedWorkload,
			Message:  fmt.Sprintf("%s %s runs, but no ManagedApplication lists it.", w.Kind, appKey(w.Namespace, w.Name)),
			Fallback: "Do not stop the workload. It runs until the node power-off.",
		})
	}
	return findings
}

// bootIDFindings returns an Error when the operator cannot read its boot ID.
func (p planner) bootIDFindings() []shutdownv1alpha1.Finding {
	_, err := readBootID(p.bootIDFile)
	if err == nil {
		return nil
	}
	return []shutdownv1alpha1.Finding{{
		Severity: shutdownv1alpha1.SeverityError,
		Reason:   shutdownv1alpha1.ReasonUnreadableBootID,
		Message:  fmt.Sprintf("The operator cannot get its boot ID: %v.", err),
		Fallback: "Count each operator restart during a shutdown as a power cycle, and scale up from the record.",
	}}
}
