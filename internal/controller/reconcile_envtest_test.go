//go:build envtest

package controller

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kumacov1alpha1 "github.com/kubereboot/kumaco/api/v1alpha1"
	"github.com/kubereboot/kumaco/internal/maintenance"
)

// The life of a reboot, with the triggers documented for Kured and a
// concurrency of 1: maintenance starts on one node, and the next one waits
// until the first is released.
//
// README: #reconciling-a-window, #what-does-it-look-like-on-my-node, #one-function-one-write-per-node, #the-node-is-the-record, #debugging-with-kumaco-events
func TestMaintenanceStartsOnOneNodeAtATime(t *testing.T) {
	h := newHarness(t)
	h.node("a", rebootable...)
	h.node("b", rebootable...)
	h.node("c", ready+"=True") // needs no reboot
	h.kuredWindow("reboots", 1)
	controller := h.start(func(options *controllerOptions) { options.HeartbeatInterval = time.Second })

	// Maintenance starts on a: the oldest node, then the name.
	h.wantConditions("a", reboot+"=True", generic+"=True")
	h.wantConditions("b", generic+"=False")
	h.wantConditions("c", generic+"=False")
	h.wantEvent("reboots", "Normal MaintenanceStarted", h.nodeName("a"), reboot)

	started := h.condition("a", reboot)
	if started.Reason != maintenance.ReasonMaintenanceRunning || started.Message != "Started by MaintenanceWindow "+h.windowName("reboots") {
		t.Errorf("%s on a: reason %q, message %q", reboot, started.Reason, started.Message)
	}
	inProgress := h.condition("a", generic)
	if inProgress.Reason != maintenance.ReasonMaintenanceRunning || !strings.HasPrefix(inProgress.Message, "In progress: "+reboot) {
		t.Errorf("%s on a: reason %q, message %q", generic, inProgress.Reason, inProgress.Message)
	}
	// The messages of the other nodes say why maintenance did not start. They
	// travel with the next write: the heartbeat.
	for node, situation := range map[string]string{"b": "MaxConcurrencyReached", "c": "UnmetTrigger"} {
		want := fmt.Sprintf("No maintenance in progress. Not started: %s (%s)", h.windowName("reboots"), situation)
		h.eventually("the message of "+node, func() (bool, string) {
			condition := h.condition(node, generic)
			return condition.Reason == maintenance.ReasonNoMaintenance && condition.Message == want, condition.Message
		})
	}

	status := h.wantStatus("reboots", "in progress with a under maintenance", func(status kumacov1alpha1.MaintenanceWindowStatus) bool {
		return status.State == kumacov1alpha1.StateInProgress && len(status.NodesUnderMaintenance) == 1
	})
	wantCounts := map[string]int32{"UnderMaintenance": 1, "MaxConcurrencyReached": 1, "UnmetTrigger": 1}
	if status.NodesUnderMaintenance[0] != h.nodeName("a") || status.MatchingNodes != 3 || status.Message != "" ||
		fmt.Sprint(status.NodeCounts) != fmt.Sprint(wantCounts) {
		t.Errorf("status = %+v, want a under maintenance, 3 matching nodes, and the counts %v", status, wantCounts)
	}

	// An inhibitor appearing after the start is too late to matter.
	h.report("a", noPod+"=False")
	// While the node reboots, the stale RebootRequired, then Ready not being
	// True, keep it under maintenance.
	h.report("a", ready+"=Unknown")
	h.report("a", required+"=False")
	h.consistently(500*time.Millisecond, "a staying under maintenance, alone", func() (bool, string) {
		under := h.under(reboot)
		return slices.Equal(under, []string{"a"}), fmt.Sprint(under)
	})

	// The reboot is done and the node is back: a is released, and b starts.
	h.report("a", ready+"=True")
	h.wantConditions("a", generic+"=False")
	h.wantConditions("b", reboot+"=True", generic+"=True")
	h.wantEvent("reboots", "Normal MaintenanceReleased", h.nodeName("a"), "stopTriggers")
	h.wantEvent("reboots", "Normal MaintenanceStarted", h.nodeName("b"))

	// The conditions of the other writers were never touched.
	if got := h.conditions("a"); !slices.Contains(got, noPod+"=False") || !slices.Contains(got, ready+"=True") || !slices.Contains(got, required+"=False") {
		t.Errorf("conditions of a = %v, want the reporters' conditions preserved", got)
	}

	// The metrics report the same decisions.
	window := "window=" + h.windowName("reboots")
	h.wantMetric(controller, 1, "kumaco_maintenance_window_active", window)
	h.wantMetric(controller, 0, "kumaco_maintenance_window_invalid", window)
	h.wantMetric(controller, 1, "kumaco_maintenance_window_nodes", "state=UnderMaintenance")
	h.wantMetric(controller, 2, "kumaco_maintenance_window_nodes", "state=UnmetTrigger")
	h.wantMetric(controller, 0, "kumaco_maintenance_window_nodes", "state=MaxConcurrencyReached")
	h.wantMetric(controller, 0, "kumaco_node_slot_occupied", "node="+h.nodeName("a"))
	h.wantMetric(controller, 1, "kumaco_node_slot_occupied", "node="+h.nodeName("b"))
	h.wantMetric(controller, 0, "kumaco_node_slot_occupied", "node="+h.nodeName("c"))
	h.wantMetric(controller, 1, "kumaco_leader")
	for _, name := range []string{"kumaco_reconcile_total", "kumaco_reconcile_duration_seconds", "kumaco_node_write_total", "kumaco_oldest_heartbeat_age_seconds"} {
		if _, ok := controller.metric(t, name); !ok {
			t.Errorf("no %s metric", name)
		}
	}
}

