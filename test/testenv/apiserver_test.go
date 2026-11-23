//go:build envtest

package testenv

import (
	"context"
	"fmt"
	"maps"
	"os"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/component-base/version"
)

// These tests prove that the supported Kubernetes release, as pinned for
// envtest, has the API semantics that the README's design relies on. They need
// no kumaco code: they fail when a Kubernetes upgrade breaks an assumption.
// They do not cover what kumaco does with these semantics, so the sections
// describing that stay pending until the controller's own tests link to them.

const (
	fieldManager          = "kumaco" // README, "One function, one write per node"
	maintenanceInProgress = "MaintenanceInProgress"
	rebootInProgress      = "kumaco.kured.dev/RebootInProgress"
	rebootRequired        = "detectors.kured.dev/RebootRequired"
)

var apiServer kubernetes.Interface

func TestMain(m *testing.M) {
	os.Exit(runTests(m))
}

func runTests(m *testing.M) int {
	config, stop, err := Start()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer func() {
		if err := stop(); err != nil {
			fmt.Fprintln(os.Stderr, "stopping the envtest control plane:", err)
		}
	}()
	apiServer, err = kubernetes.NewForConfig(config)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return m.Run()
}

// README: #kubernetes-versions
func TestTheAPIServerIsTheSupportedKubernetesRelease(t *testing.T) {
	server, err := apiServer.Discovery().ServerVersion()
	if err != nil {
		t.Fatalf("reading the API server version: %v", err)
	}
	// The client libraries kumaco is built with name the supported release.
	if got := server.Major + "." + server.Minor; got != version.DefaultKubeBinaryVersion {
		t.Errorf("the envtest API server runs Kubernetes %s (%s), want %s, the release of the client libraries",
			got, server.GitVersion, version.DefaultKubeBinaryVersion)
	}
}

// kumaco sends the complete list of its conditions in one server-side apply on
// nodes/status. This relies on conditions being merged per type: what kumaco
// stops sending is removed, and the conditions of other writers are kept.
//
// README: #kubernetes-versions
func TestServerSideApplyMergesNodeConditionsPerType(t *testing.T) {
	ctx := t.Context()
	nodes := apiServer.CoreV1().Nodes()
	node := createNode(t, "merge-per-type")

	// Other writers: the kubelet updates the status, a reporter applies it.
	node.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}
	if _, err := nodes.UpdateStatus(ctx, node, metav1.UpdateOptions{FieldManager: "kubelet"}); err != nil {
		t.Fatalf("updating the status as the kubelet: %v", err)
	}
	reporter := corev1ac.Node(node.Name).WithStatus(corev1ac.NodeStatus().WithConditions(
		condition(rebootRequired, corev1.ConditionTrue)))
	if _, err := nodes.ApplyStatus(ctx, reporter, metav1.ApplyOptions{FieldManager: "reporter", Force: true}); err != nil {
		t.Fatalf("applying the status as a reporter: %v", err)
	}

	applyAsKumaco(t, node.Name,
		condition(rebootInProgress, corev1.ConditionTrue),
		condition(maintenanceInProgress, corev1.ConditionTrue))
	wantConditions(t, node.Name, map[corev1.NodeConditionType]corev1.ConditionStatus{
		corev1.NodeReady:      corev1.ConditionTrue,
		rebootRequired:        corev1.ConditionTrue,
		rebootInProgress:      corev1.ConditionTrue,
		maintenanceInProgress: corev1.ConditionTrue,
	})

	// The release: kumaco no longer sends the condition of the kind.
	applyAsKumaco(t, node.Name, condition(maintenanceInProgress, corev1.ConditionFalse))
	wantConditions(t, node.Name, map[corev1.NodeConditionType]corev1.ConditionStatus{
		corev1.NodeReady:      corev1.ConditionTrue,
		rebootRequired:        corev1.ConditionTrue,
		maintenanceInProgress: corev1.ConditionFalse,
	})
}

