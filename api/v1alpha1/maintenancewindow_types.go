package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// DefaultConditionPrefix is the default reserved prefix of the condition types
// that windows may declare (--condition-prefix). The CRD schema enforces it.
const DefaultConditionPrefix = "kumaco.kured.dev/"

// MaintenanceWindowSpec declares when, on which nodes, and under which
// conditions a kind of maintenance may be in progress. See README.md, "The
// kumaco.kured.dev MaintenanceWindow CRD".
type MaintenanceWindowSpec struct {
	// startTime says when the occurrences of the window start, as a Quartz cron
	// expression with an optional time zone prefix:
	// "[CRON_TZ=<IANA time zone> ]<second> <minute> <hour> <day of month> <month> <day of week> <year>".
	// The controller validates its grammar.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	StartTime string `json:"startTime"`

	// duration is how long each occurrence lasts: the window is active in
	// [start, start+duration). A Go duration string, from 1m to 744h.
	// +required
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('1m') && duration(self) <= duration('744h')",message="duration must be a duration from 1m to 744h (31 days)"
	Duration metav1.Duration `json:"duration"`

	// nodeSelector selects the nodes the window applies to, among the nodes the
	// controller manages. An empty selector selects all of them.
	// +required
	NodeSelector metav1.LabelSelector `json:"nodeSelector"`

	// conditionType is the kind of maintenance of this window, as the type of
	// the node condition that is True while it is in progress on a node. It
	// must start with the controller's reserved prefix.
	// +required
	// +kubebuilder:validation:MaxLength=316
	// +kubebuilder:validation:Pattern=`^kumaco\.kured\.dev/[A-Za-z0-9]([-A-Za-z0-9_.]{0,61}[A-Za-z0-9])?$`
	ConditionType string `json:"conditionType"`

	// startTriggers are CEL expressions that must all be true for maintenance
	// to start on a node.
	// +optional
	// +listType=atomic
	// +kubebuilder:validation:MaxItems=16
	// +kubebuilder:validation:items:MaxLength=4096
	StartTriggers []string `json:"startTriggers,omitempty"`

	// stopTriggers are CEL expressions of which any being true makes the window
	// stop keeping a node under maintenance.
	// +optional
	// +listType=atomic
	// +kubebuilder:validation:MaxItems=16
	// +kubebuilder:validation:items:MaxLength=4096
	StopTriggers []string `json:"stopTriggers,omitempty"`

	// concurrency is the maximum number of nodes, among the nodes this window
	// selects, under its kind of maintenance at the same time. 0 pauses the
	// window.
	// +required
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1000
	Concurrency *int32 `json:"concurrency"`
}

// MaintenanceWindowState summarizes a window.
// +kubebuilder:validation:Enum=InProgress;Scheduled;Closed;Invalid
type MaintenanceWindowState string

const (
	// StateInProgress: the current time falls within an occurrence.
	StateInProgress MaintenanceWindowState = "InProgress"
	// StateScheduled: no occurrence is active, but a future one exists.
	StateScheduled MaintenanceWindowState = "Scheduled"
	// StateClosed: no occurrence is active and none will start.
	StateClosed MaintenanceWindowState = "Closed"
	// StateInvalid: the spec cannot be used.
	StateInvalid MaintenanceWindowState = "Invalid"
)

// The situations of the matching nodes, as keys of status.nodeCounts.
const (
	SituationUnderMaintenance      = "UnderMaintenance"
	SituationUnmetTrigger          = "UnmetTrigger"
	SituationMaxConcurrencyReached = "MaxConcurrencyReached"
)

// MaintenanceWindowStatus is a report: the controller writes it and never
// reads it back.
type MaintenanceWindowStatus struct {
	// observedGeneration is the metadata.generation this status was computed from.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// state summarizes the window: InProgress, Scheduled, Closed, or Invalid.
	// +optional
	State MaintenanceWindowState `json:"state,omitempty"`

	// message says, for humans, why the window is Invalid.
	// +optional
	Message string `json:"message,omitempty"`

	// nodesUnderMaintenance names the nodes this window selects that are under
	// its kind of maintenance.
	// +optional
	// +listType=set
	// +kubebuilder:validation:MaxItems=1000
	NodesUnderMaintenance []string `json:"nodesUnderMaintenance,omitempty"`

	// matchingNodes is the number of managed nodes matching nodeSelector.
	// +optional
	MatchingNodes int32 `json:"matchingNodes"`

	// nodeCounts says, while the window is InProgress, how many matching nodes
	// are UnderMaintenance, UnmetTrigger, or MaxConcurrencyReached.
	// +optional
	NodeCounts map[string]int32 `json:"nodeCounts,omitempty"`
}

// MaintenanceWindow is the Schema for the maintenancewindows API.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
type MaintenanceWindow struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// +required
	Spec MaintenanceWindowSpec `json:"spec"`

	// +optional
	Status MaintenanceWindowStatus `json:"status,omitzero"`
}

// MaintenanceWindowList contains a list of MaintenanceWindow.
// +kubebuilder:object:root=true
type MaintenanceWindowList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []MaintenanceWindow `json:"items"`
}
