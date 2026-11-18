package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	kumacov1alpha1 "github.com/kubereboot/kumaco/api/v1alpha1"
	"github.com/kubereboot/kumaco/internal/maintenance"
)

// The reasons of the events on MaintenanceWindows. See README.md, "Debugging".
const (
	EventMaintenanceStarted  = "MaintenanceStarted"
	EventMaintenanceReleased = "MaintenanceReleased"
	EventTriggerFailed       = "TriggerFailed"
	EventStartConflict       = "StartConflict"
)

const (
	// cacheLagRetry is how long a reconcile is put off while the cache does
	// not show the node writes of the controller yet.
	cacheLagRetry = 10 * time.Millisecond
	// releasedRetry is how long after a release the window is reconciled
	// again, for the released nodes to be candidates. It also bounds the pace
	// of a window whose stopTriggers undo its own starts (README, "Gotchas").
	releasedRetry = time.Second
	// maxNodesUnderMaintenance bounds status.nodesUnderMaintenance, as the CRD does.
	maxNodesUnderMaintenance = 1000
)

// statusInterval is the shortest time between two status writes of a window,
// unless its state or its observed generation changes. Tests shorten it.
var statusInterval = 10 * time.Second

// The access rights of the controller: it watches its windows and the nodes,
// writes the status of both, and emits events. See README.md, "Security and
// access rights".
//
// +kubebuilder:rbac:groups=kumaco.kured.dev,resources=maintenancewindows,verbs=get;list;watch
// +kubebuilder:rbac:groups=kumaco.kured.dev,resources=maintenancewindows/status,verbs=patch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=nodes/status,verbs=patch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

// Reconciler reconciles one MaintenanceWindow at a time.
type Reconciler struct {
	// client reads from the informer cache, and writes window statuses.
	client   client.Client
	writer   *nodeWriter
	state    *state
	metrics  *metrics
	recorder events.EventRecorder
	options  Options
	// enqueue receives the other windows to reconcile after a node write.
	enqueue chan<- event.GenericEvent

	// The fields below are derived data of the reconciler only, which
	// reconciles one window at a time.

	// known is, per window, what its last reconcile saw of it.
	known map[string]knownWindow
	// compiled caches the compilation of the specs.
	compiled map[types.UID]compiledSpec
	// failing are, per window, the expressions that failed in its last reconcile.
	failing map[string]sets.Set[string]
	// statusWritten is, per window, when its status was last written.
	statusWritten map[string]time.Time
}

// knownWindow lets the reconcile of a deleted or edited window release the
// nodes of the kind it had, and emit events about it.
type knownWindow struct {
	kind   string
	object *kumacov1alpha1.MaintenanceWindow
}

type compiledSpec struct {
	generation int64
	compiled   *maintenance.Compiled
	err        error
}

// view is what one reconcile decides from, read once from the cache.
type view struct {
	now time.Time
	// object is the reconciled window, or nil when it was deleted.
	object *kumacov1alpha1.MaintenanceWindow
	// window is the reconciled window when it is valid.
	window *maintenance.Window
	// invalid says why the reconciled window is Invalid.
	invalid error
	// windows are the valid windows, at the same time.
	windows []*maintenance.Window
	// types are the condition types of the existing windows.
	types sets.Set[string]
	nodes []*corev1.Node
}

// Reconcile implements README.md, "Reconciling a window".
func (r *Reconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	select {
	case <-r.state.swept:
	case <-ctx.Done():
		return ctrl.Result{}, nil
	}
	if caughtUp, err := r.writer.caughtUp(ctx); err != nil || !caughtUp {
		return ctrl.Result{RequeueAfter: cacheLagRetry}, err
	}
	started := time.Now()
	result, err := r.reconcile(ctx, request.Name)
	r.metrics.reconciled(request.Name, time.Since(started), err)
	if err == nil {
		r.state.reconciled(request.Name)
	}
	return result, err
}

