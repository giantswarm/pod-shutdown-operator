package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// ShutdownRequestName is the only valid name of a ShutdownRequest.
const ShutdownRequestName = "cluster"

// Action is the intent that the caller writes to a ShutdownRequest.
// +kubebuilder:validation:Enum=Shutdown;Cancel
type Action string

const (
	ActionShutdown Action = "Shutdown"
	ActionCancel   Action = "Cancel"
)

// Phase is the phase of a ShutdownRequest.
// +kubebuilder:validation:Enum=ShuttingDown;Complete;ScalingUp;Restored
type Phase string

const (
	PhaseShuttingDown Phase = "ShuttingDown"
	PhaseComplete     Phase = "Complete"
	PhaseScalingUp    Phase = "ScalingUp"
	PhaseRestored     Phase = "Restored"
)

// ShutdownRequestSpec holds the intent of the caller and the time budget.
type ShutdownRequestSpec struct {
	// Action is the intent of the caller.
	// +required
	Action Action `json:"action"`

	// Budget is the time from the start of the shutdown to the node power-off.
	// +optional
	// +kubebuilder:default="10m"
	Budget *metav1.Duration `json:"budget,omitempty"`

	// SafetyMargin is the time at the end of the budget that the operator keeps free.
	// +optional
	// +kubebuilder:default="30s"
	SafetyMargin *metav1.Duration `json:"safetyMargin,omitempty"`
}

// RecordedWorkload is the state of one workload at the start of a shutdown.
type RecordedWorkload struct {
	// +required
	Kind WorkloadKind `json:"kind"`

	// +required
	Name string `json:"name"`

	// Replicas is the replica count of a Deployment or a StatefulSet.
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`

	// RunStrategy is the run strategy of a VirtualMachine that uses it.
	// +optional
	RunStrategy *string `json:"runStrategy,omitempty"`

	// Running is the running field of a VirtualMachine that uses it.
	// +optional
	Running *bool `json:"running,omitempty"`

	// Suspend is the original suspend value of a CronJob.
	// +optional
	Suspend *bool `json:"suspend,omitempty"`
}

// RecordedApplication is the state of the workloads of one ManagedApplication at the start of a shutdown.
type RecordedApplication struct {
	// +required
	Namespace string `json:"namespace"`

	// +required
	Name string `json:"name"`

	// Workloads are the listed workloads that existed at the start of the shutdown.
	// +optional
	// +listType=atomic
	Workloads []RecordedWorkload `json:"workloads,omitempty"`
}

// RecordedFluxController is the replica count of one Flux controller Deployment at the start of a shutdown.
type RecordedFluxController struct {
	// +required
	Namespace string `json:"namespace"`

	// +required
	Name string `json:"name"`

	// +required
	Replicas int32 `json:"replicas"`
}

// RecordedState is the desired state of the cluster at the start of a shutdown.
type RecordedState struct {
	// +optional
	// +listType=atomic
	Applications []RecordedApplication `json:"applications,omitempty"`

	// +optional
	// +listType=atomic
	FluxControllers []RecordedFluxController `json:"fluxControllers,omitempty"`
}

// UnmanagedWorkload is a workload with replicas above 0 that no ManagedApplication lists.
type UnmanagedWorkload struct {
	// +required
	Namespace string `json:"namespace"`

	// +required
	Kind WorkloadKind `json:"kind"`

	// +required
	Name string `json:"name"`
}

// ShutdownRequestStatus is the state of the shutdown event. Only the leader writes it.
type ShutdownRequestStatus struct {
	// +optional
	Phase Phase `json:"phase,omitempty"`

	// Reason tells why the request has its phase, for example Stale.
	// +optional
	Reason string `json:"reason,omitempty"`

	// RecordedState is the record that the operator writes once, before it changes a workload.
	// +optional
	RecordedState *RecordedState `json:"recordedState,omitempty"`

	// BootIDs holds the boot ID of each node by node name.
	// +optional
	BootIDs map[string]string `json:"bootIDs,omitempty"`

	// +optional
	// +listType=atomic
	Unmanaged []UnmanagedWorkload `json:"unmanaged,omitempty"`

	// Degraded lists the applications (namespace/name) that did not become ready at scale-up.
	// +optional
	// +listType=atomic
	Degraded []string `json:"degraded,omitempty"`

	// Failed lists the applications (namespace/name) that the operator could not stop: the API server
	// rejected a write, or a write still failed at the forced stop.
	// +optional
	// +listType=atomic
	Failed []string `json:"failed,omitempty"`

	// Stopped is the number of applications that have stopped.
	// +optional
	Stopped int32 `json:"stopped"`

	// Total is the number of applications in the record.
	// +optional
	Total int32 `json:"total"`

	// +optional
	ShutdownStartedAt *metav1.Time `json:"shutdownStartedAt,omitempty"`

	// +optional
	ScaleUpStartedAt *metav1.Time `json:"scaleUpStartedAt,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:validation:XValidation:rule="self.metadata.name == 'cluster'",message="the name must be cluster"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Stopped",type=integer,JSONPath=`.status.stopped`
// +kubebuilder:printcolumn:name="Total",type=integer,JSONPath=`.status.total`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ShutdownRequest is the latest shutdown event of the cluster.
type ShutdownRequest struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// +required
	Spec ShutdownRequestSpec `json:"spec"`

	// +optional
	Status ShutdownRequestStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// ShutdownRequestList contains a list of ShutdownRequest.
type ShutdownRequestList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []ShutdownRequest `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(GroupVersion, &ShutdownRequest{}, &ShutdownRequestList{})
		return nil
	})
}
