package controller

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"

	kumacov1alpha1 "github.com/kubereboot/kumaco/api/v1alpha1"
	"github.com/kubereboot/kumaco/internal/maintenance"
)

const (
	unitPrefix   = kumacov1alpha1.DefaultConditionPrefix
	unitReboot   = "kumaco.kured.dev/RebootInProgress"
	unitRequired = "detectors.kured.dev/RebootRequired"

	needsReboot = `node.status.conditions.exists(c, c.type == 'detectors.kured.dev/RebootRequired' && c.status == 'True')`
)

// laggingCluster stands for the API server and the informer cache, with the
// lag between them under the control of the test: the controller reads the
// cache, its node writes go to the API server, and the cache only shows them
// when the test says so.
type laggingCluster struct {
	t     *testing.T
	cache client.Client
	// written are the node writes that the cache does not show yet.
	written []*corev1.Node
	// applied counts the node writes.
	applied int
}

func (c *laggingCluster) ApplyStatus(ctx context.Context, apply *corev1ac.NodeApplyConfiguration, _ metav1.ApplyOptions) (*corev1.Node, error) {
	var node corev1.Node
	if err := c.cache.Get(ctx, client.ObjectKey{Name: *apply.Name}, &node); err != nil {
		return nil, err
	}
	var kept []corev1.NodeCondition
	for _, condition := range node.Status.Conditions {
		if !maintenance.Owned(condition.Type, unitPrefix) {
			kept = append(kept, condition)
		}
	}
	for _, condition := range apply.Status.Conditions {
		kept = append(kept, corev1.NodeCondition{
			Type: *condition.Type, Status: *condition.Status, Reason: *condition.Reason, Message: *condition.Message,
			LastTransitionTime: *condition.LastTransitionTime, LastHeartbeatTime: *condition.LastHeartbeatTime,
		})
	}
	node.Status.Conditions = kept
	// The version that the fake cache gives to the node when it shows the write.
	version, err := strconv.Atoi(node.ResourceVersion)
	if err != nil {
		return nil, err
	}
	node.ResourceVersion = strconv.Itoa(version + 1)
	c.written = append(c.written, &node)
	c.applied++
	return &node, nil
}

// catchUp makes the cache show the node writes.
func (c *laggingCluster) catchUp() {
	c.t.Helper()
	for _, node := range c.written {
		current := &corev1.Node{}
		if err := c.cache.Get(c.t.Context(), client.ObjectKeyFromObject(node), current); err != nil {
			c.t.Fatal(err)
		}
		current.Status = node.Status
		if err := c.cache.Status().Update(c.t.Context(), current); err != nil {
			c.t.Fatal(err)
		}
	}
	c.written = nil
}

// add adds a node that needs a reboot.
func (c *laggingCluster) add(name string) {
	c.t.Helper()
	node := &corev1.Node{}
	node.Name = name
	node.CreationTimestamp = metav1.NewTime(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	node.Status.Conditions = []corev1.NodeCondition{{Type: unitRequired, Status: corev1.ConditionTrue}}
	if err := c.cache.Create(c.t.Context(), node); err != nil {
		c.t.Fatal(err)
	}
}

func (c *laggingCluster) under(kind string) []string {
	c.t.Helper()
	var nodes corev1.NodeList
	if err := c.cache.List(c.t.Context(), &nodes); err != nil {
		c.t.Fatal(err)
	}
	var result []string
	for _, node := range nodes.Items {
		if _, ok := maintenance.Under(&node, kind); ok {
			result = append(result, node.Name)
		}
	}
	return result
}

// newUnitReconciler returns a reconciler reading the cache of the cluster,
// after its startup, and the recorder of its events.
func newUnitReconciler(t *testing.T, windows ...*kumacov1alpha1.MaintenanceWindow) (*Reconciler, *laggingCluster, *events.FakeRecorder) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := kumacov1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	builder := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&kumacov1alpha1.MaintenanceWindow{}, &corev1.Node{})
	for _, window := range windows {
		builder.WithObjects(window)
	}
	cluster := &laggingCluster{t: t, cache: builder.Build()}
	options := Options{ControllerName: "kumaco", ConditionPrefix: unitPrefix, HeartbeatInterval: time.Hour, Now: time.Now}
	metrics, err := newMetrics(prometheus.NewRegistry(), newNodeCollector(cluster.cache, unitPrefix, time.Now))
	if err != nil {
		t.Fatal(err)
	}
	recorder := events.NewFakeRecorder(100)
	state := newState()
	state.startupSwept(nil)
	return &Reconciler{
		client:        cluster.cache,
		writer:        newNodeWriter(cluster.cache, cluster, metrics),
		state:         state,
		metrics:       metrics,
		recorder:      recorder,
		options:       options,
		enqueue:       make(chan event.GenericEvent, 100),
		known:         map[string]knownWindow{},
		compiled:      map[types.UID]compiledSpec{},
		failing:       map[string]sets.Set[string]{},
		statusWritten: map[string]time.Time{},
	}, cluster, recorder
}

