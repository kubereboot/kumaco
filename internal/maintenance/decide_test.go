package maintenance

import (
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	kumacov1alpha1 "github.com/kubereboot/kumaco/api/v1alpha1"
)

const (
	underMaintenance      = kumacov1alpha1.SituationUnderMaintenance
	unmetTrigger          = kumacov1alpha1.SituationUnmetTrigger
	maxConcurrencyReached = kumacov1alpha1.SituationMaxConcurrencyReached
)

// decide reconciles one window, alone of its kind unless others are given.
func decide(t *testing.T, window *Window, nodes []*corev1.Node, others ...*Window) Decision {
	t.Helper()
	return Decide(t.Context(), Input{
		Kind: window.ConditionType, Window: window, Windows: append([]*Window{window}, others...), Nodes: nodes,
	})
}

func wantNames(t *testing.T, what string, got []string, want ...string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

func wantSituations(t *testing.T, decision Decision, want map[string]string) {
	t.Helper()
	if len(decision.Situations) != len(want) {
		t.Errorf("situations = %v, want %v", decision.Situations, want)
	}
	for node, situation := range want {
		if decision.Situations[node] != situation {
			t.Errorf("situation of %s = %q, want %q", node, decision.Situations[node], situation)
		}
	}
}

func TestMaintenanceStartsOnTheCandidatesUpToTheFreeSlots(t *testing.T) {
	window := newWindow(t, "reboots", concurrency(2), startTriggers(rebootRequired), stopTriggers(rebootDone))
	nodes := []*corev1.Node{
		newNode("a", needsReboot()),
		newNode("b", needsReboot()),
		newNode("c", needsReboot()),
		newNode("d"), // does not need a reboot
	}

	decision := decide(t, window, nodes)

	wantNames(t, "started", names(decision.Started), "a", "b")
	wantNames(t, "released", releasedNames(decision.Released))
	wantNames(t, "under maintenance", decision.Under, "a", "b")
	if decision.Matching != 4 {
		t.Errorf("matching nodes = %d, want 4", decision.Matching)
	}
	wantSituations(t, decision, map[string]string{
		"a": underMaintenance, "b": underMaintenance, "c": maxConcurrencyReached, "d": unmetTrigger,
	})
}

func TestAnEmptyStartTriggersListLetsTheScheduleAndConcurrencyDecide(t *testing.T) {
	decision := decide(t, newWindow(t, "reboots"), []*corev1.Node{newNode("a"), newNode("b")})

	wantNames(t, "started", names(decision.Started), "a")
	wantSituations(t, decision, map[string]string{"a": underMaintenance, "b": maxConcurrencyReached})
}

// The release runs before any start: the slot it frees is reused at once, and a
// node whose stopTriggers match does not start.
//
// README: #when-does-a-window-keep-a-node-under-maintenance
func TestAReleasedSlotIsReusedInTheSameReconcile(t *testing.T) {
	window := newWindow(t, "reboots", startTriggers(rebootRequired), stopTriggers(rebootDone))
	nodes := []*corev1.Node{
		newNode("a", under(reboot, minutesAgo(20))), // rebooted: RebootRequired is gone
		newNode("b", needsReboot()),
	}

	decision := decide(t, window, nodes)

	wantNames(t, "released", releasedNames(decision.Released), "a")
	if len(decision.Released) == 1 && !strings.Contains(decision.Released[0].Reason, "stopTriggers") {
		t.Errorf("release reason = %q, want it to mention stopTriggers", decision.Released[0].Reason)
	}
	wantNames(t, "started", names(decision.Started), "b")
	wantNames(t, "under maintenance", decision.Under, "b")
	wantSituations(t, decision, map[string]string{"a": unmetTrigger, "b": underMaintenance})
}

// README: #when-does-a-window-keep-a-node-under-maintenance
func TestAWindowKeepsANode(t *testing.T) {
	underReboot := under(reboot, minutesAgo(20))
	for _, tt := range []struct {
		name   string
		window []specOption
		node   *corev1.Node
		// wantReason is a part of the release reason; empty when the node is kept.
		wantReason string
	}{
		{name: "in progress, selected, started in this occurrence, no stop trigger",
			node: newNode("a", underReboot)},
		{name: "a stop trigger that does not match",
			window: []specOption{stopTriggers(rebootDone)}, node: newNode("a", underReboot, needsReboot())},
		{name: "started exactly at the start of the occurrence",
			node: newNode("a", under(reboot, minutesAgo(30)))},

		{name: "the window is not in progress",
			window: []specOption{closed()}, node: newNode("a", underReboot), wantReason: "not in progress"},
		{name: "the node no longer matches the nodeSelector",
			window: []specOption{selecting("team", "a")}, node: newNode("a", underReboot, labelled("team", "b")), wantReason: "nodeSelector"},
		{name: "the record is older than the current occurrence",
			node: newNode("a", under(reboot, minutesAgo(31))), wantReason: "before the current occurrence"},
		{name: "one of the stop triggers matches",
			window: []specOption{stopTriggers("false", rebootDone)}, node: newNode("a", underReboot), wantReason: "stopTriggers"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			window := newWindow(t, "reboots", append([]specOption{concurrency(0)}, tt.window...)...)

			decision := decide(t, window, []*corev1.Node{tt.node})

			if tt.wantReason == "" {
				wantNames(t, "released", releasedNames(decision.Released))
				return
			}
			if len(decision.Released) != 1 || !strings.Contains(decision.Released[0].Reason, tt.wantReason) {
				t.Errorf("released = %+v, want the node, with a reason mentioning %q", decision.Released, tt.wantReason)
			}
		})
	}
}

