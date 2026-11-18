//go:build envtest

package v1alpha1_test

import (
	"fmt"
	"os"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kumacov1alpha1 "github.com/kubereboot/kumaco/api/v1alpha1"
	"github.com/kubereboot/kumaco/test/testenv"
)

var apiServer client.Client

func TestMain(m *testing.M) {
	os.Exit(runTests(m))
}

func runTests(m *testing.M) int {
	config, stop, err := testenv.Start()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer func() {
		if err := stop(); err != nil {
			fmt.Fprintln(os.Stderr, "stopping the envtest control plane:", err)
		}
	}()
	if err := testenv.InstallCRD(config); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	scheme := runtime.NewScheme()
	if err := kumacov1alpha1.AddToScheme(scheme); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	apiServer, err = client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return m.Run()
}

// validSpec is the spec of a window that the schema accepts. The tests change
// one field of it at a time.
func validSpec() map[string]any {
	return map[string]any{
		"startTime":     "CRON_TZ=Europe/Brussels 0 0 2 * * ? *",
		"duration":      "2h30m",
		"nodeSelector":  map[string]any{},
		"conditionType": "kumaco.kured.dev/RebootInProgress",
		"concurrency":   int64(1),
	}
}

func window(name string, spec map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kumaco.kured.dev/v1alpha1",
		"kind":       "MaintenanceWindow",
		"metadata":   map[string]any{"name": name},
		"spec":       spec,
	}}
}

func repeat(expression string, count int) []any {
	list := make([]any, count)
	for i := range list {
		list[i] = expression
	}
	return list
}

// README: #the-kumacokureddev-maintenancewindow-crd
func TestTheCRDSchemaValidatesTheSpec(t *testing.T) {
	without := func(field string) func(map[string]any) {
		return func(spec map[string]any) { delete(spec, field) }
	}
	with := func(field string, value any) func(map[string]any) {
		return func(spec map[string]any) { spec[field] = value }
	}
	for _, tt := range []struct {
		name   string
		change func(spec map[string]any)
		// wantErr is a part of the expected rejection; empty when the window is accepted.
		wantErr string
	}{
		{name: "the valid spec", change: func(map[string]any) {}},

		{name: "startTime is required", change: without("startTime"), wantErr: "spec.startTime"},
		{name: "startTime is not empty", change: with("startTime", ""), wantErr: "spec.startTime"},
		{name: "startTime of 256 characters", change: with("startTime", strings.Repeat("0", 256))},
		{name: "startTime of 257 characters", change: with("startTime", strings.Repeat("0", 257)), wantErr: "spec.startTime"},

		{name: "duration is required", change: without("duration"), wantErr: "spec.duration"},
		{name: "duration of 1m", change: with("duration", "1m")},
		{name: "duration of 59s", change: with("duration", "59s"), wantErr: "from 1m to 744h"},
		{name: "duration of 744h", change: with("duration", "744h")},
		{name: "duration of 744h1s", change: with("duration", "744h1s"), wantErr: "from 1m to 744h"},
		{name: "duration that is not a duration", change: with("duration", "tomorrow"), wantErr: "spec.duration"},

		{name: "nodeSelector is required", change: without("nodeSelector"), wantErr: "spec.nodeSelector"},
		{name: "nodeSelector with labels", change: with("nodeSelector", map[string]any{"matchLabels": map[string]any{"team": "a"}})},

		{name: "conditionType is required", change: without("conditionType"), wantErr: "spec.conditionType"},
		{name: "conditionType without the reserved prefix", change: with("conditionType", "Ready"), wantErr: "spec.conditionType"},
		{name: "conditionType of another owner", change: with("conditionType", "detectors.kured.dev/RebootRequired"), wantErr: "spec.conditionType"},
		{name: "conditionType that is only the prefix", change: with("conditionType", "kumaco.kured.dev/"), wantErr: "spec.conditionType"},
		{name: "conditionType that is not a qualified name", change: with("conditionType", "kumaco.kured.dev/Reboot In Progress"), wantErr: "spec.conditionType"},
		{name: "conditionType longer than 316 characters", change: with("conditionType", "kumaco.kured.dev/"+strings.Repeat("a", 300)), wantErr: "spec.conditionType"},

		{name: "no startTriggers", change: without("startTriggers")},
		{name: "empty startTriggers", change: with("startTriggers", []any{})},
		{name: "16 startTriggers", change: with("startTriggers", repeat("true", 16))},
		{name: "17 startTriggers", change: with("startTriggers", repeat("true", 17)), wantErr: "spec.startTriggers"},
		{name: "a startTriggers expression of 4096 characters", change: with("startTriggers", []any{strings.Repeat("a", 4096)})},
		{name: "a startTriggers expression of 4097 characters", change: with("startTriggers", []any{strings.Repeat("a", 4097)}), wantErr: "spec.startTriggers"},

		{name: "16 stopTriggers", change: with("stopTriggers", repeat("true", 16))},
		{name: "17 stopTriggers", change: with("stopTriggers", repeat("true", 17)), wantErr: "spec.stopTriggers"},
		{name: "a stopTriggers expression of 4097 characters", change: with("stopTriggers", []any{strings.Repeat("a", 4097)}), wantErr: "spec.stopTriggers"},

		{name: "concurrency is required", change: without("concurrency"), wantErr: "spec.concurrency"},
		{name: "concurrency of 0", change: with("concurrency", int64(0))},
		{name: "concurrency of 1000", change: with("concurrency", int64(1000))},
		{name: "concurrency of 1001", change: with("concurrency", int64(1001)), wantErr: "spec.concurrency"},
		{name: "negative concurrency", change: with("concurrency", int64(-1)), wantErr: "spec.concurrency"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			spec := validSpec()
			tt.change(spec)
			object := window("", spec)
			object.SetGenerateName("schema-")

			err := apiServer.Create(t.Context(), object)
			if err == nil {
				t.Cleanup(func() { _ = apiServer.Delete(t.Context(), object) })
			}
			switch {
			case tt.wantErr == "" && err != nil:
				t.Errorf("the API server rejected the window: %v", err)
			case tt.wantErr != "" && err == nil:
				t.Errorf("the API server accepted the window, want a rejection mentioning %q", tt.wantErr)
			case tt.wantErr != "" && (!apierrors.IsInvalid(err) || !strings.Contains(err.Error(), tt.wantErr)):
				t.Errorf("the API server answered %v, want an Invalid error mentioning %q", err, tt.wantErr)
			}
		})
	}
}

