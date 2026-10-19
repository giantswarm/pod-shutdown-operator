package controller

import (
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	shutdownv1alpha1 "github.com/giantswarm/pod-shutdown-operator/api/v1alpha1"
)

// newScalable returns an empty object for a kind that the operator scales.
// It returns false for VirtualMachines and CronJobs, which a later slice handles.
func newScalable(kind shutdownv1alpha1.WorkloadKind) (client.Object, bool) {
	switch kind {
	case shutdownv1alpha1.WorkloadKindDeployment:
		return &appsv1.Deployment{}, true
	case shutdownv1alpha1.WorkloadKindStatefulSet:
		return &appsv1.StatefulSet{}, true
	}
	return nil, false
}

// replicasAndSelector returns spec.replicas and spec.selector of an object from newScalable.
func replicasAndSelector(obj client.Object) (*int32, *metav1.LabelSelector) {
	switch o := obj.(type) {
	case *appsv1.Deployment:
		return o.Spec.Replicas, o.Spec.Selector
	case *appsv1.StatefulSet:
		return o.Spec.Replicas, o.Spec.Selector
	}
	return nil, nil
}
