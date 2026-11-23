package maintenance

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
)

var bothTypes = sets.New(reboot, firmware)

func desired(change func(*Desired)) Desired {
	d := Desired{Prefix: prefix, WindowTypes: bothTypes, Now: now}
	if change != nil {
		change(&d)
	}
	return d
}

// statuses returns the types and statuses of a write, in order.
func statuses(write Write) []string {
	result := make([]string, len(write.Conditions))
	for i, condition := range write.Conditions {
		result[i] = string(condition.Type) + "=" + string(condition.Status)
	}
	return result
}

func conditionOf(t *testing.T, write Write, conditionType string) corev1.NodeCondition {
	t.Helper()
	for _, condition := range write.Conditions {
		if string(condition.Type) == conditionType {
			return condition
		}
	}
	t.Fatalf("the write has no %s condition: %v", conditionType, statuses(write))
	return corev1.NodeCondition{}
}

// The table of the README: a reboot window and a firmware window.
//
// README: #one-function-one-write-per-node, #the-node-is-the-record
func TestDesiredConditionsIsTheCompleteList(t *testing.T) {
	since := minutesAgo(20)
	for _, tt := range []struct {
		name        string
		node        *corev1.Node
		desired     Desired
		want        []string
		wantChanged bool
		wantStarts  bool
	}{
		{
			name: "nothing in progress",
			node: newNode("a", with(generic, corev1.ConditionFalse, since)), desired: desired(nil),
			want: []string{generic + "=False"},
		},
		{
			name: "a new node has no condition yet",
			node: newNode("a"), desired: desired(nil),
			want: []string{generic + "=False"}, wantChanged: true,
		},
		{
			name:    "the reboot window starts on the node",
			node:    newNode("a", with(generic, corev1.ConditionFalse, since)),
			desired: desired(func(d *Desired) { d.Started = map[string]string{reboot: "reboots-weekdays"} }),
			want:    []string{reboot + "=True", generic + "=True"}, wantChanged: true, wantStarts: true,
		},
		{
			name:    "the firmware window starts on the node too",
			node:    newNode("a", under(reboot, since)),
			desired: desired(func(d *Desired) { d.Started = map[string]string{firmware: "firmware-q4"} }),
			want:    []string{firmware + "=True", reboot + "=True", generic + "=True"}, wantChanged: true, wantStarts: true,
		},
		{
			name:    "the reboot stopTriggers match",
			node:    newNode("a", under(reboot, since), under(firmware, since)),
			desired: desired(func(d *Desired) { d.Released = []string{reboot} }),
			want:    []string{firmware + "=True", generic + "=True"}, wantChanged: true,
		},
		{
			name:    "the last kind is released",
			node:    newNode("a", under(reboot, since)),
			desired: desired(func(d *Desired) { d.Released = []string{reboot} }),
			want:    []string{generic + "=False"}, wantChanged: true,
		},
		{
			name:    "the firmware window was deleted while kumaco was down",
			node:    newNode("a", under(firmware, since)),
			desired: desired(func(d *Desired) { d.WindowTypes = sets.New(reboot) }),
			want:    []string{generic + "=False"}, wantChanged: true,
		},
		{
			name:    "MaintenanceInProgress is outdated",
			node:    newNode("a", with(reboot, corev1.ConditionTrue, since), with(generic, corev1.ConditionFalse, since)),
			desired: desired(nil),
			want:    []string{reboot + "=True", generic + "=True"}, wantChanged: true,
		},
		{
			name:    "a condition under the prefix that is not True is not a record, and is not kept",
			node:    newNode("a", with(reboot, corev1.ConditionUnknown, since), with(generic, corev1.ConditionFalse, since)),
			desired: desired(nil),
			want:    []string{generic + "=False"}, wantChanged: true,
		},
		{
			name:    "the conditions of other writers are never in the list",
			node:    newNode("a", with("Ready", corev1.ConditionTrue, since), needsReboot(), under(reboot, since)),
			desired: desired(nil),
			want:    []string{reboot + "=True", generic + "=True"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			write := DesiredConditions(tt.node, tt.desired)

			wantNames(t, "conditions", statuses(write), tt.want...)
			if write.Changed != tt.wantChanged || write.Starts != tt.wantStarts {
				t.Errorf("Changed = %v, Starts = %v, want %v and %v", write.Changed, write.Starts, tt.wantChanged, tt.wantStarts)
			}
		})
	}
}

// README: #one-function-one-write-per-node
func TestDesiredConditionsDoesNotChangeTheNode(t *testing.T) {
	node := newNode("a", under(reboot, minutesAgo(20)))
	before := node.DeepCopy()

	DesiredConditions(node, desired(func(d *Desired) {
		d.Released = []string{reboot}
		d.HeartbeatStaleBefore = now
	}))

	if len(node.Status.Conditions) != len(before.Status.Conditions) || node.Status.Conditions[0] != before.Status.Conditions[0] {
		t.Errorf("the node changed: %+v", node.Status.Conditions)
	}
}

