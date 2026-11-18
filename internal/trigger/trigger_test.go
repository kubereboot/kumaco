package trigger

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The triggers of the README, "How kured relies on kumaco".
const (
	rebootRequired   = `node.status.conditions.exists(c, c.type == 'detectors.kured.dev/RebootRequired' && c.status == 'True')`
	noPodInhibitor   = `node.status.conditions.exists(c, c.type == 'detectors.kured.dev/NoPodInhibitor' && c.status == 'True')`
	noAlertInhibitor = `node.status.conditions.exists(c, c.type == 'detectors.kured.dev/NoAlertInhibitor' && c.status == 'True')`
	rebootDone       = `!node.status.conditions.exists(c, c.type == 'detectors.kured.dev/RebootRequired' && c.status == 'True') &&
node.status.conditions.exists(c, c.type == 'Ready' && c.status == 'True')`
	inhibitorMissing = `!node.status.conditions.exists(c, c.type == 'detectors.kured.dev/NoPodInhibitor')`
	inhibitorUnknown = `node.status.conditions.exists(c, c.type == 'detectors.kured.dev/NoPodInhibitor' && c.status == 'Unknown')`
)

func nodeWith(conditions ...corev1.NodeCondition) *corev1.Node {
	node := &corev1.Node{}
	node.Name = "node-1"
	node.Status.Conditions = conditions
	return node
}

func condition(conditionType string, status corev1.ConditionStatus) corev1.NodeCondition {
	return corev1.NodeCondition{Type: corev1.NodeConditionType(conditionType), Status: status}
}

func eval(t *testing.T, expression string, node *corev1.Node) (bool, error) {
	t.Helper()
	compiled, err := Compile(expression)
	if err != nil {
		t.Fatalf("Compile(%q): %v", expression, err)
	}
	return compiled.Eval(t.Context(), variable(t, node))
}

func variable(t testing.TB, node *corev1.Node) map[string]any {
	t.Helper()
	variable, err := NodeVariable(node)
	if err != nil {
		t.Fatal(err)
	}
	return variable
}

// The triggers that the README documents for Kured give the documented answers.
// This is not the Kured compatibility suite: that section stays pending until
// the e2e tests run Kured and its reporters.
//
// README: #cel-triggers
func TestTheDocumentedKuredTriggers(t *testing.T) {
	const (
		required = "detectors.kured.dev/RebootRequired"
		noPod    = "detectors.kured.dev/NoPodInhibitor"
	)
	for _, tt := range []struct {
		name       string
		expression string
		node       *corev1.Node
		want       bool
	}{
		{"RebootRequired is True", rebootRequired, nodeWith(condition(required, corev1.ConditionTrue)), true},
		{"RebootRequired is False", rebootRequired, nodeWith(condition(required, corev1.ConditionFalse)), false},
		{"RebootRequired is missing", rebootRequired, nodeWith(), false},

		{"NoPodInhibitor is True", noPodInhibitor, nodeWith(condition(noPod, corev1.ConditionTrue)), true},
		{"NoPodInhibitor is False does not authorize", noPodInhibitor, nodeWith(condition(noPod, corev1.ConditionFalse)), false},
		{"NoPodInhibitor is Unknown does not authorize", noPodInhibitor, nodeWith(condition(noPod, corev1.ConditionUnknown)), false},
		{"NoPodInhibitor is missing does not authorize", noPodInhibitor, nodeWith(), false},
		{"NoAlertInhibitor is missing does not authorize", noAlertInhibitor, nodeWith(condition(noPod, corev1.ConditionTrue)), false},

		{"release: the reboot is done and the node is back", rebootDone,
			nodeWith(condition(required, corev1.ConditionFalse), condition("Ready", corev1.ConditionTrue)), true},
		{"release: RebootRequired was removed", rebootDone, nodeWith(condition("Ready", corev1.ConditionTrue)), true},
		{"no release: the reboot is still required", rebootDone,
			nodeWith(condition(required, corev1.ConditionTrue), condition("Ready", corev1.ConditionTrue)), false},
		{"no release: the node is not Ready yet", rebootDone,
			nodeWith(condition(required, corev1.ConditionFalse), condition("Ready", corev1.ConditionUnknown)), false},

		{"an explicit match of a missing condition", inhibitorMissing, nodeWith(), true},
		{"an explicit match of a missing condition, which is there", inhibitorMissing, nodeWith(condition(noPod, corev1.ConditionFalse)), false},
		{"an explicit match of Unknown", inhibitorUnknown, nodeWith(condition(noPod, corev1.ConditionUnknown)), true},
		{"an explicit match of Unknown, which is missing", inhibitorUnknown, nodeWith(), false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := eval(t, tt.expression, tt.node)
			if err != nil || got != tt.want {
				t.Errorf("Eval = %v, %v, want %v", got, err, tt.want)
			}
		})
	}
}

