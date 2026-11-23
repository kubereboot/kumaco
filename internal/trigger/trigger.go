// Package trigger compiles and evaluates the CEL expressions of startTriggers
// and stopTriggers. See README.md, "CEL triggers".
package trigger

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/google/cel-go/cel"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/version"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
	"k8s.io/apiserver/pkg/cel/environment"
)

// Timeout is the longest evaluation of one expression on one node.
const Timeout = 100 * time.Millisecond

// nodeVariable is the only variable that expressions see.
const nodeVariable = "node"

// env is Kubernetes' CEL environment, in the version of the libraries kumaco is
// built with, plus the node variable. Its program options hold the cost limit
// of one evaluation (celconfig.PerCallLimit) and Kubernetes' cost estimator.
//
// The node is declared as dyn: declaring its fields would need the schema of
// the whole node spec. An expression whose type cannot be known before it runs
// (`node.spec.unschedulable`) must therefore return a bool when it runs.
var env = sync.OnceValues(func() (*cel.Env, error) {
	set, err := environment.MustBaseEnvSet(environment.DefaultCompatibilityVersion()).Extend(environment.VersionedOptions{
		IntroducedVersion: version.MajorMinor(1, 0),
		EnvOptions:        []cel.EnvOption{cel.Variable(nodeVariable, cel.DynType)},
	})
	if err != nil {
		return nil, err
	}
	return set.Env(environment.StoredExpressions)
})

// Expression is a compiled trigger.
type Expression struct {
	source  string
	program cel.Program
}

// Compile compiles an expression, which must return a bool.
func Compile(expression string) (*Expression, error) {
	env, err := env()
	if err != nil {
		return nil, fmt.Errorf("building the CEL environment: %w", err)
	}
	ast, issues := env.Compile(expression)
	if err := issues.Err(); err != nil {
		return nil, err
	}
	if output := ast.OutputType(); !output.IsExactType(cel.BoolType) && !output.IsExactType(cel.DynType) {
		return nil, fmt.Errorf("the expression returns %s, want bool", output)
	}
	program, err := env.Program(ast, cel.InterruptCheckFrequency(celconfig.CheckFrequency))
	if err != nil {
		return nil, err
	}
	return &Expression{source: expression, program: program}, nil
}

// String returns the source of the expression.
func (e *Expression) String() string { return e.source }

// Eval evaluates the expression on a node, as NodeVariable returns it, within
// Timeout and the cost limit. An error means that there is no result.
func (e *Expression) Eval(ctx context.Context, node map[string]any) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	// A program only looks at its context from time to time, in comprehensions.
	if err := ctx.Err(); err != nil {
		return false, err
	}
	result, _, err := e.program.ContextEval(ctx, map[string]any{nodeVariable: node})
	if err != nil {
		return false, err
	}
	value, isBool := result.Value().(bool)
	if !isBool {
		return false, fmt.Errorf("the expression returned %s, want bool", result.Type().TypeName())
	}
	return value, nil
}

// NodeVariable returns the reduced copy of a node that expressions see. It
// does not keep a reference to the node.
func NodeVariable(node *corev1.Node) (map[string]any, error) {
	spec, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&node.Spec)
	if err != nil {
		return nil, fmt.Errorf("converting the spec of node %s: %w", node.Name, err)
	}
	spec["unschedulable"] = node.Spec.Unschedulable

	conditions := make([]any, len(node.Status.Conditions))
	for i, condition := range node.Status.Conditions {
		conditions[i] = map[string]any{
			"type":               string(condition.Type),
			"status":             string(condition.Status),
			"reason":             condition.Reason,
			"message":            condition.Message,
			"lastHeartbeatTime":  rfc3339(condition.LastHeartbeatTime),
			"lastTransitionTime": rfc3339(condition.LastTransitionTime),
		}
	}
	return map[string]any{
		"metadata": map[string]any{
			"name":        node.Name,
			"labels":      copyOf(node.Labels),
			"annotations": copyOf(node.Annotations),
		},
		"spec":   spec,
		"status": map[string]any{"conditions": conditions},
	}, nil
}

// rfc3339 returns the empty string for a time that is not set.
func rfc3339(t metav1.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func copyOf(source map[string]string) map[string]any {
	result := make(map[string]any, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}