// README: #what-does-it-look-like-on-my-node, #one-function-one-write-per-node
func TestTheConditionsOfANodeUnderMaintenance(t *testing.T) {
	node := newNode("a", with(generic, corev1.ConditionFalse, minutesAgo(600)), under(firmware, minutesAgo(20)))

	write := DesiredConditions(node, desired(func(d *Desired) {
		d.Started = map[string]string{reboot: "reboots-weekdays"}
		d.NotStarted = []NotStarted{{Window: "kernel-live-patch", Situation: "UnmetTrigger"}}
	}))

	started := conditionOf(t, write, reboot)
	if started.Reason != ReasonMaintenanceRunning || started.Message != "Started by MaintenanceWindow reboots-weekdays" {
		t.Errorf("new kind: reason %q, message %q", started.Reason, started.Message)
	}
	if !started.LastTransitionTime.Time.Equal(now) || !started.LastHeartbeatTime.Time.Equal(now) {
		t.Errorf("new kind: transition %s, heartbeat %s, want the current time", started.LastTransitionTime, started.LastHeartbeatTime)
	}
	carried := conditionOf(t, write, firmware)
	if !carried.LastTransitionTime.Time.Equal(minutesAgo(20)) || !carried.LastHeartbeatTime.Time.Equal(minutesAgo(20)) {
		t.Errorf("carried-over kind: transition %s, heartbeat %s, want them kept", carried.LastTransitionTime, carried.LastHeartbeatTime)
	}
	inProgress := conditionOf(t, write, generic)
	if inProgress.Reason != ReasonMaintenanceRunning {
		t.Errorf("MaintenanceInProgress: reason %q", inProgress.Reason)
	}
	const message = "In progress: kumaco.kured.dev/FirmwareInProgress, kumaco.kured.dev/RebootInProgress. " +
		"Not started: kernel-live-patch (UnmetTrigger)"
	if inProgress.Message != message {
		t.Errorf("MaintenanceInProgress: message %q, want %q", inProgress.Message, message)
	}
	if !inProgress.LastTransitionTime.Time.Equal(minutesAgo(20)) {
		t.Errorf("MaintenanceInProgress: transition %s, want it kept: the status did not change", inProgress.LastTransitionTime)
	}
}

// README: #what-does-it-look-like-on-my-node, #one-function-one-write-per-node
func TestTheConditionOfANodeWithoutMaintenance(t *testing.T) {
	node := newNode("a", under(reboot, minutesAgo(20)))

	write := DesiredConditions(node, desired(func(d *Desired) { d.Released = []string{reboot} }))

	condition := conditionOf(t, write, generic)
	if condition.Reason != ReasonNoMaintenance || condition.Message != "No maintenance in progress." {
		t.Errorf("reason %q, message %q", condition.Reason, condition.Message)
	}
	if !condition.LastTransitionTime.Time.Equal(now) {
		t.Errorf("transition %s, want the current time: the status changed", condition.LastTransitionTime)
	}
	if !condition.LastHeartbeatTime.Time.Equal(minutesAgo(20)) {
		t.Errorf("heartbeat %s, want it kept: no heartbeat is due", condition.LastHeartbeatTime)
	}

	write = DesiredConditions(newNode("a"), desired(func(d *Desired) {
		d.NotStarted = []NotStarted{
			{Window: "reboots-weekend", Situation: "UnmetTrigger"},
			{Window: "reboots-weekdays", Situation: "MaxConcurrencyReached"},
		}
	}))

	const message = "No maintenance in progress. Not started: reboots-weekdays (MaxConcurrencyReached), reboots-weekend (UnmetTrigger)"
	if got := conditionOf(t, write, generic).Message; got != message {
		t.Errorf("message %q, want %q", got, message)
	}
}

// A difference in message alone does not cause a write.
//
// README: #one-function-one-write-per-node
func TestAMessageAloneIsNotAChange(t *testing.T) {
	node := newNode("a", with(generic, corev1.ConditionFalse, minutesAgo(20)))

	write := DesiredConditions(node, desired(func(d *Desired) {
		d.NotStarted = []NotStarted{{Window: "reboots", Situation: "UnmetTrigger"}}
	}))

	if write.Changed || write.Refreshed {
		t.Errorf("Changed = %v, Refreshed = %v, want no reason to write", write.Changed, write.Refreshed)
	}
	if got := conditionOf(t, write, generic).Message; got != "No maintenance in progress. Not started: reboots (UnmetTrigger)" {
		t.Errorf("message %q, want the new one, for the next write", got)
	}
}