// The window is cluster-scoped, its typed client round-trips a spec with
// `concurrency: 0`, and its status is a subresource: a write to the window
// does not change the status, and a status write does not change the spec.
//
// README: #the-kumacokureddev-maintenancewindow-crd, #api-versions
func TestTheStatusIsASubresourceOfAClusterScopedWindow(t *testing.T) {
	ctx := t.Context()
	zero := int32(0)
	created := &kumacov1alpha1.MaintenanceWindow{}
	created.Name = "status-subresource"
	created.Spec = kumacov1alpha1.MaintenanceWindowSpec{
		StartTime:     "0 0 2 * * ? *",
		ConditionType: "kumaco.kured.dev/RebootInProgress",
		Concurrency:   &zero,
	}
	created.Spec.Duration.Duration = 2 * 60 * 60 * 1e9
	created.Status.State = kumacov1alpha1.StateInProgress
	if err := apiServer.Create(ctx, created); err != nil {
		t.Fatalf("creating the window: %v", err)
	}
	t.Cleanup(func() { _ = apiServer.Delete(ctx, created) })
	if created.Namespace != "" {
		t.Errorf("the window is in the namespace %q, want a cluster-scoped resource", created.Namespace)
	}
	if created.Status.State != "" {
		t.Errorf("creating the window set status.state to %q, want the status to be ignored", created.Status.State)
	}
	if created.Spec.Concurrency == nil || *created.Spec.Concurrency != 0 {
		t.Errorf("spec.concurrency = %v, want 0", created.Spec.Concurrency)
	}

	created.Status = kumacov1alpha1.MaintenanceWindowStatus{
		ObservedGeneration:    created.Generation,
		State:                 kumacov1alpha1.StateInProgress,
		NodesUnderMaintenance: []string{"node-1"},
		MatchingNodes:         3,
		NodeCounts:            map[string]int32{kumacov1alpha1.SituationUnderMaintenance: 1},
	}
	if err := apiServer.Status().Update(ctx, created); err != nil {
		t.Fatalf("writing the status: %v", err)
	}

	got := &kumacov1alpha1.MaintenanceWindow{}
	if err := apiServer.Get(ctx, client.ObjectKeyFromObject(created), got); err != nil {
		t.Fatalf("reading the window back: %v", err)
	}
	if got.Status.State != kumacov1alpha1.StateInProgress || got.Status.MatchingNodes != 3 ||
		len(got.Status.NodesUnderMaintenance) != 1 || got.Status.NodeCounts[kumacov1alpha1.SituationUnderMaintenance] != 1 {
		t.Errorf("status = %+v, want the status that was written", got.Status)
	}
	if got.Generation != created.Generation {
		t.Errorf("the status write changed metadata.generation from %d to %d", created.Generation, got.Generation)
	}

	got.Status.State = "Paused"
	if err := apiServer.Status().Update(ctx, got); !apierrors.IsInvalid(err) {
		t.Errorf("writing status.state = Paused: %v, want an Invalid error", err)
	}
}
