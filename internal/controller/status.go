package controller

import (
	"context"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	kumacov1alpha1 "github.com/kubereboot/kumaco/api/v1alpha1"
	"github.com/kubereboot/kumaco/internal/maintenance"
)

// status computes the report of a window from what was actually written.
func (r *Reconciler) status(view *view, decision maintenance.Decision, outcome *outcome) *kumacov1alpha1.MaintenanceWindowStatus {
	status := &kumacov1alpha1.MaintenanceWindowStatus{ObservedGeneration: view.object.Generation}
	if view.invalid != nil {
		status.State = kumacov1alpha1.StateInvalid
		status.Message = view.invalid.Error()
		return status
	}
	switch {
	case view.window.Active:
		status.State = kumacov1alpha1.StateInProgress
	case !view.window.NextBoundary.IsZero():
		status.State = kumacov1alpha1.StateScheduled
	default:
		status.State = kumacov1alpha1.StateClosed
	}
	status.MatchingNodes = int32(decision.Matching)
	for _, node := range decision.Under {
		// A start that was rejected is not a start.
		if !outcome.failed.Has(node) && len(status.NodesUnderMaintenance) < maxNodesUnderMaintenance {
			status.NodesUnderMaintenance = append(status.NodesUnderMaintenance, node)
		}
	}
	if view.window.Active {
		status.NodeCounts = map[string]int32{}
		for _, situation := range situations {
			status.NodeCounts[situation] = 0
		}
		for node, situation := range decision.Situations {
			if !outcome.failed.Has(node) {
				status.NodeCounts[situation]++
			}
		}
	}
	return status
}

// notStartedSituations returns what to remember for the messages of the nodes:
// the situations of the nodes of a window in progress, or nil.
func notStartedSituations(view *view, decision maintenance.Decision, outcome *outcome) map[string]string {
	if view.window == nil || !view.window.Active {
		return nil
	}
	result := map[string]string{}
	for node, situation := range decision.Situations {
		if !outcome.failed.Has(node) {
			result[node] = situation
		}
	}
	return result
}

// writeStatus writes the status when its content changed: at once when the
// state or the observed generation changed, and otherwise at most once per
// statusInterval, so that node changes across a large cluster do not cause a
// write storm. It returns when to try again a write that it put off.
func (r *Reconciler) writeStatus(ctx context.Context, view *view, status *kumacov1alpha1.MaintenanceWindowStatus) (time.Duration, error) {
	current := &view.object.Status
	if equality.Semantic.DeepEqual(current, status) {
		return 0, nil
	}
	name := view.object.Name
	urgent := current.State != status.State || current.ObservedGeneration != status.ObservedGeneration
	if wait := statusInterval - time.Since(r.statusWritten[name]); !urgent && wait > 0 {
		return wait, nil
	}
	updated := view.object.DeepCopy()
	updated.Status = *status
	if err := r.client.Status().Patch(ctx, updated, client.MergeFrom(view.object)); err != nil {
		return 0, client.IgnoreNotFound(err)
	}
	r.statusWritten[name] = time.Now()
	logf.FromContext(ctx).V(4).Info("Status written", "state", status.State, "observedGeneration", status.ObservedGeneration)
	return 0, nil
}
