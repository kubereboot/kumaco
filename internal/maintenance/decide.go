package maintenance

import (
	"cmp"
	"context"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"

	kumacov1alpha1 "github.com/kubereboot/kumaco/api/v1alpha1"
	"github.com/kubereboot/kumaco/internal/trigger"
)

// Release is a node that no window of the kind keeps.
type Release struct {
	Node *corev1.Node
	// Reason is for humans.
	Reason string
}

// TriggerFailure is an expression that errored or timed out for a node.
type TriggerFailure struct {
	Window     string
	Node       string
	Expression string
	Err        error
}

// Input is what one reconcile of a window decides from.
type Input struct {
	// Kind is the condition type of the reconciled window.
	Kind string
	// Window is the reconciled window. It is nil when the window was deleted
	// or is Invalid: its reconcile then only releases.
	Window *Window
	// ReleaseReason says, when Window is nil, why it keeps nothing.
	ReleaseReason string
	// Windows are the valid windows, all at the same time. Only those of the
	// kind are looked at.
	Windows []*Window
	// Nodes are the managed nodes, as seen in the cache. They are not changed.
	Nodes []*corev1.Node
}

// Decision is the result of one reconcile of a window.
type Decision struct {
	// Released are the nodes of the kind that no window keeps.
	Released []Release
	// Started are the nodes on which maintenance starts, in order.
	Started []*corev1.Node
	// Matching is the number of nodes that the window selects.
	Matching int
	// Under names, sorted, the nodes that the window selects and that are
	// under its kind of maintenance after this decision.
	Under []string
	// Situations is, while the window is InProgress, the situation of each
	// node that it selects.
	Situations map[string]string
	// Failures are the expressions that failed. They changed nothing.
	Failures []TriggerFailure
}

// Decide runs the release, then finds the candidates and fills the free slots
// of the window. See README.md, "Reconciling a window" (steps two to four).
func Decide(ctx context.Context, in Input) Decision {
	evaluator := &evaluator{ctx: ctx, variables: map[string]map[string]any{}}
	decision := Decision{}

	// Second step: release. It runs before any start, so that the slots it
	// frees can be reused in the same reconcile.
	released := map[string]bool{}
	for _, node := range in.Nodes {
		since, isUnder := Under(node, in.Kind)
		if !isUnder {
			continue
		}
		if reason, isReleased := evaluator.release(in, node, since); isReleased {
			decision.Released = append(decision.Released, Release{Node: node, Reason: reason})
			released[node.Name] = true
		}
	}
	if in.Window != nil {
		decision.start(evaluator, in.Window, in.Nodes, released)
	}
	decision.Failures = evaluator.failures
	return decision
}

// start finds the candidates and fills the free slots of the window.
func (d *Decision) start(evaluator *evaluator, window *Window, nodes []*corev1.Node, released map[string]bool) {
	// Third step: find the candidates, among the nodes that the window selects
	// and that are not under its kind of maintenance. A node released by this
	// reconcile is not started again by it.
	situations := map[string]string{}
	var candidates []*corev1.Node
	for _, node := range nodes {
		if !window.Selector.Matches(labels.Set(node.Labels)) {
			continue
		}
		d.Matching++
		_, isUnder := Under(node, window.ConditionType)
		switch {
		case isUnder && !released[node.Name]:
			situations[node.Name] = kumacov1alpha1.SituationUnderMaintenance
			d.Under = append(d.Under, node.Name)
		case !window.Active:
		case !isUnder && evaluator.isCandidate(window, node):
			candidates = append(candidates, node)
		default:
			situations[node.Name] = kumacov1alpha1.SituationUnmetTrigger
		}
	}

	// Fourth step: fill the free slots. When concurrency was lowered, there
	// are none, and nothing is released either.
	slices.SortFunc(candidates, compareCandidates)
	free := max(window.Concurrency-len(d.Under), 0)
	for i, node := range candidates {
		if i < free {
			d.Started = append(d.Started, node)
			d.Under = append(d.Under, node.Name)
			situations[node.Name] = kumacov1alpha1.SituationUnderMaintenance
		} else {
			situations[node.Name] = kumacov1alpha1.SituationMaxConcurrencyReached
		}
	}
	slices.Sort(d.Under)
	if window.Active {
		d.Situations = situations
	}
}