// unitWindow returns a window that is always in progress.
func unitWindow(name string, concurrency int32, startTriggers ...string) *kumacov1alpha1.MaintenanceWindow {
	window := &kumacov1alpha1.MaintenanceWindow{}
	window.Name, window.UID, window.Generation = name, types.UID(name), 1
	window.Spec = kumacov1alpha1.MaintenanceWindowSpec{
		StartTime:     "0 * * * * ? *",
		Duration:      metav1.Duration{Duration: time.Hour},
		ConditionType: unitReboot,
		StartTriggers: startTriggers,
		Concurrency:   &concurrency,
	}
	return window
}

func reconcileWindow(t *testing.T, reconciler *Reconciler, name string) ctrl.Result {
	t.Helper()
	result, err := reconciler.Reconcile(t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{Name: name}})
	if err != nil {
		t.Fatalf("Reconcile(%s): %v", name, err)
	}
	return result
}

// The cache can lag behind the controller's own writes. Maintenance started on
// node b, and the cache does not show it yet, when node a becomes a candidate
// that comes before b: counting from that cache would start a too, and exceed
// the concurrency of 1. The window is not reconciled until the cache shows the
// write.
//
// README: #reading-its-own-writes, #concurrency-conflicts
func TestAReconcileWaitsUntilTheCacheShowsItsWrites(t *testing.T) {
	reconciler, cluster, _ := newUnitReconciler(t, unitWindow("reboots", 1, needsReboot))
	cluster.add("b")

	reconcileWindow(t, reconciler, "reboots")
	if cluster.applied != 1 {
		t.Fatalf("%d node writes, want the start on b", cluster.applied)
	}

	cluster.add("a")
	for range 3 {
		result := reconcileWindow(t, reconciler, "reboots")
		if cluster.applied != 1 || result.RequeueAfter != cacheLagRetry {
			t.Fatalf("with a cache that does not show the start on b: %d node writes, requeue after %s; want no new write, and a retry after %s",
				cluster.applied, result.RequeueAfter, cacheLagRetry)
		}
	}

	cluster.catchUp()
	result := reconcileWindow(t, reconciler, "reboots")
	if under := cluster.under(unitReboot); cluster.applied != 1 || fmt.Sprint(under) != "[b]" || result.RequeueAfter == cacheLagRetry {
		t.Errorf("once the cache shows the start: %d node writes, under maintenance %v, requeue after %s; want a reconcile that keeps only b",
			cluster.applied, under, result.RequeueAfter)
	}
	if waiting := reconciler.state.notStartedOn("a"); len(waiting) != 1 || waiting[0].Situation != kumacov1alpha1.SituationMaxConcurrencyReached {
		t.Errorf("a is %v, want it waiting for the slot of b", waiting)
	}
}

// The sweep computes the complete list of conditions from the cache. Computed
// from a cache that does not show a start yet, that list would remove the
// record: the sweep leaves such a node alone.
//
// README: #reading-its-own-writes, #one-function-one-write-per-node
func TestTheSweepLeavesANodeAloneUntilTheCacheShowsItsLastWrite(t *testing.T) {
	reconciler, cluster, _ := newUnitReconciler(t, unitWindow("reboots", 1, needsReboot))
	cluster.add("a")
	reconcileWindow(t, reconciler, "reboots")
	sweep := func() (bool, error) {
		return reconciler.writer.sweep(t.Context(), "a", func() maintenance.Desired {
			return maintenance.Desired{Prefix: unitPrefix, WindowTypes: sets.New(unitReboot), Now: time.Now()}
		})
	}

	skipped, err := sweep()
	if err != nil || !skipped || cluster.applied != 1 {
		t.Fatalf("sweep with a lagging cache: skipped %v, error %v, %d node writes; want the node skipped", skipped, err, cluster.applied)
	}

	cluster.catchUp()
	skipped, err = sweep()
	if err != nil || skipped || cluster.applied != 1 {
		t.Errorf("sweep once the cache shows the start: skipped %v, error %v, %d node writes; want nothing to write", skipped, err, cluster.applied)
	}
}

