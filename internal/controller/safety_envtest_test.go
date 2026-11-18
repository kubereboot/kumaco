//go:build envtest

package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"

	kumacov1alpha1 "github.com/kubereboot/kumaco/api/v1alpha1"
	"github.com/kubereboot/kumaco/internal/maintenance"
)

// hookedApplier runs a function before each node write of the controller.
type hookedApplier struct {
	NodeStatusApplier
	before func(node *corev1ac.NodeApplyConfiguration) error
}

func (a hookedApplier) ApplyStatus(ctx context.Context, node *corev1ac.NodeApplyConfiguration, options metav1.ApplyOptions) (*corev1.Node, error) {
	if err := a.before(node); err != nil {
		return nil, err
	}
	return a.NodeStatusApplier.ApplyStatus(ctx, node, options)
}

// starts says whether a node write starts that kind of maintenance.
func starts(node *corev1ac.NodeApplyConfiguration, kind string) bool {
	if node.ResourceVersion == nil {
		return false
	}
	for _, condition := range node.Status.Conditions {
		if string(*condition.Type) == kind {
			return true
		}
	}
	return false
}

// Two windows of the same kind roll over the nodes, while a stand-in for Kured
// "reboots" each node under maintenance at once. The controller counts nodes
// from a cache that lags behind its own writes: without reading them back, it
// would start more nodes than the concurrency. Every observed state of the
// cluster is checked, and each node is rebooted exactly once.
//
// README: #reading-its-own-writes, #concurrency-conflicts, #reconciling-a-window
func TestConcurrencyIsNeverExceeded(t *testing.T) {
	const nodes, concurrency = 12, 2
	h := newHarness(t)
	for i := range nodes {
		h.node(fmt.Sprintf("n%02d", i), rebootable...)
	}
	h.kuredWindow("weekdays", concurrency)
	h.kuredWindow("weekend", concurrency)

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	watcher, err := clientset.CoreV1().Nodes().Watch(ctx, metav1.ListOptions{LabelSelector: scopeLabel + "=" + h.scope})
	if err != nil {
		t.Fatal(err)
	}
	defer watcher.Stop()

	h.start(nil)

	reboots, most := h.rollOut(ctx, watcher, nodes, concurrency)

	for name, count := range reboots {
		if count != 1 {
			t.Errorf("node %s was put under maintenance %d times, want once", name, count)
		}
	}
	if most != concurrency {
		t.Errorf("at most %d nodes were under maintenance at the same time, want the %d slots to be used", most, concurrency)
	}
}

// rollOut follows the nodes until all of them were rebooted and released. It
// stands for Kured: a node under maintenance no longer requires a reboot. It
// fails the test when more nodes than the concurrency are under maintenance,
// and returns the reboots of each node, and the most nodes seen under
// maintenance at the same time.
func (h *harness) rollOut(ctx context.Context, watcher watch.Interface, nodes, concurrency int) (map[string]int, int) {
	h.t.Helper()
	under := map[string]bool{}
	reboots := map[string]int{}
	most := 0
	for len(reboots) < nodes || len(under) > 0 {
		var event watch.Event
		select {
		case event = <-watcher.ResultChan():
		case <-ctx.Done():
			h.t.Fatalf("timed out: rebooted %v, under maintenance %v", reboots, under)
		}
		node, isNode := event.Object.(*corev1.Node)
		if !isNode {
			h.t.Fatalf("watch event %v: %v", event.Type, event.Object)
		}
		name := node.Labels["name"]
		_, isUnder := maintenance.Under(node, reboot)
		switch {
		case isUnder && !under[name]:
			under[name] = true
			reboots[name]++
			h.report(name, required+"=False")
		case !isUnder:
			delete(under, name)
		}
		most = max(most, len(under))
		if len(under) > concurrency {
			h.t.Fatalf("%d nodes under maintenance at the same time, want at most %d: %v", len(under), concurrency, under)
		}
	}
	return reboots, most
}

