//go:build envtest

package controller

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/yaml"

	kumacov1alpha1 "github.com/kubereboot/kumaco/api/v1alpha1"
)

func stateIs(state kumacov1alpha1.MaintenanceWindowState) func(kumacov1alpha1.MaintenanceWindowStatus) bool {
	return func(status kumacov1alpha1.MaintenanceWindowStatus) bool { return status.State == state }
}

// An Invalid window starts no maintenance and keeps no node under maintenance.
//
// README: #reconciling-a-window, #the-kumacokureddev-maintenancewindow-crd, #time-zones, #debugging-with-kumaco-events, #metrics
func TestAnInvalidWindowKeepsNothing(t *testing.T) {
	h := newHarness(t)
	h.node("a")
	h.window("reboots", nil)
	controller := h.start(nil)
	h.wantConditions("a", reboot+"=True", generic+"=True")
	h.wantStatus("reboots", "InProgress", stateIs(kumacov1alpha1.StateInProgress))

	// A CEL compile error makes the window Invalid.
	h.edit("reboots", func(spec *kumacov1alpha1.MaintenanceWindowSpec) { spec.StartTriggers = []string{"node."} })
	status := h.wantStatus("reboots", "Invalid", stateIs(kumacov1alpha1.StateInvalid))
	if !strings.Contains(status.Message, "startTriggers[0]") || status.NodeCounts != nil || len(status.NodesUnderMaintenance) != 0 {
		t.Errorf("status = %+v, want a message naming startTriggers[0] and no node", status)
	}
	h.wantConditions("a", generic+"=False")
	h.wantEvent("reboots", "Normal MaintenanceReleased", h.nodeName("a"), "Invalid")
	h.eventually("kumaco_maintenance_window_invalid to be 1", func() (bool, string) {
		invalid, _ := controller.metric(t, "kumaco_maintenance_window_invalid")
		active, _ := controller.metric(t, "kumaco_maintenance_window_active")
		return invalid == 1 && active == 0, fmt.Sprint(invalid, active)
	})

	// So does an unparsable schedule, and an unknown time zone.
	for _, startTime := range []string{"0 0 2 * * *", "CRON_TZ=Local 0 * * * * ? *"} {
		h.edit("reboots", func(spec *kumacov1alpha1.MaintenanceWindowSpec) { spec.StartTriggers, spec.StartTime = nil, startTime })
		status = h.wantStatus("reboots", "Invalid", stateIs(kumacov1alpha1.StateInvalid))
		if !strings.Contains(status.Message, "startTime") {
			t.Errorf("startTime %q: message %q, want it to name startTime", startTime, status.Message)
		}
	}

	// Once fixed, the window works again. The time zone database knows Brussels.
	h.edit("reboots", func(spec *kumacov1alpha1.MaintenanceWindowSpec) { spec.StartTime = "CRON_TZ=Europe/Brussels " + always })
	status = h.wantStatus("reboots", "InProgress", stateIs(kumacov1alpha1.StateInProgress))
	if status.Message != "" {
		t.Errorf("message = %q, want it empty when the window is not Invalid", status.Message)
	}
	h.wantConditions("a", reboot+"=True", generic+"=True")
}

// When the conditionType of a window is edited, the nodes it kept under its
// previous kind are released, unless another window of that kind keeps them.
//
// README: #gotchas, #one-function-one-write-per-node
func TestAnEditedConditionTypeReleasesThePreviousKind(t *testing.T) {
	h := newHarness(t)
	h.node("a")
	h.window("reboots", nil)
	h.start(nil)
	h.wantConditions("a", reboot+"=True", generic+"=True")

	h.edit("reboots", func(spec *kumacov1alpha1.MaintenanceWindowSpec) { spec.ConditionType = firmware })

	h.wantConditions("a", firmware+"=True", generic+"=True")
	h.wantEvent("reboots", "Normal MaintenanceReleased", h.nodeName("a"), reboot, "conditionType")
}

