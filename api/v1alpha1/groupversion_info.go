// Package v1alpha1 contains the API types of the shutdown.giantswarm.io v1alpha1 group.
// +kubebuilder:object:generate=true
// +groupName=shutdown.giantswarm.io
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	// GroupVersion is the group version of the API types.
	GroupVersion = schema.GroupVersion{Group: "shutdown.giantswarm.io", Version: "v1alpha1"}

	// SchemeBuilder adds the API types to a scheme.
	SchemeBuilder = runtime.NewSchemeBuilder(func(scheme *runtime.Scheme) error {
		metav1.AddToGroupVersion(scheme, GroupVersion)
		return nil
	})

	// AddToScheme adds the API types to a scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)
