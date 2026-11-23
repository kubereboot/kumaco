package controller

import (
	"context"
	"strings"
	"sync"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/resourceversion"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/kubereboot/kumaco/internal/maintenance"
)

// listNodes returns the managed nodes, as the cache holds them: they are
// shared with the informer and must never be changed.
func listNodes(ctx context.Context, reader client.Reader) ([]*corev1.Node, error) {
	var list corev1.NodeList
	if err := reader.List(ctx, &list, &client.ListOptions{UnsafeDisableDeepCopy: ptr.To(true)}); err != nil {
		return nil, err
	}
	nodes := make([]*corev1.Node, len(list.Items))
	for i := range list.Items {
		nodes[i] = &list.Items[i]
	}
	return nodes, nil
}

// nodeWriter writes the kumaco conditions of nodes, for the reconciler and for
// the sweep, one write at a time. It remembers the writes that changed records
// until the cache shows them ("Reading its own writes").
type nodeWriter struct {
	reader  client.Reader
	applier NodeStatusApplier
	metrics *metrics

	mu sync.Mutex
	// expected is, per node, the resourceVersion of the last write that
	// changed its types or statuses, until the cache has reached it.
	expected map[string]string
}

func newNodeWriter(reader client.Reader, applier NodeStatusApplier, metrics *metrics) *nodeWriter {
	return &nodeWriter{reader: reader, applier: applier, metrics: metrics, expected: map[string]string{}}
}

// caughtUp says whether the cache shows all the writes that changed records.
// Counting nodes from an older cache could exceed a concurrency.
func (w *nodeWriter) caughtUp(ctx context.Context) (bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for name := range w.expected {
		shown, err := w.shows(ctx, name)
		if err != nil || !shown {
			return false, err
		}
	}
	return true, nil
}

// shows says whether the cache has reached the expected write of the node, and
// forgets the expectation when it has. The caller holds the lock.
func (w *nodeWriter) shows(ctx context.Context, name string) (bool, error) {
	expected, isExpected := w.expected[name]
	if !isExpected {
		return true, nil
	}
	var node corev1.Node
	err := w.reader.Get(ctx, client.ObjectKey{Name: name}, &node, &client.GetOptions{UnsafeDisableDeepCopy: ptr.To(true)})
	switch {
	case apierrors.IsNotFound(err):
		// The node was deleted: its records went with it.
	case err != nil:
		return false, err
	default:
		// A version that cannot be compared is not waited for.
		if order, err := resourceversion.CompareResourceVersion(node.ResourceVersion, expected); err == nil && order < 0 {
			return false, nil
		}
	}
	delete(w.expected, name)
	return true, nil
}

// apply sends the complete list of kumaco conditions of a node, as decided
// from the given node of the cache. A write that starts a kind of maintenance
// is rejected with a conflict when the node changed since.
func (w *nodeWriter) apply(ctx context.Context, node *corev1.Node, write maintenance.Write) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.applyLocked(ctx, node, write)
}

func (w *nodeWriter) applyLocked(ctx context.Context, node *corev1.Node, write maintenance.Write) error {
	logf.FromContext(ctx).V(4).Info("Applying node conditions", "node", node.Name,
		"conditions", summary(write.Conditions), "resourceVersionPrecondition", write.Starts)
	applied, err := w.applier.ApplyStatus(ctx, applyConfiguration(node, write),
		metav1.ApplyOptions{FieldManager: FieldManager, Force: true})
	switch {
	case apierrors.IsConflict(err) || isAnotherNode(err):
		w.metrics.nodeWritten(resultConflict)
		return err
	case err != nil:
		w.metrics.nodeWritten(resultError)
		return err
	}
	w.metrics.nodeWritten(resultOK)
	if write.Changed {
		w.expected[node.Name] = applied.ResourceVersion
	}
	return nil
}

// sweep computes the conditions of one node from the cache, and applies them
// when the node differs or a heartbeat is due. It leaves alone a node that is
// not in the cache anymore, or whose last write the cache does not show yet:
// a list computed from an older node would undo that write.
func (w *nodeWriter) sweep(ctx context.Context, name string, desired func() maintenance.Desired) (skipped bool, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	shown, err := w.shows(ctx, name)
	if err != nil || !shown {
		return true, err
	}
	var node corev1.Node
	err = w.reader.Get(ctx, client.ObjectKey{Name: name}, &node, &client.GetOptions{UnsafeDisableDeepCopy: ptr.To(true)})
	if err != nil {
		return false, client.IgnoreNotFound(err)
	}
	write := maintenance.DesiredConditions(&node, desired())
	if !write.Changed && !write.Refreshed {
		return false, nil
	}
	// The sweep starts nothing: it never needs the precondition.
	write.Starts = false
	err = w.applyLocked(ctx, &node, write)
	if apierrors.IsNotFound(err) || isAnotherNode(err) {
		// The node was deleted, or created again: this list is not for it.
		return false, nil
	}
	return false, err
}

// isAnotherNode says whether the API server refused a write because the node
// was deleted and created again with the same name: the UID that the write
// carries is not the one of the new node, and the UID is immutable.
func isAnotherNode(err error) bool {
	return apierrors.IsInvalid(err) && strings.Contains(err.Error(), "metadata.uid")
}

// applyConfiguration builds the apply of the complete list. The UID makes the
// API server refuse a node that was recreated with the same name: its
// conditions were decided for another node.
func applyConfiguration(node *corev1.Node, write maintenance.Write) *corev1ac.NodeApplyConfiguration {
	status := corev1ac.NodeStatus()
	for _, condition := range write.Conditions {
		status.WithConditions(corev1ac.NodeCondition().
			WithType(condition.Type).
			WithStatus(condition.Status).
			WithReason(condition.Reason).
			WithMessage(condition.Message).
			WithLastTransitionTime(condition.LastTransitionTime).
			WithLastHeartbeatTime(condition.LastHeartbeatTime))
	}
	apply := corev1ac.Node(node.Name).WithUID(node.UID).WithStatus(status)
	if write.Starts {
		apply.WithResourceVersion(node.ResourceVersion)
	}
	return apply
}

// summary returns the types and statuses of conditions, for the logs.
func summary(conditions []corev1.NodeCondition) []string {
	result := make([]string, len(conditions))
	for i, condition := range conditions {
		result[i] = string(condition.Type) + "=" + string(condition.Status)
	}
	return result
}