func (r *Reconciler) reconcile(ctx context.Context, name string) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	view, err := r.read(ctx, name)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Steps one to four: the decisions. The kind that the window had before
	// it was deleted or edited is released too.
	previous := r.known[name]
	kind := ""
	if view.object != nil && maintenance.CheckConditionType(view.object.Spec.ConditionType, r.options.ConditionPrefix) == nil {
		kind = view.object.Spec.ConditionType
	}
	changes := newNodeChanges()
	if previous.kind != "" && previous.kind != kind {
		reason := "the conditionType of the window changed"
		if view.object == nil {
			reason = "the window was deleted"
		}
		changes.add(previous.kind, name, maintenance.Decide(ctx, maintenance.Input{
			Kind: previous.kind, ReleaseReason: reason, Windows: view.windows, Nodes: view.nodes,
		}))
	}
	var decision maintenance.Decision
	if kind != "" {
		decision = maintenance.Decide(ctx, maintenance.Input{
			Kind: kind, Window: view.window, ReleaseReason: "the window is Invalid", Windows: view.windows, Nodes: view.nodes,
		})
		changes.add(kind, name, decision)
	}

	// Step five: one write per node released or started.
	regarding := view.object
	if regarding == nil {
		regarding = previous.object
	}
	outcome := r.write(ctx, view, regarding, changes)

	// Step six: report.
	if view.object == nil {
		r.forget(name)
		log.V(1).Info("Reconciled a deleted window", "released", outcome.released)
		return ctrl.Result{}, errors.Join(outcome.errs...)
	}
	r.known[name] = knownWindow{kind: kind, object: view.object}
	r.reportFailures(view, decision.Failures)
	status := r.status(view, decision, outcome)
	r.state.remember(name, notStartedSituations(view, decision, outcome))
	r.metrics.windowReported(name, status)
	r.enqueueOthers(ctx, name, view, outcome)
	if len(outcome.errs) > 0 {
		return ctrl.Result{}, errors.Join(outcome.errs...)
	}
	statusRetry, err := r.writeStatus(ctx, view, status)
	if err != nil {
		return ctrl.Result{}, err
	}

	requeue := r.requeueAfter(view, outcome, statusRetry)
	log.V(1).Info("Reconciled", "state", status.State, "matchingNodes", status.MatchingNodes,
		"nodeCounts", status.NodeCounts, "started", outcome.started, "released", outcome.released,
		"conflicts", outcome.conflicts, "requeueAfter", requeue.String())
	return ctrl.Result{RequeueAfter: requeue}, nil
}

// read reads the windows and the nodes from the cache, and the time, once.
func (r *Reconciler) read(ctx context.Context, name string) (*view, error) {
	view := &view{now: r.options.Now(), types: sets.New[string]()}
	var windows kumacov1alpha1.MaintenanceWindowList
	if err := r.client.List(ctx, &windows); err != nil {
		return nil, err
	}
	for i := range windows.Items {
		object := &windows.Items[i]
		view.types.Insert(object.Spec.ConditionType)
		compiled, err := r.compile(object)
		if object.Name == name {
			view.object, view.invalid = object, err
		}
		if err != nil {
			continue
		}
		window := compiled.At(object.Name, view.now)
		view.windows = append(view.windows, window)
		if object.Name == name {
			view.window = window
		}
	}
	var err error
	view.nodes, err = listNodes(ctx, r.client)
	return view, err
}

// compile compiles the spec of a window, once per generation.
func (r *Reconciler) compile(object *kumacov1alpha1.MaintenanceWindow) (*maintenance.Compiled, error) {
	cached, ok := r.compiled[object.UID]
	if !ok || cached.generation != object.Generation {
		cached = compiledSpec{generation: object.Generation}
		cached.compiled, cached.err = maintenance.Compile(&object.Spec, r.options.ConditionPrefix)
		r.compiled[object.UID] = cached
	}
	return cached.compiled, cached.err
}

// forget drops what is remembered of a deleted window.
func (r *Reconciler) forget(name string) {
	if known, ok := r.known[name]; ok && known.object != nil {
		delete(r.compiled, known.object.UID)
	}
	delete(r.known, name)
	delete(r.failing, name)
	delete(r.statusWritten, name)
	r.state.remember(name, nil)
	r.metrics.windowDeleted(name)
}

// requeueAfter returns when to reconcile the window again without an event:
// its next schedule boundary, or earlier when something is left to do.
func (r *Reconciler) requeueAfter(view *view, outcome *outcome, statusRetry time.Duration) time.Duration {
	var requeue time.Duration
	sooner := func(delay time.Duration) {
		if delay > 0 && (requeue == 0 || delay < requeue) {
			requeue = delay
		}
	}
	sooner(statusRetry)
	if view.window == nil {
		return requeue
	}
	if boundary := view.window.NextBoundary; !boundary.IsZero() {
		sooner(max(boundary.Sub(view.now), time.Millisecond))
	}
	if view.window.Active && len(outcome.released) > 0 {
		sooner(releasedRetry)
	}
	if outcome.conflicts > 0 {
		// A rejected start is decided again, on the fresh node.
		sooner(cacheLagRetry)
	}
	return requeue
}

// nodeChange is what one reconcile changes on one node.
type nodeChange struct {
	node     *corev1.Node
	started  map[string]string
	released []string
	reasons  []string
}

// nodeChanges are the changes of one reconcile, per node, in order.
type nodeChanges struct {
	byNode map[string]*nodeChange
	order  []*nodeChange
}

func newNodeChanges() *nodeChanges {
	return &nodeChanges{byNode: map[string]*nodeChange{}}
}

func (c *nodeChanges) of(node *corev1.Node) *nodeChange {
	change, ok := c.byNode[node.Name]
	if !ok {
		change = &nodeChange{node: node, started: map[string]string{}}
		c.byNode[node.Name] = change
		c.order = append(c.order, change)
	}
	return change
}

// add records the releases and the starts of a decision about one kind.
func (c *nodeChanges) add(kind, window string, decision maintenance.Decision) {
	for _, release := range decision.Released {
		change := c.of(release.Node)
		change.released = append(change.released, kind)
		change.reasons = append(change.reasons, release.Reason)
	}
	for _, node := range decision.Started {
		c.of(node).started[kind] = window
	}
}

