package controller

import (
	"context"
	"fmt"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlcontroller "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	kumacov1alpha1 "github.com/kubereboot/kumaco/api/v1alpha1"
	"github.com/kubereboot/kumaco/internal/maintenance"
)

// Setup adds the controller to the manager: the reconciler of the windows, the
// startup and periodic sweep, and the metrics. The cache of the manager must
// be restricted with CacheOptions. nodes writes the status of nodes.
func Setup(mgr ctrl.Manager, nodes NodeStatusApplier, options Options) error {
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.HeartbeatInterval <= 0 {
		return fmt.Errorf("the heartbeat interval must be positive, got %s", options.HeartbeatInterval)
	}
	metrics, err := newMetrics(options.Metrics, newNodeCollector(mgr.GetCache(), options.ConditionPrefix, options.Now))
	if err != nil {
		return fmt.Errorf("registering the metrics: %w", err)
	}
	state := newState()
	writer := newNodeWriter(mgr.GetCache(), nodes, metrics)
	enqueue := make(chan event.GenericEvent, 64)
	reconciler := &Reconciler{
		client:        mgr.GetClient(),
		writer:        writer,
		state:         state,
		metrics:       metrics,
		recorder:      mgr.GetEventRecorder(options.ControllerName),
		options:       options,
		enqueue:       enqueue,
		known:         map[string]knownWindow{},
		compiled:      map[types.UID]compiledSpec{},
		failing:       map[string]sets.Set[string]{},
		statusWritten: map[string]time.Time{},
	}

	if err := mgr.Add(&sweeper{reader: mgr.GetCache(), writer: writer, state: state, options: options}); err != nil {
		return err
	}
	if err := mgr.Add(leaderGauge{elected: mgr.Elected(), metrics: metrics}); err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).
		Named(options.ControllerName).
		// A change of status or of metadata changes no decision.
		For(&kumacov1alpha1.MaintenanceWindow{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&corev1.Node{}, nodeHandler(mgr.GetCache(), options.ConditionPrefix)).
		WatchesRawSource(source.Channel(enqueue, &handler.EnqueueRequestForObject{})).
		// Windows are reconciled one at a time.
		WithOptions(ctrlcontroller.Options{MaxConcurrentReconciles: 1}).
		Complete(reconciler)
}

// leaderGauge sets kumaco_leader when this replica becomes the leader. A
// replica that loses leadership exits.
type leaderGauge struct {
	elected <-chan struct{}
	metrics *metrics
}

var _ manager.LeaderElectionRunnable = leaderGauge{}

// NeedLeaderElection is false: the gauge exists on every replica.
func (leaderGauge) NeedLeaderElection() bool { return false }

func (g leaderGauge) Start(ctx context.Context) error {
	g.metrics.leader.Set(0)
	select {
	case <-g.elected:
		g.metrics.leader.Set(1)
		logf.FromContext(ctx).Info("Leading")
		<-ctx.Done()
		g.metrics.leader.Set(0)
	case <-ctx.Done():
	}
	return nil
}

// nodeHandler enqueues, for a node event, the windows whose nodeSelector
// matches the node before or after the change: a window that does not select
// it cannot be affected. Changes that can change no decision enqueue nothing
// (heartbeats, and the conditions kumaco writes itself).
func nodeHandler(reader client.Reader, prefix string) handler.EventHandler {
	enqueue := func(ctx context.Context, queue workqueue.TypedRateLimitingInterface[reconcile.Request], nodes ...client.Object) {
		windows, err := windowsSelecting(ctx, reader, nodes)
		if err != nil {
			logf.FromContext(ctx).Error(err, "Listing the windows for a node event")
			return
		}
		for _, window := range windows {
			queue.Add(reconcile.Request{NamespacedName: types.NamespacedName{Name: window}})
		}
	}
	return handler.Funcs{
		CreateFunc: func(ctx context.Context, e event.CreateEvent, queue workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			enqueue(ctx, queue, e.Object)
		},
		DeleteFunc: func(ctx context.Context, e event.DeleteEvent, queue workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			enqueue(ctx, queue, e.Object)
		},
		UpdateFunc: func(ctx context.Context, e event.UpdateEvent, queue workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			before, after := e.ObjectOld.(*corev1.Node), e.ObjectNew.(*corev1.Node)
			if maintenance.RelevantChange(before, after, prefix) {
				enqueue(ctx, queue, before, after)
			}
		},
	}
}

// windowsSelecting returns the names of the windows whose nodeSelector matches
// one of the nodes.
func windowsSelecting(ctx context.Context, reader client.Reader, nodes []client.Object) ([]string, error) {
	var windows kumacov1alpha1.MaintenanceWindowList
	if err := reader.List(ctx, &windows); err != nil {
		return nil, err
	}
	var names []string
	for _, window := range windows.Items {
		selector, err := metav1.LabelSelectorAsSelector(&window.Spec.NodeSelector)
		if err != nil {
			continue // an Invalid window selects nothing
		}
		selects := func(node client.Object) bool { return selector.Matches(labels.Set(node.GetLabels())) }
		if slices.ContainsFunc(nodes, selects) {
			names = append(names, window.Name)
		}
	}
	return names, nil
}
