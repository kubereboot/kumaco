package maintenance

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kumacov1alpha1 "github.com/kubereboot/kumaco/api/v1alpha1"
)

const (
	prefix   = kumacov1alpha1.DefaultConditionPrefix
	reboot   = "kumaco.kured.dev/RebootInProgress"
	firmware = "kumaco.kured.dev/FirmwareInProgress"
	generic  = string(corev1.NodeMaintenanceInProgress)

	required = "detectors.kured.dev/RebootRequired"

	rebootRequired = `node.status.conditions.exists(c, c.type == 'detectors.kured.dev/RebootRequired' && c.status == 'True')`
	rebootDone     = `!node.status.conditions.exists(c, c.type == 'detectors.kured.dev/RebootRequired' && c.status == 'True')`
)

// now is a Wednesday, at 02:30 UTC.
var now = time.Date(2026, 11, 18, 2, 30, 0, 0, time.UTC)

func minutesAgo(minutes int) time.Time { return now.Add(-time.Duration(minutes) * time.Minute) }

// nodeOption changes a node under construction.
type nodeOption func(*corev1.Node)

func newNode(name string, options ...nodeOption) *corev1.Node {
	node := &corev1.Node{}
	node.Name = name
	node.CreationTimestamp = metav1.NewTime(now.Add(-24 * time.Hour))
	for _, option := range options {
		option(node)
	}
	return node
}

func labelled(key, value string) nodeOption {
	return func(node *corev1.Node) {
		if node.Labels == nil {
			node.Labels = map[string]string{}
		}
		node.Labels[key] = value
	}
}

func created(at time.Time) nodeOption {
	return func(node *corev1.Node) { node.CreationTimestamp = metav1.NewTime(at) }
}

// with adds a condition whose last transition and heartbeat are at the given time.
func with(conditionType string, status corev1.ConditionStatus, since time.Time) nodeOption {
	return func(node *corev1.Node) {
		node.Status.Conditions = append(node.Status.Conditions, corev1.NodeCondition{
			Type: corev1.NodeConditionType(conditionType), Status: status,
			LastTransitionTime: metav1.NewTime(since), LastHeartbeatTime: metav1.NewTime(since),
		})
	}
}

// under puts the node under a kind of maintenance since the given time.
func under(kind string, since time.Time) nodeOption {
	return func(node *corev1.Node) {
		with(kind, corev1.ConditionTrue, since)(node)
		for i := range node.Status.Conditions {
			if node.Status.Conditions[i].Type == corev1.NodeMaintenanceInProgress {
				node.Status.Conditions[i].Status = corev1.ConditionTrue
				node.Status.Conditions[i].LastTransitionTime = metav1.NewTime(since)
				node.Status.Conditions[i].LastHeartbeatTime = metav1.NewTime(since)
				return
			}
		}
		with(generic, corev1.ConditionTrue, since)(node)
	}
}

func needsReboot() nodeOption { return with(required, corev1.ConditionTrue, minutesAgo(600)) }

// specOption changes the spec of a window under construction.
type specOption func(*kumacov1alpha1.MaintenanceWindowSpec)

// newWindow returns a window of the reboot kind, in progress at `now` since
// 02:00 (daily, for two hours), selecting all nodes, without triggers, and
// with a concurrency of 1.
func newWindow(t *testing.T, name string, options ...specOption) *Window {
	t.Helper()
	concurrency := int32(1)
	spec := &kumacov1alpha1.MaintenanceWindowSpec{
		StartTime:     "0 0 2 * * ? *",
		Duration:      metav1.Duration{Duration: 2 * time.Hour},
		ConditionType: reboot,
		Concurrency:   &concurrency,
	}
	for _, option := range options {
		option(spec)
	}
	compiled, err := Compile(spec, prefix)
	if err != nil {
		t.Fatalf("Compile(%s): %v", name, err)
	}
	return compiled.At(name, now)
}

func kind(conditionType string) specOption {
	return func(spec *kumacov1alpha1.MaintenanceWindowSpec) { spec.ConditionType = conditionType }
}

func concurrency(value int32) specOption {
	return func(spec *kumacov1alpha1.MaintenanceWindowSpec) { spec.Concurrency = &value }
}

func startTime(value string) specOption {
	return func(spec *kumacov1alpha1.MaintenanceWindowSpec) { spec.StartTime = value }
}

func lasting(duration time.Duration) specOption {
	return func(spec *kumacov1alpha1.MaintenanceWindowSpec) { spec.Duration.Duration = duration }
}

func selecting(key, value string) specOption {
	return func(spec *kumacov1alpha1.MaintenanceWindowSpec) {
		spec.NodeSelector.MatchLabels = map[string]string{key: value}
	}
}

func startTriggers(expressions ...string) specOption {
	return func(spec *kumacov1alpha1.MaintenanceWindowSpec) { spec.StartTriggers = expressions }
}

func stopTriggers(expressions ...string) specOption {
	return func(spec *kumacov1alpha1.MaintenanceWindowSpec) { spec.StopTriggers = expressions }
}

// closed is a window whose only occurrence is over.
func closed() specOption { return startTime("0 0 2 1 1 ? 2020") }

func names(nodes []*corev1.Node) []string {
	result := make([]string, len(nodes))
	for i, node := range nodes {
		result[i] = node.Name
	}
	return result
}

func releasedNames(releases []Release) []string {
	result := make([]string, len(releases))
	for i, release := range releases {
		result[i] = release.Node.Name
	}
	return result
}