// When a window is deleted, the nodes it was keeping are released, unless
// another window of the same kind keeps them.
//
// README: #gotchas, #reconciling-a-window, #metrics
func TestADeletedWindowReleasesItsNodes(t *testing.T) {
	h := newHarness(t)
	h.node("a")
	h.node("b")
	deleted := h.window("deleted", func(spec *kumacov1alpha1.MaintenanceWindowSpec) { spec.Concurrency = new(int32(2)) })
	controller := h.start(nil)
	h.wantUnder(reboot, "a", "b")
	// Another window of the same kind keeps b: it started during its occurrence.
	h.window("other", func(spec *kumacov1alpha1.MaintenanceWindowSpec) {
		spec.NodeSelector.MatchLabels = map[string]string{"name": "b"}
	})
	h.wantStatus("other", "keeping b", func(status kumacov1alpha1.MaintenanceWindowStatus) bool {
		return len(status.NodesUnderMaintenance) == 1
	})

	if err := api.Delete(context.Background(), deleted); err != nil {
		t.Fatal(err)
	}

	h.wantConditions("a", generic+"=False")
	h.wantEvent("deleted", "Normal MaintenanceReleased", h.nodeName("a"), "deleted")
	h.consistently(500*time.Millisecond, "b staying under maintenance", func() (bool, string) {
		return h.isUnder("b", reboot), fmt.Sprint(h.conditions("b"))
	})
	h.eventually("the metrics of the deleted window to be gone", func() (bool, string) {
		_, exists := controller.metric(t, "kumaco_maintenance_window_active", "window="+h.windowName("deleted"))
		return !exists, "kumaco_maintenance_window_active still has its series"
	})
}

// A node under maintenance that stops matching the nodeSelector is released,
// and so is a node under a window whose selector was edited.
//
// README: #the-kumacokureddev-maintenancewindow-crd, #what-does-it-reconcile-and-when
func TestANodeThatStopsMatchingTheSelectorIsReleased(t *testing.T) {
	h := newHarness(t)
	a := h.node("a")
	h.node("b")
	h.window("reboots", func(spec *kumacov1alpha1.MaintenanceWindowSpec) {
		spec.NodeSelector.MatchLabels = map[string]string{"name": "a"}
	})
	h.start(nil)
	h.wantConditions("a", reboot+"=True", generic+"=True")
	// The nodeSelector has precedence: b is not selected, whatever the triggers.
	h.wantConditions("b", generic+"=False")
	if status := h.wantStatus("reboots", "InProgress", stateIs(kumacov1alpha1.StateInProgress)); status.MatchingNodes != 1 {
		t.Errorf("matchingNodes = %d, want 1", status.MatchingNodes)
	}

	// A label change of the node is a relevant change.
	labelled := h.getNode("a")
	labelled.Labels["name"] = "renamed"
	if err := api.Update(context.Background(), labelled); err != nil {
		t.Fatal(err)
	}
	h.wantConditions("a", generic+"=False")
	h.wantEvent("reboots", "Normal MaintenanceReleased", a.Name, "nodeSelector")

	// The selector of the window is edited to select b.
	h.edit("reboots", func(spec *kumacov1alpha1.MaintenanceWindowSpec) {
		spec.NodeSelector.MatchLabels = map[string]string{"name": "b"}
	})
	h.wantConditions("b", reboot+"=True", generic+"=True")
}

// Each controller only manages the nodes of its node scope and the windows
// matching its window label selector. Controllers for disjoint teams run at
// the same time, each with its own concurrency pool.
//
// README: #multi-tenancy-while-doing-self-service-is-not-an-afterthought, #the-softer-approach-the-window-label-selector, #the-node-is-the-record, #configuration
func TestControllersForDisjointTeams(t *testing.T) {
	teamA, teamB := newHarness(t), newHarness(t)
	teamA.scope, teamB.scope = teamA.scope+"-a", teamB.scope+"-b"
	teamA.node("n1")
	teamA.node("n2")
	teamB.node("n1")
	teamB.node("n2")
	// Both windows select all nodes: only the scope of their controller bounds them.
	teamA.window("reboots", nil)
	teamB.window("reboots", nil)

	// Only the controller of team A runs.
	teamA.start(nil)
	teamA.wantUnder(reboot, "n1")
	teamA.wantConditions("n2", generic+"=False")
	teamA.wantStatus("reboots", "InProgress", stateIs(kumacov1alpha1.StateInProgress))
	teamA.consistently(time.Second, "team A leaving the nodes and the window of team B alone", func() (bool, string) {
		window := teamB.getWindow("reboots")
		return len(teamB.conditions("n1")) == 0 && len(teamB.conditions("n2")) == 0 && window.Status.State == "",
			fmt.Sprint(teamB.conditions("n1"), teamB.conditions("n2"), window.Status)
	})

	// The controller of team B has its own concurrency pool.
	teamB.start(nil)
	teamB.wantUnder(reboot, "n1")
	teamA.wantUnder(reboot, "n1")
}

