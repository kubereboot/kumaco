package maintenance

import (
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	kumacov1alpha1 "github.com/kubereboot/kumaco/api/v1alpha1"
)

func validSpec() *kumacov1alpha1.MaintenanceWindowSpec {
	concurrency := int32(3)
	return &kumacov1alpha1.MaintenanceWindowSpec{
		StartTime:     "CRON_TZ=Europe/Brussels 0 0 2 * * ? *",
		Duration:      metav1.Duration{Duration: 2 * time.Hour},
		NodeSelector:  metav1.LabelSelector{MatchLabels: map[string]string{"team": "a"}},
		ConditionType: reboot,
		StartTriggers: []string{rebootRequired},
		StopTriggers:  []string{rebootDone},
		Concurrency:   &concurrency,
	}
}

// README: #the-kumacokureddev-maintenancewindow-crd
func TestCompile(t *testing.T) {
	compiled, err := Compile(validSpec(), prefix)
	if err != nil {
		t.Fatal(err)
	}
	if compiled.ConditionType != reboot || compiled.Concurrency != 3 || compiled.Duration != 2*time.Hour ||
		len(compiled.StartTriggers) != 1 || len(compiled.StopTriggers) != 1 {
		t.Errorf("compiled = %+v", compiled)
	}
	if !compiled.Selector.Matches(labels.Set{"team": "a"}) || compiled.Selector.Matches(labels.Set{"team": "b"}) {
		t.Errorf("selector = %s, want team=a", compiled.Selector)
	}

	inProgress := compiled.At("reboots", time.Date(2026, 11, 18, 1, 30, 0, 0, time.UTC)) // 02:30 in Brussels
	if inProgress.Name != "reboots" || !inProgress.Active {
		t.Errorf("window at 02:30 Brussels time = %+v, want it active", inProgress.Evaluation)
	}
	if scheduled := compiled.At("reboots", time.Date(2026, 11, 18, 3, 30, 0, 0, time.UTC)); scheduled.Active || scheduled.NextBoundary.IsZero() {
		t.Errorf("window at 04:30 Brussels time = %+v, want it scheduled", scheduled.Evaluation)
	}
}

// What the CRD schema cannot check makes the window Invalid, with a message
// for humans.
//
// README: #the-kumacokureddev-maintenancewindow-crd, #cel-triggers, #time-zones
func TestCompileRejectsAnUnusableSpec(t *testing.T) {
	for _, tt := range []struct {
		name   string
		prefix string
		change func(spec *kumacov1alpha1.MaintenanceWindowSpec)
		want   string
	}{
		{"an unparsable schedule", prefix,
			func(spec *kumacov1alpha1.MaintenanceWindowSpec) { spec.StartTime = "0 0 2 * * *" }, "startTime"},
		{"an unknown time zone", prefix,
			func(spec *kumacov1alpha1.MaintenanceWindowSpec) { spec.StartTime = "CRON_TZ=Local 0 0 2 * * ? *" }, "startTime"},
		{"a CEL compile error in startTriggers", prefix,
			func(spec *kumacov1alpha1.MaintenanceWindowSpec) { spec.StartTriggers = []string{"true", "node."} }, "startTriggers[1]"},
		{"a CEL expression that is not a bool in stopTriggers", prefix,
			func(spec *kumacov1alpha1.MaintenanceWindowSpec) { spec.StopTriggers = []string{"'True'"} }, "stopTriggers[0]"},
		{"a conditionType without the reserved prefix", prefix,
			func(spec *kumacov1alpha1.MaintenanceWindowSpec) { spec.ConditionType = "Ready" }, "conditionType"},
		{"a conditionType of another owner", prefix,
			func(spec *kumacov1alpha1.MaintenanceWindowSpec) { spec.ConditionType = required }, "conditionType"},
		{"a conditionType without the prefix of this controller", "kumaco.kured.dev/team-a.",
			func(spec *kumacov1alpha1.MaintenanceWindowSpec) {}, "kumaco.kured.dev/team-a."},
		{"a conditionType that is only the prefix", prefix,
			func(spec *kumacov1alpha1.MaintenanceWindowSpec) { spec.ConditionType = prefix }, "conditionType"},
		{"a conditionType that is not a qualified name", prefix,
			func(spec *kumacov1alpha1.MaintenanceWindowSpec) { spec.ConditionType = prefix + "Reboot In Progress" }, "conditionType"},
		{"an invalid nodeSelector", prefix,
			func(spec *kumacov1alpha1.MaintenanceWindowSpec) {
				spec.NodeSelector.MatchExpressions = []metav1.LabelSelectorRequirement{{Key: "team", Operator: "Near"}}
			}, "nodeSelector"},
		{"no concurrency", prefix,
			func(spec *kumacov1alpha1.MaintenanceWindowSpec) { spec.Concurrency = nil }, "concurrency"},
		{"a duration outside of the bounds", prefix,
			func(spec *kumacov1alpha1.MaintenanceWindowSpec) { spec.Duration.Duration = 0 }, "duration"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			spec := validSpec()
			tt.change(spec)
			compiled, err := Compile(spec, tt.prefix)
			if err == nil {
				t.Fatalf("Compile = %+v, want an error", compiled)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %q, want it to mention %q", err, tt.want)
			}
		})
	}
}

// README: #the-kumacokureddev-maintenancewindow-crd
func TestCompileAcceptsThePrefixOfTheController(t *testing.T) {
	spec := validSpec()
	spec.ConditionType = "kumaco.kured.dev/team-a.RebootInProgress"
	if _, err := Compile(spec, "kumaco.kured.dev/team-a."); err != nil {
		t.Error(err)
	}
}