// The example of the README: a daily window from 02:00 to 04:00. Maintenance
// started on Monday at 02:10; the controller was down from Monday 03:00 to
// Tuesday 02:05. The record outlived its occurrence: the node is released, and
// is not started again in the same reconcile.
//
// README: #when-does-a-window-keep-a-node-under-maintenance
func TestARecordThatOutlivedItsOccurrenceIsReleased(t *testing.T) {
	monday := time.Date(2026, 11, 16, 2, 10, 0, 0, time.UTC)
	tuesday := time.Date(2026, 11, 17, 2, 5, 0, 0, time.UTC)
	window := newWindow(t, "reboots").Compiled.At("reboots", tuesday)

	decision := decide(t, window, []*corev1.Node{newNode("a", under(reboot, monday))})

	wantNames(t, "released", releasedNames(decision.Released), "a")
	wantNames(t, "started", names(decision.Started))
}

// A window running 24/7 as daily 24-hour occurrences is a single chain: its
// records are not released every midnight.
//
// README: #when-does-a-window-keep-a-node-under-maintenance
func TestAChainOfOccurrencesKeepsItsRecords(t *testing.T) {
	window := newWindow(t, "always", startTime("0 0 0 * * ? *"), lasting(24*time.Hour))
	yesterday := now.Add(-26 * time.Hour)

	decision := decide(t, window, []*corev1.Node{newNode("a", under(reboot, yesterday))})

	wantNames(t, "released", releasedNames(decision.Released))

	tooOld := now.Add(-32 * 24 * time.Hour)
	decision = decide(t, window, []*corev1.Node{newNode("a", under(reboot, tooOld))})

	wantNames(t, "released after 31 days", releasedNames(decision.Released), "a")
}

// An expression that errors never changes whether a node is under maintenance.
//
// README: #the-kumacokureddev-maintenancewindow-crd, #cel-triggers
func TestATriggerThatErrorsFailsClosed(t *testing.T) {
	const erroring = `node.spec.taints.size() > 0` // the nodes have no taints
	for _, tt := range []struct {
		name          string
		window        []specOption
		node          *corev1.Node
		wantSituation string
	}{
		{name: "in startTriggers, maintenance does not start",
			window: []specOption{startTriggers("true", erroring)}, node: newNode("a"), wantSituation: unmetTrigger},
		{name: "in stopTriggers, for a node not under maintenance, maintenance does not start",
			window: []specOption{stopTriggers(erroring)}, node: newNode("a"), wantSituation: unmetTrigger},
		{name: "in stopTriggers, for a node under maintenance, the expression does not release",
			window: []specOption{stopTriggers(erroring)}, node: newNode("a", under(reboot, minutesAgo(20))), wantSituation: underMaintenance},
	} {
		t.Run(tt.name, func(t *testing.T) {
			decision := decide(t, newWindow(t, "reboots", tt.window...), []*corev1.Node{tt.node})

			wantNames(t, "started", names(decision.Started))
			wantNames(t, "released", releasedNames(decision.Released))
			wantSituations(t, decision, map[string]string{"a": tt.wantSituation})
			if len(decision.Failures) != 1 {
				t.Fatalf("failures = %+v, want one", decision.Failures)
			}
			failure := decision.Failures[0]
			if failure.Window != "reboots" || failure.Node != "a" || failure.Expression != erroring || failure.Err == nil {
				t.Errorf("failure = %+v, want the window, the node, the expression and its error", failure)
			}
		})
	}
}

