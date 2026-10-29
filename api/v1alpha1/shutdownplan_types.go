package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// PlanResult is the worst severity of the findings of a ShutdownPlan.
// +kubebuilder:validation:Enum=OK;Warnings;Errors
type PlanResult string

const (
	PlanResultOK       PlanResult = "OK"
	PlanResultWarnings PlanResult = "Warnings"
	PlanResultErrors   PlanResult = "Errors"
)

// Severity is the severity of a finding.
// +kubebuilder:validation:Enum=Error;Warning
type Severity string

const (
	SeverityError   Severity = "Error"
	SeverityWarning Severity = "Warning"
)

// The reasons of the findings.
const (
	ReasonCycle             = "Cycle"
	ReasonDepthAboveLimit   = "DepthAboveLimit"
	ReasonUnknownDependency = "UnknownDependency"
	ReasonMissingWorkload   = "MissingWorkload"
	ReasonGracePeriodShrink = "GracePeriodShrink"
)

// PlannedWorkload is a workload and the action that a real shutdown does on it.
type PlannedWorkload struct {
	// +required
	Kind WorkloadKind `json:"kind"`

	// +required
	Name string `json:"name"`

	// Action is the action of a real shutdown, for example ScaleToZero.
	// +required
	Action string `json:"action"`
}

// PlannedApplication is an application and its time plan.
type PlannedApplication struct {
	// +required
	Namespace string `json:"namespace"`

	// +required
	Name string `json:"name"`

	// GracePeriod is the configured gracePeriod.
	// +required
	GracePeriod metav1.Duration `json:"gracePeriod"`

	// ShrunkGracePeriod is the gracePeriod after the shrink. It is equal to GracePeriod when the group fits.
	// +required
	ShrunkGracePeriod metav1.Duration `json:"shrunkGracePeriod"`

	// Deadline is the latest time of the forced stop, as the time after the start of the shutdown.
	// +required
	Deadline metav1.Duration `json:"deadline"`

	// +optional
	// +listType=atomic
	Workloads []PlannedWorkload `json:"workloads,omitempty"`
}

// PlannedTier is the set of applications at one position of a dependency group.
// Tier 1 stops first.
type PlannedTier struct {
	// +required
	Tier int32 `json:"tier"`

	// +required
	// +listType=atomic
	Applications []PlannedApplication `json:"applications"`
}

// PlannedGroup is a dependency group: a set of applications that dependsOn links together.
type PlannedGroup struct {
	// LongestChain is the gracePeriod sum of the longest chain in the group, before the shrink.
	// +required
	LongestChain metav1.Duration `json:"longestChain"`

	// Available is the time from the start of the group to the end of the budget minus the safety margin.
	// +required
	Available metav1.Duration `json:"available"`

	// +required
	// +listType=atomic
	Tiers []PlannedTier `json:"tiers"`
}

// Finding is a problem in the plan.
type Finding struct {
	// +required
	Severity Severity `json:"severity"`

	// Reason is the type of the finding, for example Cycle.
	// +required
	Reason string `json:"reason"`

	// Applications are the applications (namespace/name) that the finding is about.
	// +optional
	// +listType=atomic
	Applications []string `json:"applications,omitempty"`

	// +required
	Message string `json:"message"`

	// Fallback is the rule that a real shutdown uses.
	// +required
	Fallback string `json:"fallback"`
}

// ShutdownPlanStatus is the snapshot of the plan. The operator writes it once.
type ShutdownPlanStatus struct {
	// ComputedAt is the time of the snapshot.
	// +optional
	ComputedAt *metav1.Time `json:"computedAt,omitempty"`

	// +optional
	Result PlanResult `json:"result,omitempty"`

	// +optional
	// +listType=atomic
	Groups []PlannedGroup `json:"groups,omitempty"`

	// +optional
	// +listType=atomic
	Findings []Finding `json:"findings,omitempty"`

	// Report is the plan as text.
	// +optional
	Report string `json:"report,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Result",type=string,JSONPath=`.status.result`
// +kubebuilder:printcolumn:name="Computed",type=date,JSONPath=`.status.computedAt`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ShutdownPlan is a dry run of a shutdown. The operator plans for the whole cluster and changes nothing.
type ShutdownPlan struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// +optional
	Status ShutdownPlanStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// ShutdownPlanList contains a list of ShutdownPlan.
type ShutdownPlanList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []ShutdownPlan `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(GroupVersion, &ShutdownPlan{}, &ShutdownPlanList{})
		return nil
	})
}