// A record left over from before the downtime must not look fresh before it
// has been checked: heartbeats start once every window that existed at startup
// was reconciled.
//
// README: #startup-and-periodic-sweep, #heartbeat-contract
func TestHeartbeatsWaitForTheFirstReconcileOfEveryWindow(t *testing.T) {
	state := newState()
	select {
	case <-state.swept:
		t.Fatal("reconciles may run before the startup sweep")
	default:
	}

	state.startupSwept([]string{"weekdays", "weekend"})
	select {
	case <-state.swept:
	default:
		t.Fatal("reconciles may not run after the startup sweep")
	}
	state.reconciled("created-later")
	state.reconciled("weekdays")
	if state.heartbeatsStarted() {
		t.Fatal("heartbeats started before the weekend window was reconciled")
	}
	state.reconciled("weekend")
	if !state.heartbeatsStarted() {
		t.Fatal("heartbeats did not start although every window was reconciled")
	}

	none := newState()
	none.startupSwept(nil)
	if !none.heartbeatsStarted() {
		t.Error("without windows, heartbeats did not start after the startup sweep")
	}
}

// A reconcile refreshes the heartbeats that are due in the same write as its
// decision, but not before heartbeats have started.
//
// README: #heartbeat-contract, #startup-and-periodic-sweep
func TestAReconcileOnlyRefreshesHeartbeatsOnceTheyStarted(t *testing.T) {
	stale := metav1.NewTime(time.Now().Add(-3 * time.Hour).Truncate(time.Second))
	for _, started := range []bool{false, true} {
		reconciler, cluster, _ := newUnitReconciler(t, unitWindow("reboots", 1, needsReboot))
		if !started {
			reconciler.state = newState()
			reconciler.state.startupSwept([]string{"reboots", "another-window"})
		}
		cluster.add("a")
		node := &corev1.Node{}
		if err := cluster.cache.Get(t.Context(), client.ObjectKey{Name: "a"}, node); err != nil {
			t.Fatal(err)
		}
		node.Status.Conditions = append(node.Status.Conditions, corev1.NodeCondition{
			Type: corev1.NodeMaintenanceInProgress, Status: corev1.ConditionFalse, LastHeartbeatTime: stale, LastTransitionTime: stale,
		})
		if err := cluster.cache.Status().Update(t.Context(), node); err != nil {
			t.Fatal(err)
		}

		reconcileWindow(t, reconciler, "reboots")
		cluster.catchUp()

		if err := cluster.cache.Get(t.Context(), client.ObjectKey{Name: "a"}, node); err != nil {
			t.Fatal(err)
		}
		heartbeat := maintenance.Condition(node, string(corev1.NodeMaintenanceInProgress)).LastHeartbeatTime
		if refreshed := !heartbeat.Equal(&stale); refreshed != started {
			t.Errorf("heartbeats started: %v; the stale heartbeat was refreshed: %v", started, refreshed)
		}
	}
}

// Events follow changes, not the number of nodes: an expression that fails
// emits one TriggerFailed event, whatever the number of nodes and reconciles.
//
// README: #debugging-with-kumaco-events, #cel-triggers
func TestTriggerFailedIsEmittedOncePerExpression(t *testing.T) {
	const failing = `node.spec.taints.size() > 0`
	reconciler, cluster, recorder := newUnitReconciler(t, unitWindow("reboots", 1, failing))
	for i := range 5 {
		cluster.add(fmt.Sprintf("node-%d", i))
	}

	reconcileWindow(t, reconciler, "reboots")
	reconcileWindow(t, reconciler, "reboots")

	if cluster.applied != 0 {
		t.Errorf("%d node writes, want none: a trigger that fails starts nothing", cluster.applied)
	}
	var got []string
	for len(recorder.Events) > 0 {
		got = append(got, <-recorder.Events)
	}
	if len(got) != 1 || !strings.Contains(got[0], "Warning "+EventTriggerFailed) || !strings.Contains(got[0], failing) {
		t.Errorf("events = %q, want one %s event naming the expression", got, EventTriggerFailed)
	}
}