// In stopTriggers, an expression that errors does not hide another one that matches.
//
// README: #the-kumacokureddev-maintenancewindow-crd
func TestAnotherStopTriggerStillReleasesWhenOneErrors(t *testing.T) {
	window := newWindow(t, "reboots", stopTriggers(`node.spec.taints.size() > 0`, rebootDone))

	decision := decide(t, window, []*corev1.Node{newNode("a", under(reboot, minutesAgo(20)))})

	wantNames(t, "released", releasedNames(decision.Released), "a")
}

func TestAWindowThatIsNotInProgressStartsNothing(t *testing.T) {
	window := newWindow(t, "reboots", closed())
	nodes := []*corev1.Node{newNode("a", under(reboot, minutesAgo(20))), newNode("b")}

	decision := decide(t, window, nodes)

	wantNames(t, "started", names(decision.Started))
	wantNames(t, "released", releasedNames(decision.Released), "a")
	if decision.Matching != 2 || len(decision.Situations) != 0 {
		t.Errorf("matching = %d, situations = %v, want 2 matching nodes and no situation", decision.Matching, decision.Situations)
	}
}

// Lowering concurrency never releases nodes: it only prevents new starts.
//
// README: #the-kumacokureddev-maintenancewindow-crd
func TestLoweringConcurrencyDoesNotEvict(t *testing.T) {
	nodes := []*corev1.Node{
		newNode("a", under(reboot, minutesAgo(20))),
		newNode("b", under(reboot, minutesAgo(10))),
		newNode("c"),
	}
	for _, value := range []int32{1, 0} {
		decision := decide(t, newWindow(t, "reboots", concurrency(value)), nodes)

		wantNames(t, "released", releasedNames(decision.Released))
		wantNames(t, "started", names(decision.Started))
		wantSituations(t, decision, map[string]string{"a": underMaintenance, "b": underMaintenance, "c": maxConcurrencyReached})
	}
}

func TestTheOrderOfTheCandidates(t *testing.T) {
	old, recent := minutesAgo(600), minutesAgo(60)
	nodes := []*corev1.Node{
		// Standalone candidates: the one that left maintenance the longest time ago first.
		newNode("left-recently", with(generic, corev1.ConditionFalse, recent)),
		newNode("left-long-ago", with(generic, corev1.ConditionFalse, old)),
		// Without the condition yet: by creation time, among the others.
		newNode("new-node", created(minutesAgo(5))),
		newNode("old-node", created(minutesAgo(6000))),
		// Ties are broken by name.
		newNode("tie-b", with(generic, corev1.ConditionFalse, old)),
		// Candidates already under another kind of maintenance come first, most recent first.
		newNode("firmware-since-long", under(firmware, old)),
		newNode("firmware-since-recently", under(firmware, recent)),
	}
	window := newWindow(t, "reboots", concurrency(7))
	firmwareWindow := newWindow(t, "firmware", kind(firmware), concurrency(7))

	decision := decide(t, window, nodes, firmwareWindow)

	wantNames(t, "started, in order", names(decision.Started),
		"firmware-since-recently", "firmware-since-long", "old-node", "left-long-ago", "tie-b", "left-recently", "new-node")
}

// Windows of the same kind work on the same records: they count the same
// nodes, and one keeps what another started, if it started during its own
// current occurrence.
//
// README: #concurrency-conflicts, #when-does-a-window-keep-a-node-under-maintenance, #the-kumacokureddev-maintenancewindow-crd
func TestWindowsOfTheSameKindShareRecords(t *testing.T) {
	weekdays := newWindow(t, "weekdays")
	// The other window has been in progress since 01:00.
	nights := newWindow(t, "nights", startTime("0 0 1 * * ? *"), lasting(3*time.Hour))
	nodes := []*corev1.Node{
		newNode("a", under(reboot, minutesAgo(45))), // started by nights, at 01:45
		newNode("b"),
	}

	decision := decide(t, weekdays, nodes, nights)

	// a started before the occurrence of weekdays (02:00), but nights keeps it.
	wantNames(t, "released", releasedNames(decision.Released))
	// It uses the only slot of weekdays, although nights started it.
	wantNames(t, "started", names(decision.Started))
	wantSituations(t, decision, map[string]string{"a": underMaintenance, "b": maxConcurrencyReached})

	// When nights ends, a is only kept by a window whose occurrence it started in.
	over := newWindow(t, "nights", closed())
	decision = decide(t, over, nodes, weekdays)

	wantNames(t, "released when its window ended", releasedNames(decision.Released), "a")
}