// README: #cel-triggers
func TestTheNodeVariable(t *testing.T) {
	bare := nodeWith()
	full := nodeWith(corev1.NodeCondition{
		Type: "Ready", Status: corev1.ConditionTrue, Reason: "KubeletReady", Message: "kubelet is posting ready status",
		LastHeartbeatTime:  metav1.NewTime(time.Date(2026, 11, 18, 10, 5, 0, 0, time.UTC)),
		LastTransitionTime: metav1.NewTime(time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)),
	})
	full.Labels = map[string]string{"team": "a"}
	full.Annotations = map[string]string{"example.com/note": "x"}
	full.Spec.Unschedulable = true
	full.Spec.Taints = []corev1.Taint{{Key: "node.kubernetes.io/unschedulable", Effect: corev1.TaintEffectNoSchedule}}
	full.Status.NodeInfo.KubeletVersion = "v1.37.0"

	for _, tt := range []struct {
		name       string
		expression string
		node       *corev1.Node
	}{
		{"the name", `node.metadata.name == 'node-1'`, bare},
		{"labels", `node.metadata.labels['team'] == 'a'`, full},
		{"labels are an empty map when the node has none", `node.metadata.labels.size() == 0 && !('team' in node.metadata.labels)`, bare},
		{"annotations", `node.metadata.annotations['example.com/note'] == 'x'`, full},
		{"annotations are an empty map when the node has none", `node.metadata.annotations.size() == 0`, bare},
		{"a cordoned node", `node.spec.unschedulable`, full},
		{"unschedulable is false, not absent, on a schedulable node", `node.spec.unschedulable == false`, bare},
		{"other spec fields, as the API returns them", `node.spec.taints.exists(t, t.key == 'node.kubernetes.io/unschedulable' && t.effect == 'NoSchedule')`, full},
		{"an absent spec field is tested with has()", `!has(node.spec.taints)`, bare},
		{"conditions are an empty list when the node has none", `node.status.conditions.size() == 0`, bare},
		{"the fields of a condition", `node.status.conditions.exists(c, c.type == 'Ready' && c.status == 'True' &&
			c.reason == 'KubeletReady' && c.message.startsWith('kubelet'))`, full},
		{"the timestamps are RFC 3339 strings", `node.status.conditions.all(c, c.lastTransitionTime == '2026-11-18T10:00:00Z' &&
			timestamp(c.lastHeartbeatTime) - timestamp(c.lastTransitionTime) == duration('5m'))`, full},
		{"other status fields are not available", `!has(node.status.nodeInfo)`, full},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := eval(t, tt.expression, tt.node)
			if err != nil || !got {
				t.Errorf("Eval(%q) = %v, %v, want true", tt.expression, got, err)
			}
		})
	}
}

// The same environment as Kubernetes' own CEL features.
//
// README: #cel-triggers
func TestTheKubernetesLibraries(t *testing.T) {
	node := nodeWith()
	node.Labels = map[string]string{"zone": "eu-west-1a", "kernel": "6.12.3"}
	for name, expression := range map[string]string{
		"strings":                    `node.metadata.name.split('-')[1] == '1' && 'A'.lowerAscii() == 'a'`,
		"lists":                      `[3, 1, 2].sort() == [1, 2, 3] && [1, 2].sum() == 3`,
		"sets":                       `sets.contains(['a', 'b'], ['a'])`,
		"regular expressions":        `node.metadata.labels['zone'].find('[a-z]+-[a-z]+') == 'eu-west'`,
		"URLs":                       `url('https://kured.dev/docs').getHostname() == 'kured.dev'`,
		"quantities":                 `quantity('1Gi').isGreaterThan(quantity('500Mi'))`,
		"IP addresses":               `isIP('192.0.2.1') && ip('192.0.2.1').family() == 4`,
		"CIDRs":                      `cidr('192.0.2.0/24').containsIP('192.0.2.1')`,
		"semantic versions":          `semver(node.metadata.labels['kernel']).isGreaterThan(semver('6.1.0'))`,
		"formats":                    `!format.dns1123Label().validate('node-1').hasValue()`,
		"optional types":             `node.metadata.labels[?'missing'].orValue('default') == 'default'`,
		"two-variable comprehension": `node.metadata.labels.all(key, value, key != '' && value != '')`,
	} {
		t.Run(name, func(t *testing.T) {
			got, err := eval(t, expression, node)
			if err != nil || !got {
				t.Errorf("Eval(%q) = %v, %v, want true", expression, got, err)
			}
		})
	}
}

