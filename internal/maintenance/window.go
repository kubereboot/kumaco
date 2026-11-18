// Package maintenance holds the maintenance decisions of kumaco and their
// invariants, as pure functions: which conditions a node must have, which
// nodes a window keeps, releases, and starts. It does no Kubernetes I/O.
// See README.md, "How does it work?".
package maintenance

import (
	"fmt"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/validation"

	kumacov1alpha1 "github.com/kubereboot/kumaco/api/v1alpha1"
	"github.com/kubereboot/kumaco/internal/schedule"
	"github.com/kubereboot/kumaco/internal/trigger"
)

// The bounds of spec.duration.
const (
	MinDuration = time.Minute
	MaxDuration = schedule.MaxChain
)

// Compiled is the usable form of the spec of a valid window.
type Compiled struct {
	ConditionType string
	Selector      labels.Selector
	Schedule      *schedule.Schedule
	Duration      time.Duration
	Concurrency   int
	StartTriggers []*trigger.Expression
	StopTriggers  []*trigger.Expression
}

// Window is a valid window at one point in time: its spec, and the situation
// of its schedule.
type Window struct {
	Name string
	*Compiled
	schedule.Evaluation
}

// Compile validates a spec and returns its usable form. An error means that
// the window is Invalid; its text is for humans. It checks again what the CRD
// schema checks when a decision relies on it.
func Compile(spec *kumacov1alpha1.MaintenanceWindowSpec, prefix string) (*Compiled, error) {
	if err := CheckConditionType(spec.ConditionType, prefix); err != nil {
		return nil, err
	}
	compiled := &Compiled{ConditionType: spec.ConditionType, Duration: spec.Duration.Duration}
	if compiled.Duration < MinDuration || compiled.Duration > MaxDuration {
		return nil, fmt.Errorf("duration %s is not from %s to %s", compiled.Duration, MinDuration, MaxDuration)
	}
	if spec.Concurrency == nil || *spec.Concurrency < 0 {
		return nil, fmt.Errorf("concurrency is not set to 0 or more")
	}
	compiled.Concurrency = int(*spec.Concurrency)

	var err error
	if compiled.Schedule, err = schedule.Parse(spec.StartTime); err != nil {
		return nil, fmt.Errorf("startTime: %w", err)
	}
	if compiled.Selector, err = metav1.LabelSelectorAsSelector(&spec.NodeSelector); err != nil {
		return nil, fmt.Errorf("nodeSelector: %w", err)
	}
	if compiled.StartTriggers, err = compileTriggers("startTriggers", spec.StartTriggers); err != nil {
		return nil, err
	}
	if compiled.StopTriggers, err = compileTriggers("stopTriggers", spec.StopTriggers); err != nil {
		return nil, err
	}
	return compiled, nil
}

// CheckConditionType requires a condition type under the reserved prefix.
// Without this, a maintenance manager could make the controller write or
// delete the conditions of other owners.
func CheckConditionType(conditionType, prefix string) error {
	if !strings.HasPrefix(conditionType, prefix) || conditionType == prefix {
		return fmt.Errorf("conditionType %q does not start with the reserved prefix %q of this controller, followed by a name",
			conditionType, prefix)
	}
	if problems := validation.IsQualifiedName(conditionType); len(problems) > 0 {
		return fmt.Errorf("conditionType %q is not a qualified name: %s", conditionType, strings.Join(problems, "; "))
	}
	return nil
}

func compileTriggers(field string, expressions []string) ([]*trigger.Expression, error) {
	compiled := make([]*trigger.Expression, len(expressions))
	for i, expression := range expressions {
		var err error
		if compiled[i], err = trigger.Compile(expression); err != nil {
			return nil, fmt.Errorf("%s[%d]: %w", field, i, err)
		}
	}
	return compiled, nil
}

// At returns the window at the given time.
func (c *Compiled) At(name string, now time.Time) *Window {
	return &Window{Name: name, Compiled: c, Evaluation: c.Schedule.Evaluate(c.Duration, now)}
}