// Windows of different kinds do not count each other's nodes, and a node can be
// under several kinds of maintenance: it uses one slot in each.
//
// README: #concurrency-conflicts, #the-kumacokureddev-maintenancewindow-crd
func TestWindowsOfDifferentKindsAreDistinct(t *testing.T) {
	nodes := []*corev1.Node{newNode("a", under(firmware, minutesAgo(20))), newNode("b")}

	decision := decide(t, newWindow(t, "reboots"), nodes, newWindow(t, "firmware", kind(firmware)))

	wantNames(t, "released", releasedNames(decision.Released))
	wantNames(t, "started", names(decision.Started), "a")
	wantSituations(t, decision, map[string]string{"a": underMaintenance, "b": maxConcurrencyReached})
}

// Only a True condition is a record.
func TestOnlyATrueConditionIsARecord(t *testing.T) {
	nodes := []*corev1.Node{
		newNode("a", with(reboot, corev1.ConditionUnknown, minutesAgo(20))),
		newNode("b", with(reboot, corev1.ConditionFalse, minutesAgo(20))),
	}

	decision := decide(t, newWindow(t, "reboots", concurrency(2)), nodes)

	wantNames(t, "released", releasedNames(decision.Released))
	wantNames(t, "started", names(decision.Started), "a", "b")
}

// A deleted or Invalid window keeps nothing. Its reconcile releases the nodes of
// its kind that no other window keeps.
func TestTheReleaseOfADeletedWindow(t *testing.T) {
	nodes := []*corev1.Node{newNode("a", under(reboot, minutesAgo(20))), newNode("b", under(firmware, minutesAgo(20)))}

	decision := Decide(t.Context(), Input{Kind: reboot, ReleaseReason: "the window was deleted", Nodes: nodes})

	wantNames(t, "released", releasedNames(decision.Released), "a")
	if len(decision.Released) == 1 && decision.Released[0].Reason != "the window was deleted" {
		t.Errorf("reason = %q, want the given one", decision.Released[0].Reason)
	}
	wantNames(t, "started", names(decision.Started))

	other := newWindow(t, "weekdays")
	decision = Decide(t.Context(), Input{Kind: reboot, ReleaseReason: "the window was deleted", Windows: []*Window{other}, Nodes: nodes})

	wantNames(t, "released while another window of the kind keeps the node", releasedNames(decision.Released))
}

// A is in progress but its trigger does not match; B's trigger matches but B is
// closed: neither starts the node. Once B is in progress, B alone is sufficient.
//
// README: #scheduling-conflicts, #triggering-conflicts
func TestOneWindowAloneDecides(t *testing.T) {
	nodes := []*corev1.Node{newNode("node1")}
	a := newWindow(t, "a", startTriggers("false"))
	closedB := newWindow(t, "b", closed(), startTriggers("true"))

	wantNames(t, "started by a", names(decide(t, a, nodes, closedB).Started))
	wantNames(t, "started by b, closed", names(decide(t, closedB, nodes, a).Started))

	b := newWindow(t, "b", startTriggers("true"))
	wantNames(t, "started by b, in progress", names(decide(t, b, nodes, a).Started), "node1")
}

// README: #nodeselector-and-trigger-conflicts
func TestTheNodeSelectorHasPrecedenceOverTriggers(t *testing.T) {
	nodes := []*corev1.Node{newNode("node1", labelled("name", "node1")), newNode("node2", labelled("name", "node2"))}
	for _, tt := range []struct {
		name   string
		window []specOption
		want   []string
	}{
		{"all nodes selected, a trigger matching node1",
			[]specOption{startTriggers(`node.metadata.name == 'node1'`)}, []string{"node1"}},
		{"node1 selected, a trigger matching all nodes",
			[]specOption{selecting("name", "node1"), startTriggers("true")}, []string{"node1"}},
		{"node1 selected, a trigger matching node2",
			[]specOption{selecting("name", "node1"), startTriggers(`node.metadata.name == 'node2'`)}, []string{}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			decision := decide(t, newWindow(t, "w", append(tt.window, concurrency(2))...), nodes)

			wantNames(t, "started", names(decision.Started), tt.want...)
		})
	}
}

// A stopTriggers expression depending on a condition the window sets
// oscillates: the node is released right after its maintenance started. This
// documents the behaviour; kumaco does not prevent it.
func TestASelfReferentialStopTriggerReleasesRightAfterTheStart(t *testing.T) {
	window := newWindow(t, "reboots", stopTriggers(
		`node.status.conditions.exists(c, c.type == 'MaintenanceInProgress' && c.status == 'True')`))

	wantNames(t, "started", names(decide(t, window, []*corev1.Node{newNode("a")}).Started), "a")

	started := []*corev1.Node{newNode("a", under(reboot, now))}
	wantNames(t, "released", releasedNames(decide(t, window, started).Released), "a")
}