// A concurrent change to the node rejects the write that starts maintenance.
// Nothing is recorded, and the decision is taken again from the fresh node:
// here, the node no longer needs a reboot, so maintenance never starts on it.
//
// README: #one-function-one-write-per-node, #reconciling-a-window, #debugging-with-kumaco-events
func TestARejectedStartIsDecidedAgainFromTheFreshNode(t *testing.T) {
	h := newHarness(t)
	h.node("a", rebootable...)
	h.kuredWindow("reboots", 1)
	var once sync.Once
	controller := h.start(func(options *controllerOptions) {
		options.applier = func(applier NodeStatusApplier) NodeStatusApplier {
			return hookedApplier{NodeStatusApplier: applier, before: func(node *corev1ac.NodeApplyConfiguration) error {
				if starts(node, reboot) {
					// The reboot reporter changes the node just before the write.
					once.Do(func() { h.report("a", required+"=False") })
				}
				return nil
			}}
		}
	})

	h.wantEvent("reboots", "Warning StartConflict", h.nodeName("a"))
	h.wantConditions("a", generic+"=False")
	h.consistently(time.Second, "a staying out of maintenance", func() (bool, string) {
		return !h.isUnder("a", reboot), fmt.Sprint(h.conditions("a"))
	})
	if conflicts, _ := controller.metric(t, "kumaco_node_write_total", "result=conflict"); conflicts != 1 {
		t.Errorf("kumaco_node_write_total{result=conflict} = %v, want 1", conflicts)
	}
	if events := h.events("reboots"); slices.ContainsFunc(events, func(event string) bool { return strings.Contains(event, EventMaintenanceStarted) }) {
		t.Errorf("events = %v, want no MaintenanceStarted: a rejected start is not a start", events)
	}
}

// A node write that fails leaves no record: the next reconcile decides again.
//
// README: #the-node-is-the-record, #metrics
func TestAFailedNodeWriteIsRetried(t *testing.T) {
	h := newHarness(t)
	h.node("a", rebootable...)
	h.kuredWindow("reboots", 1)
	var failures atomic.Int32
	controller := h.start(func(options *controllerOptions) {
		options.applier = func(applier NodeStatusApplier) NodeStatusApplier {
			return hookedApplier{NodeStatusApplier: applier, before: func(node *corev1ac.NodeApplyConfiguration) error {
				if starts(node, reboot) && failures.Add(1) <= 2 {
					return apierrors.NewInternalError(errors.New("the API server is unavailable"))
				}
				return nil
			}}
		}
	})

	h.wantConditions("a", reboot+"=True", generic+"=True")
	for _, labels := range []string{"kumaco_node_write_total result=error", "kumaco_reconcile_total result=error"} {
		name, label, _ := strings.Cut(labels, " ")
		if got, _ := controller.metric(t, name, label); got != 2 {
			t.Errorf("%s{%s} = %v, want 2", name, label, got)
		}
	}
}

// kumacoWrote sets conditions on a node as a previous run of kumaco did.
func (h *harness) kumacoWrote(node string, at time.Time, conditions ...string) {
	h.t.Helper()
	status := corev1ac.NodeStatus()
	for _, pair := range conditions {
		conditionType, conditionStatus, _ := strings.Cut(pair, "=")
		status.WithConditions(corev1ac.NodeCondition().
			WithType(corev1.NodeConditionType(conditionType)).WithStatus(corev1.ConditionStatus(conditionStatus)).
			WithReason("Test").WithLastTransitionTime(metav1.NewTime(at)).WithLastHeartbeatTime(metav1.NewTime(at)))
	}
	_, err := clientset.CoreV1().Nodes().ApplyStatus(context.Background(), corev1ac.Node(h.nodeName(node)).WithStatus(status),
		metav1.ApplyOptions{FieldManager: FieldManager, Force: true})
	if err != nil {
		h.t.Fatal(err)
	}
}

