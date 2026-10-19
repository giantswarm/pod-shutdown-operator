package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// WorkloadKind is the kind of a workload that a ManagedApplication lists.
// +kubebuilder:validation:Enum=Deployment;StatefulSet;VirtualMachine;CronJob
type WorkloadKind string

const (
	WorkloadKindDeployment     WorkloadKind = "Deployment"
	WorkloadKindStatefulSet    WorkloadKind = "StatefulSet"
	WorkloadKindVirtualMachine WorkloadKind = "VirtualMachine"
	WorkloadKindCronJob        WorkloadKind = "CronJob"
)

// WorkloadReference names a workload in the namespace of the ManagedApplication.
type WorkloadReference struct {
	// Kind is the kind of the workload.
	// +required
	Kind WorkloadKind `json:"kind"`

	// Name is the name of the workload.
	// +required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// ManagedApplicationSpec holds the workloads, the dependencies, and the time limits of an application.
type ManagedApplicationSpec struct {
	// Workloads are the workloads of the application. They are in the namespace of the resource.
	// +required
	// +kubebuilder:validation:MinItems=1
	// +listType=atomic
	Workloads []WorkloadReference `json:"workloads"`

	// DependsOn names the applications that this application depends on.
	// A name in another namespace has the form namespace/name.
	// +optional
	// +listType=set
	DependsOn []string `json:"dependsOn,omitempty"`

	// GracePeriod is the time that the application gets to stop.
	// +optional
	// +kubebuilder:default="30s"
	GracePeriod *metav1.Duration `json:"gracePeriod,omitempty"`

	// ReadyTimeout is the time that the application gets to become ready at scale-up.
	// +optional
	// +kubebuilder:default="5m"
	ReadyTimeout *metav1.Duration `json:"readyTimeout,omitempty"`
}

// +kubebuilder:object:root=true

// ManagedApplication declares an application that the operator stops at a shutdown and starts at power-on.
type ManagedApplication struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// +required
	Spec ManagedApplicationSpec `json:"spec"`
}

// +kubebuilder:object:root=true

// ManagedApplicationList contains a list of ManagedApplication.
type ManagedApplicationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []ManagedApplication `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(GroupVersion, &ManagedApplication{}, &ManagedApplicationList{})
		return nil
	})
}