// outcome is what the writes of one reconcile did.
type outcome struct {
	started   []string
	released  []string
	conflicts int
	// failed are the nodes whose write did not happen.
	failed sets.Set[string]
	errs   []error
}

// write applies the complete list of conditions of every node released or
// started. A start whose write is rejected is not a start: nothing is recorded.
func (r *Reconciler) write(ctx context.Context, view *view, regarding *kumacov1alpha1.MaintenanceWindow, changes *nodeChanges) *outcome {
	log := logf.FromContext(ctx)
	outcome := &outcome{failed: sets.New[string]()}
	for _, change := range changes.order {
		node := change.node
		desired := maintenance.Desired{
			Prefix:      r.options.ConditionPrefix,
			WindowTypes: view.types,
			Started:     change.started,
			Released:    change.released,
			NotStarted:  r.state.notStartedOn(node.Name),
			Now:         view.now,
		}
		if r.state.heartbeatsStarted() {
			desired.HeartbeatStaleBefore = view.now.Add(-heartbeatDue(r.options.HeartbeatInterval))
		}
		err := r.writer.apply(ctx, node, maintenance.DesiredConditions(node, desired))
		switch {
		case err == nil:
		case apierrors.IsNotFound(err):
			// The node was deleted: its records went with it.
			outcome.failed.Insert(node.Name)
			continue
		case apierrors.IsConflict(err) || isAnotherNode(err):
			outcome.conflicts++
			outcome.failed.Insert(node.Name)
			log.Info("Node write rejected by a concurrent change, deciding again", "node", node.Name, "starting", len(change.started) > 0)
			if len(change.started) > 0 {
				r.event(regarding, node, corev1.EventTypeWarning, EventStartConflict,
					"Start on node %s rejected: the node changed concurrently", node.Name)
			}
			continue
		default:
			outcome.failed.Insert(node.Name)
			outcome.errs = append(outcome.errs, fmt.Errorf("writing the conditions of node %s: %w", node.Name, err))
			continue
		}
		for i, kind := range change.released {
			outcome.released = append(outcome.released, node.Name)
			log.Info("Node released", "node", node.Name, "conditionType", kind, "reason", change.reasons[i])
			r.event(regarding, node, corev1.EventTypeNormal, EventMaintenanceReleased,
				"Node %s released from %s: %s", node.Name, kind, change.reasons[i])
		}
		for kind := range change.started {
			outcome.started = append(outcome.started, node.Name)
			log.Info("Maintenance started", "node", node.Name, "conditionType", kind)
			r.event(regarding, node, corev1.EventTypeNormal, EventMaintenanceStarted,
				"Maintenance %s started on node %s", kind, node.Name)
		}
	}
	return outcome
}

// event emits an event on the window, related to a node.
func (r *Reconciler) event(regarding *kumacov1alpha1.MaintenanceWindow, node *corev1.Node, eventType, reason, format string, args ...any) {
	if regarding == nil {
		return
	}
	// The cache drops the type of the objects it holds.
	object := regarding.DeepCopy()
	object.SetGroupVersionKind(kumacov1alpha1.SchemeGroupVersion.WithKind("MaintenanceWindow"))
	var related runtime.Object
	if node != nil {
		reference := &corev1.Node{}
		reference.Name, reference.UID = node.Name, node.UID
		reference.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("Node"))
		related = reference
	}
	r.recorder.Eventf(object, related, eventType, reason, reason, format, args...)
}

// reportFailures emits one TriggerFailed event per expression that starts to
// fail, whatever the number of nodes it fails for: events follow changes, not
// the number of nodes.
func (r *Reconciler) reportFailures(view *view, failures []maintenance.TriggerFailure) {
	failing := sets.New[string]()
	for _, failure := range failures {
		if failing.Has(failure.Expression) {
			continue
		}
		failing.Insert(failure.Expression)
		if !r.failing[view.object.Name].Has(failure.Expression) {
			r.event(view.object, nil, corev1.EventTypeWarning, EventTriggerFailed,
				"Trigger %q failed, first on node %s: %v", failure.Expression, failure.Node, failure.Err)
		}
	}
	r.failing[view.object.Name] = failing
}

// enqueueOthers enqueues the other windows selecting the nodes released or
// started, so that a trigger reading the conditions kumaco writes is
// evaluated again: these writes requeue nothing through node events.
func (r *Reconciler) enqueueOthers(ctx context.Context, name string, view *view, outcome *outcome) {
	written := sets.New(outcome.started...).Insert(outcome.released...)
	if written.Len() == 0 {
		return
	}
	for _, window := range view.windows {
		if window.Name == name || !selectsAny(window.Selector, view.nodes, written) {
			continue
		}
		object := &kumacov1alpha1.MaintenanceWindow{}
		object.Name = window.Name
		select {
		case r.enqueue <- event.GenericEvent{Object: object}:
		case <-ctx.Done():
			return
		}
	}
}

func selectsAny(selector labels.Selector, nodes []*corev1.Node, names sets.Set[string]) bool {
	for _, node := range nodes {
		if names.Has(node.Name) && selector.Matches(labels.Set(node.Labels)) {
			return true
		}
	}
	return false
}