// After a restart, there is nothing to rebuild: the records are on the nodes.
// The startup sweep removes the records whose type no window has anymore, and
// sets MaintenanceInProgress where it is missing or outdated; the first
// reconciles release what is no longer justified, and keep the rest untouched.
//
// README: #startup-and-periodic-sweep, #the-node-is-the-record, #gotchas
func TestStartupKeepsOnlyWhatIsJustified(t *testing.T) {
	h := newHarness(t)
	before := time.Now().Add(-10 * time.Minute).Truncate(time.Second)
	// Its firmware window was deleted while kumaco was down.
	h.node("leftover", rebootable...)
	h.kumacoWrote("leftover", before, firmware+"=True", generic+"=True")
	// Its reboot is still in progress.
	h.node("kept", rebootable...)
	h.kumacoWrote("kept", before, reboot+"=True", generic+"=True")
	// Its reboot finished while kumaco was down.
	h.node("done", ready+"=True")
	h.kumacoWrote("done", before, reboot+"=True", generic+"=True")
	// kumaco never saw it.
	h.node("new")
	// Its MaintenanceInProgress is outdated.
	h.node("outdated", rebootable...)
	h.kumacoWrote("outdated", before, reboot+"=True", generic+"=False")
	h.kuredWindow("reboots", 2)

	h.start(nil)

	h.wantConditions("leftover", generic+"=False")
	h.wantConditions("kept", reboot+"=True", generic+"=True")
	h.wantConditions("done", generic+"=False")
	h.wantConditions("new", generic+"=False")
	h.wantConditions("outdated", reboot+"=True", generic+"=True")
	h.wantEvent("reboots", "Normal MaintenanceReleased", h.nodeName("done"))

	// The record that is kept is untouched: it is the same maintenance.
	kept := h.condition("kept", reboot)
	if !kept.LastTransitionTime.Time.Equal(before) {
		t.Errorf("the record of kept started at %s, want %s: it was not started again", kept.LastTransitionTime, before)
	}
	// leftover holds no slot: its firmware record is gone, and both reboot
	// slots are used by the nodes that were already under maintenance.
	h.consistently(500*time.Millisecond, "the two slots staying with kept and outdated", func() (bool, string) {
		under := h.under(reboot)
		return slices.Equal(under, []string{"kept", "outdated"}), fmt.Sprint(under)
	})
}

// Replicas of the same controller share a Lease: only the leader writes. When
// it stops, another replica takes over from the records on the nodes, without
// starting maintenance again or releasing anything.
//
// README: #replicas-of-the-same-controller, #metrics
func TestALeaderHandoverNeedsNothingFromThePreviousLeader(t *testing.T) {
	h := newHarness(t)
	h.node("a", rebootable...)
	h.node("b", rebootable...)
	h.kuredWindow("reboots", 1)
	elect := func(options *controllerOptions) { options.leaderElection = true }

	first := h.start(elect)
	h.wantConditions("a", reboot+"=True", generic+"=True")
	started := h.condition("a", reboot).LastTransitionTime

	second := h.start(elect)
	h.consistently(time.Second, "the second replica staying a follower", func() (bool, string) {
		leader, _ := second.metric(t, "kumaco_leader")
		_, wrote := second.metric(t, "kumaco_node_write_total")
		return leader == 0 && !wrote, fmt.Sprintf("kumaco_leader=%v, wrote nodes=%v", leader, wrote)
	})
	if leader, _ := first.metric(t, "kumaco_leader"); leader != 1 {
		t.Errorf("kumaco_leader of the first replica = %v, want 1", leader)
	}

	first.stop()
	h.eventually("the second replica to lead", func() (bool, string) {
		leader, _ := second.metric(t, "kumaco_leader")
		return leader == 1, fmt.Sprint(leader)
	})
	// The new leader finishes the rollout: a is released, then b starts.
	h.report("a", required+"=False")
	h.wantConditions("a", generic+"=False")
	h.wantConditions("b", reboot+"=True", generic+"=True")
	if events := h.events("reboots"); len(events) == 0 {
		t.Error("no event on the window")
	}
	if got := h.condition("b", reboot).LastTransitionTime; got.Before(&started) {
		t.Errorf("b started at %s, before a (%s)", got, started)
	}
}