// README: #cel-triggers
func TestCompileRejectsWhatCannotBeATrigger(t *testing.T) {
	for name, expression := range map[string]string{
		"a syntax error":             `node.metadata.name ==`,
		"an empty expression":        ``,
		"a string":                   `'True'`,
		"a number":                   `1 + 1`,
		"an unknown variable":        `object.metadata.name == 'node-1'`,
		"an unknown function":        `node.metadata.name.reverseWords() == 'x'`,
		"the authorization function": `authorizer.group('').resource('nodes').check('get').allowed()`,
	} {
		t.Run(name, func(t *testing.T) {
			if compiled, err := Compile(expression); err == nil {
				t.Errorf("Compile(%q) = %v, want an error", expression, compiled)
			}
		})
	}
}

// An evaluation that errors has no result: the caller fails closed.
//
// README: #cel-triggers
func TestEvalErrors(t *testing.T) {
	// Scanning a string costs a tenth of its length: little time, much cost.
	annotated := nodeWith()
	annotated.Annotations = map[string]string{"big": strings.Repeat("a", 1_000_000)}
	for _, tt := range []struct {
		name       string
		expression string
		node       *corev1.Node
		want       string
	}{
		{"reading an absent field", `node.spec.taints.size() == 0`, nodeWith(), "no such key"},
		{"reading a status field that kumaco does not provide", `node.status.nodeInfo.kubeletVersion == 'v1.37.0'`, nodeWith(), "no such key"},
		{"a result that is not a bool", `node.metadata.name`, nodeWith(), "bool"},
		{"a division by zero", `1 / node.status.conditions.size() == 1`, nodeWith(), "zero"},
		{"exceeding the cost limit", `[1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12].all(i, !node.metadata.annotations['big'].contains('needle'))`,
			annotated, "cost"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := eval(t, tt.expression, tt.node)
			if err == nil {
				t.Fatalf("Eval(%q) = %v, want an error", tt.expression, got)
			}
			if got || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Eval(%q) = %v, %q, want false and an error mentioning %q", tt.expression, got, err, tt.want)
			}
		})
	}
}

// Evaluations stop at their deadline: Eval gives each of them Timeout, within
// the context of the caller.
//
// README: #cel-triggers
func TestEvalStopsAtTheDeadline(t *testing.T) {
	if Timeout != 100*time.Millisecond {
		t.Errorf("Timeout = %s, want the 100ms of the README", Timeout)
	}
	compiled, err := Compile(`node.metadata.labels.all(a, node.metadata.labels.all(b, a != b || a == b))`)
	if err != nil {
		t.Fatal(err)
	}
	node := nodeWith()
	node.Labels = map[string]string{}
	for i := range 20 {
		node.Labels[fmt.Sprintf("label-%d", i)] = "x"
	}
	if got, err := compiled.Eval(t.Context(), variable(t, node)); err != nil || !got {
		t.Fatalf("Eval = %v, %v, want true within the deadline", got, err)
	}

	expired, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancel()
	if got, err := compiled.Eval(expired, variable(t, node)); err == nil || got {
		t.Errorf("Eval after the deadline = %v, %v, want an error", got, err)
	}
}

// README: #cel-triggers
func FuzzCompile(f *testing.F) {
	for _, seed := range []string{rebootRequired, rebootDone, inhibitorMissing, `node.spec.unschedulable`, `has(node.spec.taints)`, `'a'`} {
		f.Add(seed)
	}
	node := nodeWith(condition("Ready", corev1.ConditionTrue))
	f.Fuzz(func(t *testing.T, expression string) {
		compiled, err := Compile(expression)
		if err != nil {
			return
		}
		if got, err := compiled.Eval(t.Context(), variable(t, node)); err != nil && got {
			t.Fatalf("Eval(%q) = true with the error %v, want false", expression, err)
		}
	})
}