// The status is written at once when the state changes, and otherwise at most
// once per statusInterval; a write that is put off is retried.
//
// README: #the-kumacokureddev-maintenancewindow-crd
func TestTheStatusIsWrittenAtMostOncePerInterval(t *testing.T) {
	reconciler, cluster, _ := newUnitReconciler(t, unitWindow("reboots", 0))
	status := func() kumacov1alpha1.MaintenanceWindowStatus {
		window := &kumacov1alpha1.MaintenanceWindow{}
		if err := cluster.cache.Get(t.Context(), client.ObjectKey{Name: "reboots"}, window); err != nil {
			t.Fatal(err)
		}
		return window.Status
	}

	reconcileWindow(t, reconciler, "reboots")
	if got := status(); got.State != kumacov1alpha1.StateInProgress || got.ObservedGeneration != 1 || got.MatchingNodes != 0 {
		t.Fatalf("status = %+v, want the first status written at once", got)
	}

	cluster.add("a")
	result := reconcileWindow(t, reconciler, "reboots")
	if got := status(); got.MatchingNodes != 0 || result.RequeueAfter <= 0 || result.RequeueAfter > statusInterval {
		t.Fatalf("matchingNodes = %d, requeue after %s; want the write put off, and retried within %s",
			got.MatchingNodes, result.RequeueAfter, statusInterval)
	}

	reconciler.statusWritten["reboots"] = time.Now().Add(-statusInterval)
	reconcileWindow(t, reconciler, "reboots")
	got := status()
	wantCounts := map[string]int32{"UnderMaintenance": 0, "UnmetTrigger": 0, "MaxConcurrencyReached": 1}
	if got.MatchingNodes != 1 || fmt.Sprint(got.NodeCounts) != fmt.Sprint(wantCounts) {
		t.Errorf("status after the interval = %+v, want 1 matching node and the counts %v", got, wantCounts)
	}

	// A change of state is not put off.
	window := &kumacov1alpha1.MaintenanceWindow{}
	if err := cluster.cache.Get(t.Context(), client.ObjectKey{Name: "reboots"}, window); err != nil {
		t.Fatal(err)
	}
	window.Spec.StartTriggers = []string{"node."}
	window.Generation = 2
	if err := cluster.cache.Update(t.Context(), window); err != nil {
		t.Fatal(err)
	}
	reconcileWindow(t, reconciler, "reboots")
	if got := status(); got.State != kumacov1alpha1.StateInvalid || !strings.Contains(got.Message, "startTriggers[0]") ||
		got.ObservedGeneration != 2 || got.NodeCounts != nil {
		t.Errorf("status = %+v, want Invalid at once, with the reason in its message", got)
	}
}

// The periodic sweep spreads its writes evenly over the heartbeat interval,
// and refreshes a heartbeat on each of its visits.
//
// README: #heartbeat-contract
func TestTheSweepSpreadsItsWritesOverTheInterval(t *testing.T) {
	const nodes, interval = 10_000, 5 * time.Minute
	if first := slot(0, nodes, interval); first != 0 {
		t.Errorf("the first node is written after %s, want at once", first)
	}
	step := slot(1, nodes, interval)
	for _, index := range []int{1, 2, 5000, nodes - 1} {
		if got := slot(index, nodes, interval); got != time.Duration(index)*step {
			t.Errorf("node %d is written after %s, want %s: evenly spread", index, got, time.Duration(index)*step)
		}
	}
	if last := slot(nodes-1, nodes, interval); last >= interval || last < interval-step-time.Millisecond {
		t.Errorf("the last node is written after %s, want just before the end of the interval (%s)", last, interval)
	}
	// One write per managed node per interval: about 33 per second here.
	if perSecond := float64(time.Second) / float64(step); perSecond < 33 || perSecond > 34 {
		t.Errorf("%.1f writes per second, want about 33", perSecond)
	}
	if due := heartbeatDue(interval); due >= interval || due < interval/2 {
		t.Errorf("a heartbeat is due after %s, want a little less than the interval (%s)", due, interval)
	}
}