// A controller with its own reserved prefix refuses the condition types
// outside of it.
//
// README: #the-kumacokureddev-maintenancewindow-crd, #security-and-access-rights, #configuration
func TestTheReservedPrefixOfTheController(t *testing.T) {
	const teamPrefix = "kumaco.kured.dev/team-a."
	h := newHarness(t)
	h.node("a")
	h.window("default-prefix", nil)
	h.window("team-prefix", func(spec *kumacov1alpha1.MaintenanceWindowSpec) {
		spec.ConditionType = teamPrefix + "RebootInProgress"
	})
	h.start(func(options *controllerOptions) { options.ConditionPrefix = teamPrefix })

	status := h.wantStatus("default-prefix", "Invalid", stateIs(kumacov1alpha1.StateInvalid))
	if !strings.Contains(status.Message, teamPrefix) {
		t.Errorf("message = %q, want it to name the reserved prefix", status.Message)
	}
	h.wantStatus("team-prefix", "InProgress", stateIs(kumacov1alpha1.StateInProgress))
	h.eventually("a to be under the kind of the team only", func() (bool, string) {
		got := h.conditions("a")
		return slices.Equal(got, []string{generic + "=True", teamPrefix + "RebootInProgress=True"}), fmt.Sprint(got)
	})
}

// A node can be under several kinds of maintenance. A start enqueues the other
// windows selecting the node: here, the firmware window only combines with
// another maintenance, and nothing else tells it that the reboot started.
//
// README: #what-does-it-reconcile-and-when, #reconciling-a-window, #one-function-one-write-per-node
func TestKindsCombineOnANode(t *testing.T) {
	h := newHarness(t)
	h.node("a")
	h.window("firmware", func(spec *kumacov1alpha1.MaintenanceWindowSpec) {
		spec.ConditionType = firmware
		spec.StartTriggers = []string{`node.status.conditions.exists(c, c.type == 'MaintenanceInProgress' && c.status == 'True')`}
	})
	h.start(nil)
	h.wantConditions("a", generic+"=False")

	h.window("reboots", nil)
	h.wantConditions("a", firmware+"=True", reboot+"=True", generic+"=True")
	since := h.condition("a", generic).LastTransitionTime

	// The reboot is released: the firmware kind is carried over, and
	// MaintenanceInProgress stays True, since the same time.
	h.edit("reboots", func(spec *kumacov1alpha1.MaintenanceWindowSpec) { spec.StopTriggers = []string{"true"} })
	h.wantConditions("a", firmware+"=True", generic+"=True")
	if got := h.condition("a", generic).LastTransitionTime; !got.Equal(&since) {
		t.Errorf("MaintenanceInProgress changed its lastTransitionTime from %s to %s, although its status did not change", since, got)
	}
}

// A window is requeued at its next schedule boundary: it becomes InProgress
// without any event, and stops keeping its nodes when its duration elapsed.
//
// README: #reconciling-a-window, #what-does-it-reconcile-and-when, #when-does-a-window-keep-a-node-under-maintenance
func TestScheduleBoundaries(t *testing.T) {
	h := newHarness(t)
	h.node("a")
	// The controller's clock can be moved forward.
	var ahead atomic.Int64
	h.start(func(options *controllerOptions) {
		options.Now = func() time.Time { return time.Now().Add(time.Duration(ahead.Load())) }
	})

	// A one-off window that starts in a few seconds.
	start := time.Now().UTC().Add(3 * time.Second)
	h.window("once", func(spec *kumacov1alpha1.MaintenanceWindowSpec) {
		spec.StartTime = fmt.Sprintf("%d %d %d %d %d ? %d", start.Second(), start.Minute(), start.Hour(), start.Day(), int(start.Month()), start.Year())
		spec.Duration.Duration = time.Minute
	})
	h.wantStatus("once", "Scheduled", stateIs(kumacov1alpha1.StateScheduled))
	h.wantConditions("a", generic+"=False")

	h.wantStatus("once", "InProgress", stateIs(kumacov1alpha1.StateInProgress))
	if time.Now().Before(start.Truncate(time.Second)) {
		t.Errorf("the window is InProgress before its start time")
	}
	h.wantConditions("a", reboot+"=True", generic+"=True")

	// Two hours later, its duration elapsed and it has no other occurrence.
	// Any reconcile notices it; here, a node change.
	ahead.Store(int64(2 * time.Hour))
	h.report("a", ready+"=True")
	status := h.wantStatus("once", "Closed", stateIs(kumacov1alpha1.StateClosed))
	if status.NodeCounts != nil {
		t.Errorf("nodeCounts = %v, want none while the window is not InProgress", status.NodeCounts)
	}
	h.wantConditions("a", generic+"=False")
	h.wantEvent("once", "Normal MaintenanceReleased", h.nodeName("a"), "not in progress")
}

// repositoryFile reads a file of the repository.
func repositoryFile(t *testing.T, path string) []byte {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	content, err := os.ReadFile(filepath.Join(filepath.Dir(thisFile), "..", "..", path))
	if err != nil {
		t.Fatal(err)
	}
	return content
}