// The controller refreshes lastHeartbeatTime on every condition it owns, and
// only on those. Neither its own writes nor heartbeat-only changes of other
// writers requeue windows.
//
// README: #heartbeat-contract, #what-does-it-reconcile-and-when, #startup-and-periodic-sweep
func TestHeartbeats(t *testing.T) {
	h := newHarness(t)
	h.node("a", rebootable...)
	h.node("b", ready+"=True")
	// A daily window of 24 hours: no schedule boundary during the test.
	h.window("reboots", func(spec *kumacov1alpha1.MaintenanceWindowSpec) {
		spec.StartTime, spec.Duration.Duration = "0 0 0 * * ? *", 24*time.Hour
		spec.StartTriggers, spec.StopTriggers = kuredStart, kuredStop
	})
	controller := h.start(func(options *controllerOptions) { options.HeartbeatInterval = time.Second })
	h.wantConditions("a", reboot+"=True", generic+"=True")
	h.wantConditions("b", generic+"=False")

	type beat struct{ node, conditionType string }
	owned := []beat{{"a", reboot}, {"a", generic}, {"b", generic}}
	first := map[beat]corev1.NodeCondition{}
	for _, condition := range owned {
		first[condition] = *h.condition(condition.node, condition.conditionType)
	}
	reporterBefore := *h.condition("a", ready)
	reconcilesBefore, _ := controller.metric(t, "kumaco_reconcile_total", "result=ok")

	// Another writer only refreshes its heartbeat, as the kubelet does.
	h.eventually("three heartbeats on every owned condition", func() (bool, string) {
		h.heartbeat("b", ready)
		var observed []string
		enough := true
		for _, condition := range owned {
			now := h.condition(condition.node, condition.conditionType)
			age := now.LastHeartbeatTime.Sub(first[condition].LastHeartbeatTime.Time)
			observed = append(observed, fmt.Sprintf("%s/%s +%s", condition.node, condition.conditionType, age))
			enough = enough && age >= 3*time.Second
		}
		return enough, strings.Join(observed, ", ")
	})

	for _, condition := range owned {
		now := h.condition(condition.node, condition.conditionType)
		if !now.LastTransitionTime.Time.Equal(first[condition].LastTransitionTime.Time) || now.Status != first[condition].Status {
			t.Errorf("%s on %s: transition %s, status %s; want them unchanged by heartbeats", condition.conditionType, condition.node, now.LastTransitionTime, now.Status)
		}
	}
	if now := h.condition("a", ready); !now.LastHeartbeatTime.Equal(&reporterBefore.LastHeartbeatTime) {
		t.Errorf("the heartbeat of Ready on a changed from %s to %s: kumaco does not own it", reporterBefore.LastHeartbeatTime, now.LastHeartbeatTime)
	}
	if reconciles, _ := controller.metric(t, "kumaco_reconcile_total", "result=ok"); reconciles != reconcilesBefore {
		t.Errorf("%v reconciles during the heartbeats, want none", reconciles-reconcilesBefore)
	}
	if age, ok := controller.metric(t, "kumaco_oldest_heartbeat_age_seconds"); !ok || age > 3 {
		t.Errorf("kumaco_oldest_heartbeat_age_seconds = %v, want at most a few heartbeat intervals", age)
	}
}

// A node is deleted and created again with the same name between a decision
// and its write. The write carries the UID of the old node: the API server
// rejects it, and the new node does not inherit conditions decided for the old
// one.
//
// README: #the-node-is-the-record, #one-function-one-write-per-node
func TestAWriteDecidedForADeletedNodeDoesNotReachItsReplacement(t *testing.T) {
	h := newHarness(t)
	old := h.node("a", rebootable...)
	h.node("b", rebootable...)
	h.window("reboots", func(spec *kumacov1alpha1.MaintenanceWindowSpec) {
		spec.StartTriggers, spec.Concurrency = kuredStart, new(int32(2))
	})
	var once sync.Once
	controller := h.start(func(options *controllerOptions) {
		options.HeartbeatInterval = time.Second
		options.applier = func(applier NodeStatusApplier) NodeStatusApplier {
			return hookedApplier{NodeStatusApplier: applier, before: func(node *corev1ac.NodeApplyConfiguration) error {
				if *node.Name == old.Name && starts(node, reboot) {
					once.Do(func() {
						if err := api.Delete(context.Background(), old.DeepCopy()); err != nil {
							t.Error(err)
						}
						h.node("a", ready+"=True")
					})
				}
				return nil
			}}
		}
	})

	h.wantUnder(reboot, "b")
	h.wantConditions("a", generic+"=False")
	if replacement := h.getNode("a"); replacement.UID == old.UID {
		t.Fatal("node a was not created again")
	}
	if conflicts, _ := controller.metric(t, "kumaco_node_write_total", "result=conflict"); conflicts != 1 {
		t.Errorf("kumaco_node_write_total{result=conflict} = %v, want 1: the write decided for the old node", conflicts)
	}
}

// A deleted node takes its records with it: its slot is free. A node recreated
// with the same name has none.
//
// README: #the-node-is-the-record
func TestADeletedNodeTakesItsRecordsWithIt(t *testing.T) {
	h := newHarness(t)
	a := h.node("a", rebootable...)
	h.node("b", rebootable...)
	h.kuredWindow("reboots", 1)
	h.start(func(options *controllerOptions) { options.HeartbeatInterval = time.Second })
	h.wantConditions("a", reboot+"=True", generic+"=True")

	if err := api.Delete(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	h.wantConditions("b", reboot+"=True", generic+"=True")

	h.eventually("node a to be gone", func() (bool, string) { return isGone(a), "still there" })
	h.node("a", ready+"=True")
	h.wantConditions("a", generic+"=False")
	h.wantUnder(reboot, "b")
}