// Heartbeats are refreshed when due, on every owned condition, in the same
// list as any other change.
//
// README: #heartbeat-contract, #startup-and-periodic-sweep
func TestHeartbeatsAreRefreshedWhenDue(t *testing.T) {
	interval := 5 * time.Minute
	node := newNode("a",
		with(generic, corev1.ConditionTrue, minutesAgo(20)),
		with(reboot, corev1.ConditionTrue, minutesAgo(20)),
		with(firmware, corev1.ConditionTrue, minutesAgo(20)))
	node.Status.Conditions[1].LastHeartbeatTime = metav1.NewTime(minutesAgo(6))
	node.Status.Conditions[2].LastHeartbeatTime = metav1.NewTime(minutesAgo(4))

	write := DesiredConditions(node, desired(func(d *Desired) { d.HeartbeatStaleBefore = now.Add(-interval) }))

	if write.Changed || !write.Refreshed {
		t.Errorf("Changed = %v, Refreshed = %v, want only a refresh", write.Changed, write.Refreshed)
	}
	for conditionType, want := range map[string]time.Time{generic: now, reboot: now, firmware: minutesAgo(4)} {
		condition := conditionOf(t, write, conditionType)
		if !condition.LastHeartbeatTime.Time.Equal(want) {
			t.Errorf("%s: heartbeat %s, want %s", conditionType, condition.LastHeartbeatTime, want)
		}
		if !condition.LastTransitionTime.Time.Equal(minutesAgo(20)) {
			t.Errorf("%s: transition %s, want it kept", conditionType, condition.LastTransitionTime)
		}
	}

	write = DesiredConditions(node, desired(nil))
	if write.Refreshed || !conditionOf(t, write, reboot).LastHeartbeatTime.Time.Equal(minutesAgo(6)) {
		t.Errorf("without a heartbeat policy, as in the startup sweep: Refreshed = %v, want old heartbeats kept", write.Refreshed)
	}
}

// README: #one-function-one-write-per-node
func TestOwned(t *testing.T) {
	for conditionType, want := range map[string]bool{
		generic: true, reboot: true, "kumaco.kured.dev/Anything": true,
		"Ready": false, required: false, "kumaco.kured.dev": false, "example.com/kumaco.kured.dev/X": false,
	} {
		if got := Owned(corev1.NodeConditionType(conditionType), prefix); got != want {
			t.Errorf("Owned(%q) = %v, want %v", conditionType, got, want)
		}
	}
}

// README: #what-does-it-reconcile-and-when
func TestRelevantChange(t *testing.T) {
	base := func() *corev1.Node {
		return newNode("a", labelled("team", "a"),
			with("Ready", corev1.ConditionTrue, minutesAgo(600)), under(reboot, minutesAgo(20)))
	}
	for _, tt := range []struct {
		name   string
		change func(node *corev1.Node)
		want   bool
	}{
		{"nothing", func(*corev1.Node) {}, false},
		{"a label", func(node *corev1.Node) { node.Labels["team"] = "b" }, true},
		{"an annotation", func(node *corev1.Node) { node.Annotations = map[string]string{"a": "b"} }, true},
		{"the spec: the node is cordoned", func(node *corev1.Node) { node.Spec.Unschedulable = true }, true},
		{"the status of a condition", func(node *corev1.Node) { node.Status.Conditions[0].Status = corev1.ConditionFalse }, true},
		{"the reason of a condition", func(node *corev1.Node) { node.Status.Conditions[0].Reason = "KubeletNotReady" }, true},
		{"a new condition", func(node *corev1.Node) { needsReboot()(node) }, true},
		{"a removed condition", func(node *corev1.Node) { node.Status.Conditions = node.Status.Conditions[1:] }, true},
		{"the heartbeat of a condition only", func(node *corev1.Node) {
			node.Status.Conditions[0].LastHeartbeatTime = metav1.NewTime(now)
		}, false},
		{"kumaco's own MaintenanceInProgress", func(node *corev1.Node) {
			node.Status.Conditions[2].Status = corev1.ConditionFalse
		}, false},
		{"kumaco's own condition of a kind", func(node *corev1.Node) {
			node.Status.Conditions = append(node.Status.Conditions[:1], node.Status.Conditions[2:]...)
		}, false},
		{"another status field", func(node *corev1.Node) { node.Status.NodeInfo.KubeletVersion = "v1.37.1" }, false},
		{"other metadata", func(node *corev1.Node) { node.ResourceVersion = "42"; node.Finalizers = []string{"x"} }, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			after := base()
			tt.change(after)
			if got := RelevantChange(base(), after, prefix); got != tt.want {
				t.Errorf("RelevantChange = %v, want %v", got, tt.want)
			}
		})
	}
}