// wantMetric waits until a metric has that value.
func (h *harness) wantMetric(controller *running, want float64, name string, labels ...string) {
	h.t.Helper()
	h.eventually(fmt.Sprintf("%s%v to be %v", name, labels, want), func() (bool, string) {
		got, ok := controller.metric(h.t, name, labels...)
		return ok && got == want, fmt.Sprint(got, ok)
	})
}

// Every node write is one server-side apply of the complete list, as the field
// manager kumaco, which then owns exactly the conditions it wrote.
//
// README: #one-function-one-write-per-node, #public-contracts
func TestKumacoOwnsOnlyItsConditions(t *testing.T) {
	h := newHarness(t)
	h.node("a", rebootable...)
	h.window("reboots", nil)
	h.start(nil)
	h.wantConditions("a", reboot+"=True", generic+"=True")

	owned := map[string][]string{}
	for _, entry := range h.getNodeWithManagedFields("a").ManagedFields {
		if entry.Subresource != "status" || entry.FieldsV1 == nil {
			continue
		}
		for _, conditionType := range []string{reboot, generic, required, noPod, noAlert, ready} {
			if strings.Contains(string(entry.FieldsV1.Raw), fmt.Sprintf(`k:{\"type\":\"%s\"}`, conditionType)) ||
				strings.Contains(string(entry.FieldsV1.Raw), fmt.Sprintf(`k:{"type":"%s"}`, conditionType)) {
				owned[entry.Manager] = append(owned[entry.Manager], conditionType)
			}
		}
	}
	if got := owned[FieldManager]; !slices.Equal(got, []string{reboot, generic}) {
		t.Errorf("the field manager %s owns %v, want %v; all managers: %v", FieldManager, got, []string{reboot, generic}, owned)
	}
}

func (h *harness) getNodeWithManagedFields(name string) *corev1.Node {
	h.t.Helper()
	node, err := clientset.CoreV1().Nodes().Get(context.Background(), h.nodeName(name), metav1.GetOptions{})
	if err != nil {
		h.t.Fatal(err)
	}
	return node
}

// isGone says whether an object of the test was deleted.
func isGone(object client.Object) bool {
	return isNotFound(api.Get(context.Background(), client.ObjectKeyFromObject(object), object))
}