// compareCandidates orders the candidates: first those already under another
// kind of maintenance, most recent first, to combine maintenances rather than
// take one more node out of service; then the others, the one that left
// maintenance the longest time ago first. Ties are broken by name.
func compareCandidates(a, b *corev1.Node) int {
	aCombined, aSince := maintenanceInProgress(a)
	bCombined, bSince := maintenanceInProgress(b)
	switch {
	case aCombined != bCombined && aCombined:
		return -1
	case aCombined != bCombined:
		return 1
	case aCombined:
		return cmp.Or(bSince.Compare(aSince), cmp.Compare(a.Name, b.Name))
	}
	return cmp.Or(aSince.Compare(bSince), cmp.Compare(a.Name, b.Name))
}

// maintenanceInProgress returns whether some maintenance is in progress on the
// node, and since when it is, or is not. A node without the condition yet is
// not under maintenance since its creation.
func maintenanceInProgress(node *corev1.Node) (bool, time.Time) {
	condition := Condition(node, string(corev1.NodeMaintenanceInProgress))
	if condition == nil {
		return false, node.CreationTimestamp.Time
	}
	return condition.Status == corev1.ConditionTrue, condition.LastTransitionTime.Time
}

// evaluator evaluates triggers for one reconcile. It builds the CEL variable
// of a node once, and collects the failures.
type evaluator struct {
	ctx       context.Context
	variables map[string]map[string]any
	failures  []TriggerFailure
}

// release says whether no window of the kind keeps the node, and why the
// reconciled window does not.
func (e *evaluator) release(in Input, node *corev1.Node, since time.Time) (string, bool) {
	reason := in.ReleaseReason
	if in.Window != nil {
		if reason = e.whyNotKept(in.Window, node, since); reason == "" {
			return "", false
		}
	}
	for _, window := range in.Windows {
		if window != in.Window && window.ConditionType == in.Kind && e.whyNotKept(window, node, since) == "" {
			return "", false
		}
	}
	return reason, true
}

// whyNotKept returns the empty string when the window keeps the node, which is
// under its kind of maintenance since the given time. See README.md, "When
// does a window keep a node under maintenance?".
func (e *evaluator) whyNotKept(window *Window, node *corev1.Node, since time.Time) string {
	switch {
	case !window.Active:
		return "the window is not in progress"
	case !window.Selector.Matches(labels.Set(node.Labels)):
		return "the node does not match the nodeSelector"
	case since.Before(window.OccurrenceStart):
		return "its maintenance started before the current occurrence of the window"
	case e.anyMatches(window, window.StopTriggers, node):
		return "a stopTriggers expression matched"
	}
	return ""
}

// isCandidate says whether all the startTriggers match and none of the
// stopTriggers does. An expression that fails makes the node not a candidate.
func (e *evaluator) isCandidate(window *Window, node *corev1.Node) bool {
	for _, expression := range window.StartTriggers {
		if matches, ok := e.eval(window, expression, node); !ok || !matches {
			return false
		}
	}
	for _, expression := range window.StopTriggers {
		if matches, ok := e.eval(window, expression, node); !ok || matches {
			return false
		}
	}
	return true
}

// anyMatches says whether one of the expressions matches. An expression that
// fails does not match.
func (e *evaluator) anyMatches(window *Window, expressions []*trigger.Expression, node *corev1.Node) bool {
	for _, expression := range expressions {
		if matches, ok := e.eval(window, expression, node); ok && matches {
			return true
		}
	}
	return false
}

// eval evaluates one expression on one node. A failure is recorded, and has no
// result: its callers fail closed.
func (e *evaluator) eval(window *Window, expression *trigger.Expression, node *corev1.Node) (matches, ok bool) {
	variable, err := e.variable(node)
	if err == nil {
		matches, err = expression.Eval(e.ctx, variable)
	}
	if err != nil {
		e.failures = append(e.failures, TriggerFailure{
			Window: window.Name, Node: node.Name, Expression: expression.String(), Err: err,
		})
		return false, false
	}
	return matches, true
}

func (e *evaluator) variable(node *corev1.Node) (map[string]any, error) {
	if variable, ok := e.variables[node.Name]; ok {
		return variable, nil
	}
	variable, err := trigger.NodeVariable(node)
	if err != nil {
		return nil, err
	}
	e.variables[node.Name] = variable
	return variable, nil
}
