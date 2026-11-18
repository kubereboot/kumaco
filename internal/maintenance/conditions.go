package maintenance

import (
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
)

// The reasons of the conditions kumaco writes.
const (
	ReasonMaintenanceRunning = "KumacoMaintenanceRunning"
	ReasonNoMaintenance      = "KumacoNoMaintenance"
)

// Owned says whether kumaco owns a condition type: the generic
// MaintenanceInProgress, and the types under its reserved prefix.
func Owned(conditionType corev1.NodeConditionType, prefix string) bool {
	return conditionType == corev1.NodeMaintenanceInProgress || strings.HasPrefix(string(conditionType), prefix)
}

// Condition returns the condition of that type on the node, or nil.
func Condition(node *corev1.Node, conditionType string) *corev1.NodeCondition {
	for i := range node.Status.Conditions {
		if string(node.Status.Conditions[i].Type) == conditionType {
			return &node.Status.Conditions[i]
		}
	}
	return nil
}

// Under returns since when the node is under that kind of maintenance. The
// record of that fact is the condition of the kind being True on the node.
func Under(node *corev1.Node, kind string) (since time.Time, ok bool) {
	condition := Condition(node, kind)
	if condition == nil || condition.Status != corev1.ConditionTrue {
		return time.Time{}, false
	}
	return condition.LastTransitionTime.Time, true
}

// NotStarted says that an InProgress window selecting a node did not start
// maintenance on it, and why.
type NotStarted struct {
	Window    string
	Situation string
}

// Desired is the input of DesiredConditions, besides the node.
type Desired struct {
	// Prefix is the reserved prefix of the controller.
	Prefix string
	// WindowTypes are the condition types of the existing windows.
	WindowTypes sets.Set[string]
	// Started are the kinds that the reconcile in progress starts on the node,
	// with the name of the window starting them.
	Started map[string]string
	// Released are the kinds that the reconcile in progress releases.
	Released []string
	// NotStarted is only used for the message of MaintenanceInProgress.
	NotStarted []NotStarted
	// Now is the time given to what changes.
	Now time.Time
	// HeartbeatStaleBefore refreshes the heartbeats that are older than it.
	// The zero value refreshes none.
	HeartbeatStaleBefore time.Time
}

// Write is the complete list of the kumaco conditions of a node, to send in
// one server-side apply.
type Write struct {
	Conditions []corev1.NodeCondition
	// Changed says that the node does not have these types and statuses.
	Changed bool
	// Starts says that a kind of maintenance starts: the write must be
	// rejected if the node changed since the decision.
	Starts bool
	// Refreshed says that a heartbeat was due.
	Refreshed bool
}

// DesiredConditions computes the complete list of kumaco conditions a node
// must have, from the node, the existing windows, and the decisions of the
// reconcile in progress. It uses no other state, and does not change the node.
//
// See README.md, "One function, one write per node".
func DesiredConditions(node *corev1.Node, desired Desired) Write {
	kinds := sets.New[string]()
	for _, condition := range node.Status.Conditions {
		conditionType := string(condition.Type)
		isRecord := strings.HasPrefix(conditionType, desired.Prefix) && condition.Status == corev1.ConditionTrue
		if isRecord && desired.WindowTypes.Has(conditionType) {
			kinds.Insert(conditionType)
		}
	}
	kinds.Delete(desired.Released...)
	for kind := range desired.Started {
		kinds.Insert(kind)
	}
	inProgress := sets.List(kinds)

	var write Write
	for _, kind := range inProgress {
		existing := Condition(node, kind)
		message := "Started by MaintenanceWindow " + desired.Started[kind]
		if existing != nil && existing.Status == corev1.ConditionTrue {
			message = existing.Message
		} else {
			write.Starts = true
		}
		write.add(existing, desired, corev1.NodeCondition{
			Type: corev1.NodeConditionType(kind), Status: corev1.ConditionTrue,
			Reason: ReasonMaintenanceRunning, Message: message,
		})
	}
	generic := corev1.NodeCondition{
		Type: corev1.NodeMaintenanceInProgress, Status: corev1.ConditionFalse,
		Reason: ReasonNoMaintenance, Message: message(inProgress, desired.NotStarted),
	}
	if len(inProgress) > 0 {
		generic.Status, generic.Reason = corev1.ConditionTrue, ReasonMaintenanceRunning
	}
	write.add(Condition(node, string(generic.Type)), desired, generic)

	write.Changed = !slices.Equal(ownedStatuses(node.Status.Conditions, desired.Prefix), ownedStatuses(write.Conditions, desired.Prefix))
	return write
}

// add gives its timestamps to a condition and adds it to the list:
// lastTransitionTime is kept unless the status changes, and lastHeartbeatTime
// is kept unless it is due. A new condition gets the current time.
func (w *Write) add(existing *corev1.NodeCondition, desired Desired, condition corev1.NodeCondition) {
	// The API stores times with a precision of one second.
	now := metav1.NewTime(desired.Now.Truncate(time.Second))
	condition.LastTransitionTime, condition.LastHeartbeatTime = now, now
	if existing != nil {
		condition.LastHeartbeatTime = existing.LastHeartbeatTime
		if existing.Status == condition.Status {
			condition.LastTransitionTime = existing.LastTransitionTime
		}
		if existing.LastHeartbeatTime.Time.Before(desired.HeartbeatStaleBefore) {
			condition.LastHeartbeatTime = now
			w.Refreshed = true
		}
	}
	w.Conditions = append(w.Conditions, condition)
}

// message is for humans: nothing parses it.
func message(inProgress []string, notStarted []NotStarted) string {
	text := "No maintenance in progress."
	if len(inProgress) > 0 {
		text = "In progress: " + strings.Join(inProgress, ", ") + "."
	}
	if len(notStarted) == 0 {
		return text
	}
	reasons := make([]string, len(notStarted))
	for i, window := range notStarted {
		reasons[i] = fmt.Sprintf("%s (%s)", window.Window, window.Situation)
	}
	slices.Sort(reasons)
	return text + " Not started: " + strings.Join(reasons, ", ")
}

// ownedStatuses returns the types and statuses of the conditions kumaco owns,
// sorted, to compare two lists of conditions.
func ownedStatuses(conditions []corev1.NodeCondition, prefix string) []string {
	var result []string
	for _, condition := range conditions {
		if Owned(condition.Type, prefix) {
			result = append(result, string(condition.Type)+"="+string(condition.Status))
		}
	}
	slices.Sort(result)
	return result
}

// RelevantChange says whether a change of a node can change a decision: its
// spec, its labels and annotations, and its conditions, except their
// heartbeats and the conditions kumaco owns. See README.md, "What does it
// reconcile, and when?".
func RelevantChange(before, after *corev1.Node, prefix string) bool {
	return !equality.Semantic.DeepEqual(before.Spec, after.Spec) ||
		!equality.Semantic.DeepEqual(before.Labels, after.Labels) ||
		!equality.Semantic.DeepEqual(before.Annotations, after.Annotations) ||
		!slices.Equal(foreignConditions(before, prefix), foreignConditions(after, prefix))
}

// foreignConditions returns the conditions kumaco does not own, without their
// heartbeats.
func foreignConditions(node *corev1.Node, prefix string) []corev1.NodeCondition {
	var result []corev1.NodeCondition
	for _, condition := range node.Status.Conditions {
		if !Owned(condition.Type, prefix) {
			condition.LastHeartbeatTime = metav1.Time{}
			result = append(result, condition)
		}
	}
	return result
}