// serviceAccountConfig creates a ServiceAccount bound to a ClusterRole with
// the given rules, and returns the configuration to act as it.
func (h *harness) serviceAccountConfig(rules []rbacv1.PolicyRule) *rest.Config {
	h.t.Helper()
	ctx := context.Background()
	account := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: h.scope, Namespace: metav1.NamespaceDefault}}
	role := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: h.scope}, Rules: rules}
	binding := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: h.scope},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: role.Name},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: account.Name, Namespace: account.Namespace}},
	}
	if err := api.Create(ctx, account); err != nil {
		h.t.Fatal(err)
	}
	if err := api.Create(ctx, role); err != nil {
		h.t.Fatal(err)
	}
	if err := api.Create(ctx, binding); err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() {
		_ = api.Delete(ctx, binding)
		_ = api.Delete(ctx, role)
		_ = api.Delete(ctx, account)
	})
	token, err := clientset.CoreV1().ServiceAccounts(account.Namespace).CreateToken(ctx, account.Name, &authenticationv1.TokenRequest{}, metav1.CreateOptions{})
	if err != nil {
		h.t.Fatalf("requesting a token: %v", err)
	}
	config := rest.AnonymousClientConfig(restConfig)
	config.BearerToken = token.Status.Token
	config.QPS, config.Burst = 1000, 2000
	return config
}

// generatedRules returns the rules of the ClusterRole that the manifests ship.
func generatedRules(t *testing.T) []rbacv1.PolicyRule {
	t.Helper()
	var role rbacv1.ClusterRole
	if err := yaml.Unmarshal(repositoryFile(t, "config/rbac/role.yaml"), &role); err != nil {
		t.Fatal(err)
	}
	return role.Rules
}

// The controller works with the access rights that the manifests give it, and
// no more: it watches its windows and the nodes, patches the status of both,
// and emits events.
//
// README: #security-and-access-rights
func TestTheGeneratedRoleIsSufficient(t *testing.T) {
	h := newHarness(t)
	h.node("a", rebootable...)
	h.kuredWindow("reboots", 1)
	config := h.serviceAccountConfig(generatedRules(t))
	h.start(func(options *controllerOptions) { options.config = config })

	h.wantConditions("a", reboot+"=True", generic+"=True")
	h.wantStatus("reboots", "InProgress", stateIs(kumacov1alpha1.StateInProgress))
	h.wantEvent("reboots", "Normal MaintenanceStarted", h.nodeName("a"))
	h.report("a", required+"=False")
	h.wantConditions("a", generic+"=False")
}

// RBAC restricts the node writes of a controller to named nodes: a controller
// cannot change a node of another team, even when it is in its node scope by
// mistake and a window selects it. It then fails closed: it cannot finish its
// startup sweep, and starts nothing. A maintenance manager needs no node
// access at all.
//
// README: #the-harder-approach-the-service-account-limitations, #security-and-access-rights
func TestNodeWritesAreRestrictedToNamedNodes(t *testing.T) {
	h := newHarness(t)
	h.node("mine")
	h.node("theirs")
	h.window("reboots", func(spec *kumacov1alpha1.MaintenanceWindowSpec) { spec.Concurrency = new(int32(2)) })

	rules := generatedRules(t)
	for i := range rules {
		if slices.Contains(rules[i].Resources, "nodes/status") {
			rules[i].ResourceNames = []string{h.nodeName("mine")}
		}
	}
	config := h.serviceAccountConfig(rules)
	controller := h.start(func(options *controllerOptions) { options.config = config })

	h.eventually("refused writes on the node of the other team", func() (bool, string) {
		errors, _ := controller.metric(t, "kumaco_node_write_total", "result=error")
		return errors >= 2, fmt.Sprint(errors)
	})
	if got := h.conditions("theirs"); len(got) != 0 {
		t.Errorf("conditions of the node of the other team = %v, want none", got)
	}
	// Its own node is writable, but nothing starts before the sweep is done.
	h.wantConditions("mine", generic+"=False")

	// The manager of the windows cannot write nodes.
	manager := newHarness(t)
	manager.scope += "-manager"
	managerConfig := manager.serviceAccountConfig([]rbacv1.PolicyRule{{
		APIGroups: []string{kumacov1alpha1.SchemeGroupVersion.Group}, Resources: []string{"maintenancewindows"}, Verbs: []string{"*"},
	}})
	h.eventually("the rights of the maintenance manager to be in effect", func() (bool, string) {
		asManager, err := newClientset(managerConfig)
		if err != nil {
			return false, err.Error()
		}
		_, err = asManager.CoreV1().Nodes().Patch(context.Background(), h.nodeName("mine"), "application/merge-patch+json",
			[]byte(`{"status":{"conditions":[]}}`), metav1.PatchOptions{}, "status")
		return apierrors.IsForbidden(err), fmt.Sprint(err)
	})
}