// A write that starts maintenance carries the resourceVersion its decision was
// based on, so that a concurrent change to the node rejects it.
//
// README: #kubernetes-versions
func TestServerSideApplyRejectsAStaleResourceVersion(t *testing.T) {
	ctx := t.Context()
	nodes := apiServer.CoreV1().Nodes()
	node := createNode(t, "stale-resource-version")
	staleVersion := node.ResourceVersion

	// A concurrent change, for example by the reboot reporter.
	node.Status.Conditions = []corev1.NodeCondition{{Type: rebootRequired, Status: corev1.ConditionFalse}}
	changed, err := nodes.UpdateStatus(ctx, node, metav1.UpdateOptions{FieldManager: "reporter"})
	if err != nil {
		t.Fatalf("changing the node concurrently: %v", err)
	}

	start := func(resourceVersion string) error {
		apply := corev1ac.Node(node.Name).WithResourceVersion(resourceVersion).
			WithStatus(corev1ac.NodeStatus().WithConditions(
				condition(rebootInProgress, corev1.ConditionTrue),
				condition(maintenanceInProgress, corev1.ConditionTrue)))
		_, err := nodes.ApplyStatus(ctx, apply, metav1.ApplyOptions{FieldManager: fieldManager, Force: true})
		return err
	}

	if err := start(staleVersion); !apierrors.IsConflict(err) {
		t.Fatalf("applying with a stale resourceVersion: error = %v, want a conflict", err)
	}
	// Nothing is recorded by the rejected write.
	wantConditions(t, node.Name, map[corev1.NodeConditionType]corev1.ConditionStatus{
		rebootRequired: corev1.ConditionFalse,
	})

	if err := start(changed.ResourceVersion); err != nil {
		t.Fatalf("applying with the current resourceVersion: %v", err)
	}
	wantConditions(t, node.Name, map[corev1.NodeConditionType]corev1.ConditionStatus{
		rebootRequired:        corev1.ConditionFalse,
		rebootInProgress:      corev1.ConditionTrue,
		maintenanceInProgress: corev1.ConditionTrue,
	})
}

func createNode(t *testing.T, name string) *corev1.Node {
	t.Helper()
	nodes := apiServer.CoreV1().Nodes()
	node, err := nodes.Create(t.Context(), &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("creating node %s: %v", name, err)
	}
	t.Cleanup(func() {
		// t.Context() is already cancelled in cleanups.
		if err := nodes.Delete(context.Background(), name, metav1.DeleteOptions{}); err != nil {
			t.Errorf("deleting node %s: %v", name, err)
		}
	})
	return node
}

func condition(conditionType corev1.NodeConditionType, status corev1.ConditionStatus) *corev1ac.NodeConditionApplyConfiguration {
	return corev1ac.NodeCondition().WithType(conditionType).WithStatus(status)
}

// applyAsKumaco applies the complete list of kumaco's conditions on a node.
func applyAsKumaco(t *testing.T, node string, conditions ...*corev1ac.NodeConditionApplyConfiguration) {
	t.Helper()
	apply := corev1ac.Node(node).WithStatus(corev1ac.NodeStatus().WithConditions(conditions...))
	_, err := apiServer.CoreV1().Nodes().ApplyStatus(t.Context(), apply,
		metav1.ApplyOptions{FieldManager: fieldManager, Force: true})
	if err != nil {
		t.Fatalf("applying the status of node %s as %s: %v", node, fieldManager, err)
	}
}

func wantConditions(t *testing.T, node string, want map[corev1.NodeConditionType]corev1.ConditionStatus) {
	t.Helper()
	got, err := apiServer.CoreV1().Nodes().Get(t.Context(), node, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reading node %s: %v", node, err)
	}
	statuses := map[corev1.NodeConditionType]corev1.ConditionStatus{}
	for _, c := range got.Status.Conditions {
		statuses[c.Type] = c.Status
	}
	if !maps.Equal(statuses, want) {
		t.Errorf("conditions of node %s = %v, want %v", node, statuses, want)
	}
}

// kumaco sends the UID of the node its conditions were decided for. A node
// that was deleted and created again with the same name has another UID: the
// API server must reject the write, so that the new node does not inherit the
// records of the old one. It answers Invalid, as the UID is immutable.
//
// README: #kubernetes-versions
func TestServerSideApplyRejectsANodeCreatedAgain(t *testing.T) {
	ctx := t.Context()
	nodes := apiServer.CoreV1().Nodes()
	old := createNode(t, "created-again")
	if err := nodes.Delete(ctx, old.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	again := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: old.Name}}
	if _, err := nodes.Create(ctx, again, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	apply := corev1ac.Node(old.Name).WithUID(old.UID).
		WithStatus(corev1ac.NodeStatus().WithConditions(condition(rebootInProgress, corev1.ConditionTrue)))
	_, err := nodes.ApplyStatus(ctx, apply, metav1.ApplyOptions{FieldManager: fieldManager, Force: true})

	if !apierrors.IsInvalid(err) || !apierrors.HasStatusCause(err, metav1.CauseTypeFieldValueInvalid) || !strings.Contains(err.Error(), "metadata.uid") {
		t.Fatalf("applying conditions decided for the deleted node: %v, want an Invalid error about metadata.uid", err)
	}
	wantConditions(t, old.Name, map[corev1.NodeConditionType]corev1.ConditionStatus{})
}
