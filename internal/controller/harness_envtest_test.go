//go:build envtest

package controller

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	kumacov1alpha1 "github.com/kubereboot/kumaco/api/v1alpha1"
	"github.com/kubereboot/kumaco/test/testenv"
)

const (
	prefix   = kumacov1alpha1.DefaultConditionPrefix
	reboot   = "kumaco.kured.dev/RebootInProgress"
	firmware = "kumaco.kured.dev/FirmwareInProgress"
	generic  = string(corev1.NodeMaintenanceInProgress)

	required = "detectors.kured.dev/RebootRequired"
	noPod    = "detectors.kured.dev/NoPodInhibitor"
	noAlert  = "detectors.kured.dev/NoAlertInhibitor"
	ready    = "Ready"

	// scopeLabel separates the tests: each controller manages the nodes and
	// the windows carrying its own value.
	scopeLabel = "kumaco-test"

	// always is a schedule that is always in progress with a duration of one
	// hour: its occurrences form a chain.
	always = "0 * * * * ? *"
)

// The triggers that the README documents for Kured.
var (
	kuredStart = []string{
		`node.status.conditions.exists(c, c.type == 'detectors.kured.dev/RebootRequired' && c.status == 'True')`,
		`node.status.conditions.exists(c, c.type == 'detectors.kured.dev/NoPodInhibitor' && c.status == 'True')`,
		`node.status.conditions.exists(c, c.type == 'detectors.kured.dev/NoAlertInhibitor' && c.status == 'True')`,
	}
	kuredStop = []string{
		`!node.status.conditions.exists(c, c.type == 'detectors.kured.dev/RebootRequired' && c.status == 'True') &&
node.status.conditions.exists(c, c.type == 'Ready' && c.status == 'True')`,
	}
)

var (
	restConfig *rest.Config
	scheme     = runtime.NewScheme()
	// api reads and writes the API server directly, without a cache.
	api       client.Client
	clientset kubernetes.Interface
)

func TestMain(m *testing.M) {
	os.Exit(runTests(m))
}

func runTests(m *testing.M) int {
	ctrl.SetLogger(zap.New(zap.WriteTo(os.Stderr), zap.UseDevMode(true)))
	var stop func() error
	var err error
	if restConfig, stop, err = testenv.Start(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer func() {
		if err := stop(); err != nil {
			fmt.Fprintln(os.Stderr, "stopping the envtest control plane:", err)
		}
	}()
	// The tests are not what is rate limited.
	restConfig.QPS, restConfig.Burst = 1000, 2000
	if err := setup(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return m.Run()
}

func setup() error {
	if err := testenv.InstallCRD(restConfig); err != nil {
		return err
	}
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return err
	}
	if err := kumacov1alpha1.AddToScheme(scheme); err != nil {
		return err
	}
	var err error
	if api, err = client.New(restConfig, client.Options{Scheme: scheme}); err != nil {
		return err
	}
	clientset, err = kubernetes.NewForConfig(restConfig)
	return err
}

// harness is one test: its scope, and the controllers it starts.
type harness struct {
	t     *testing.T
	scope string
}

// newHarness returns the harness of a test. The scope is a valid label value
// and the prefix of the names of the test's objects.
func newHarness(t *testing.T) *harness {
	t.Helper()
	scope := strings.ToLower(strings.NewReplacer("_", "-", "/", "-").Replace(t.Name()))
	if len(scope) > 40 {
		scope = scope[len(scope)-40:]
	}
	return &harness{t: t, scope: strings.Trim(scope, "-")}
}

// controllerOptions change the controller that a test starts.
type controllerOptions struct {
	Options
	config *rest.Config
	// applier replaces the node client.
	applier func(NodeStatusApplier) NodeStatusApplier
	// leaderElection makes the controller compete for the Lease of its name.
	leaderElection bool
	// name is the controller name; by default, the scope.
	name string
}

// running is a started controller.
type running struct {
	metrics *prometheus.Registry
	// stop stops the controller, and waits for it.
	stop func()
}

// start starts a controller managing the scope of the test, until the test
// ends or stop is called.
func (h *harness) start(change func(*controllerOptions)) *running {
	h.t.Helper()
	registry := prometheus.NewRegistry()
	options := &controllerOptions{
		Options: Options{ConditionPrefix: prefix, HeartbeatInterval: time.Hour, Metrics: registry},
		config:  restConfig,
		name:    h.scope,
	}
	if change != nil {
		change(options)
	}
	options.ControllerName = options.name
	selector := labels.SelectorFromSet(labels.Set{scopeLabel: h.scope})
	managerOptions := ctrl.Options{
		Scheme:                 scheme,
		Cache:                  CacheOptions(selector, selector),
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		// The replicas of a controller have the same name; here, they also
		// share a process.
		Controller: config.Controller{SkipNameValidation: ptr.To(true)},
	}
	if options.leaderElection {
		managerOptions.LeaderElection = true
		managerOptions.LeaderElectionID = options.name
		managerOptions.LeaderElectionNamespace = metav1.NamespaceDefault
		lease, renew, retry := 3*time.Second, 2*time.Second, 200*time.Millisecond
		managerOptions.LeaseDuration, managerOptions.RenewDeadline, managerOptions.RetryPeriod = &lease, &renew, &retry
	}
	mgr, err := ctrl.NewManager(options.config, managerOptions)
	if err != nil {
		h.t.Fatalf("creating the manager: %v", err)
	}
	nodes, err := kubernetes.NewForConfigAndClient(options.config, mgr.GetHTTPClient())
	if err != nil {
		h.t.Fatal(err)
	}
	var applier NodeStatusApplier = nodes.CoreV1().Nodes()
	if options.applier != nil {
		applier = options.applier(applier)
	}
	if err := Setup(mgr, applier, options.Options); err != nil {
		h.t.Fatalf("setting up the controller: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- mgr.Start(ctx) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			if err := <-done; err != nil {
				h.t.Errorf("the manager stopped with an error: %v", err)
			}
		})
	}
	h.t.Cleanup(stop)
	return &running{metrics: registry, stop: stop}
}

// eventually polls until the condition holds. On timeout, it fails the test
// with what the condition last observed.
func (h *harness) eventually(what string, condition func() (bool, string)) {
	h.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		ok, observed := condition()
		if ok {
			return
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("timed out waiting for %s; last observed: %s", what, observed)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// consistently fails the test if the condition stops holding during the period.
func (h *harness) consistently(period time.Duration, what string, condition func() (bool, string)) {
	h.t.Helper()
	deadline := time.Now().Add(period)
	for time.Now().Before(deadline) {
		if ok, observed := condition(); !ok {
			h.t.Fatalf("%s did not hold: %s", what, observed)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// nodeName returns the name of a node of the test.
func (h *harness) nodeName(name string) string { return h.scope + "-" + name }

// node creates a managed node. Conditions are "type=status" pairs, written as
// another field manager would.
func (h *harness) node(name string, conditions ...string) *corev1.Node {
	h.t.Helper()
	node := &corev1.Node{}
	node.Name = h.nodeName(name)
	node.Labels = map[string]string{scopeLabel: h.scope, "name": name}
	if err := api.Create(context.Background(), node); err != nil {
		h.t.Fatalf("creating node %s: %v", name, err)
	}
	h.t.Cleanup(func() { _ = api.Delete(context.Background(), node) })
	if len(conditions) > 0 {
		h.report(name, conditions...)
	}
	return node
}

// rebootable are the conditions with which the documented Kured triggers match.
var rebootable = []string{required + "=True", noPod + "=True", noAlert + "=True", ready + "=True"}

// report sets conditions on a node, as a reporter does: with its own field
// manager, one manager per condition type.
func (h *harness) report(node string, conditions ...string) {
	h.t.Helper()
	for _, pair := range conditions {
		conditionType, status, _ := strings.Cut(pair, "=")
		h.apply(node, "reporter-"+strings.ReplaceAll(conditionType, "/", "-"), conditionType, corev1.ConditionStatus(status), time.Now())
	}
}

// apply writes one condition of a node with server-side apply.
func (h *harness) apply(node, manager, conditionType string, status corev1.ConditionStatus, at time.Time) {
	h.t.Helper()
	apply := corev1ac.Node(h.nodeName(node)).WithStatus(corev1ac.NodeStatus().WithConditions(
		corev1ac.NodeCondition().
			WithType(corev1.NodeConditionType(conditionType)).WithStatus(status).
			WithReason("Test").WithMessage("set by the test").
			WithLastTransitionTime(metav1.NewTime(at)).WithLastHeartbeatTime(metav1.NewTime(at))))
	_, err := clientset.CoreV1().Nodes().ApplyStatus(context.Background(), apply, metav1.ApplyOptions{FieldManager: manager, Force: true})
	if err != nil {
		h.t.Fatalf("setting %s on node %s: %v", conditionType, node, err)
	}
}

// heartbeat refreshes the heartbeat of a condition and nothing else, as its
// reporter does.
func (h *harness) heartbeat(node, conditionType string) {
	h.t.Helper()
	condition := h.condition(node, conditionType)
	apply := corev1ac.Node(h.nodeName(node)).WithStatus(corev1ac.NodeStatus().WithConditions(
		corev1ac.NodeCondition().
			WithType(condition.Type).WithStatus(condition.Status).
			WithReason(condition.Reason).WithMessage(condition.Message).
			WithLastTransitionTime(condition.LastTransitionTime).WithLastHeartbeatTime(metav1.Now())))
	manager := "reporter-" + strings.ReplaceAll(conditionType, "/", "-")
	_, err := clientset.CoreV1().Nodes().ApplyStatus(context.Background(), apply, metav1.ApplyOptions{FieldManager: manager, Force: true})
	if err != nil {
		h.t.Fatalf("refreshing the heartbeat of %s on node %s: %v", conditionType, node, err)
	}
}

// getNode reads a node from the API server.
func (h *harness) getNode(name string) *corev1.Node {
	h.t.Helper()
	node := &corev1.Node{}
	if err := api.Get(context.Background(), client.ObjectKey{Name: h.nodeName(name)}, node); err != nil {
		h.t.Fatalf("reading node %s: %v", name, err)
	}
	return node
}

// conditions returns the conditions of a node as "type=status", sorted.
func (h *harness) conditions(name string) []string {
	h.t.Helper()
	var result []string
	for _, condition := range h.getNode(name).Status.Conditions {
		result = append(result, string(condition.Type)+"="+string(condition.Status))
	}
	slices.Sort(result)
	return result
}

func (h *harness) condition(name, conditionType string) *corev1.NodeCondition {
	h.t.Helper()
	for _, condition := range h.getNode(name).Status.Conditions {
		if string(condition.Type) == conditionType {
			return &condition
		}
	}
	return nil
}

// isUnder says whether the node is under that kind of maintenance.
func (h *harness) isUnder(name, kind string) bool {
	h.t.Helper()
	condition := h.condition(name, kind)
	return condition != nil && condition.Status == corev1.ConditionTrue
}

// wantConditions waits until the node has exactly these kumaco conditions,
// given as "type=status".
func (h *harness) wantConditions(name string, want ...string) {
	h.t.Helper()
	slices.Sort(want)
	h.eventually(fmt.Sprintf("node %s to have the kumaco conditions %v", name, want), func() (bool, string) {
		var owned []string
		for _, pair := range h.conditions(name) {
			if strings.HasPrefix(pair, generic+"=") || strings.HasPrefix(pair, prefix) {
				owned = append(owned, pair)
			}
		}
		return slices.Equal(owned, want), fmt.Sprint(owned)
	})
}

// under returns the nodes of the test that are under that kind of maintenance.
func (h *harness) under(kind string) []string {
	h.t.Helper()
	var nodes corev1.NodeList
	if err := api.List(context.Background(), &nodes, client.MatchingLabels{scopeLabel: h.scope}); err != nil {
		h.t.Fatal(err)
	}
	var result []string
	for _, node := range nodes.Items {
		for _, condition := range node.Status.Conditions {
			if string(condition.Type) == kind && condition.Status == corev1.ConditionTrue {
				result = append(result, node.Labels["name"])
			}
		}
	}
	slices.Sort(result)
	return result
}

// wantUnder waits until exactly these nodes are under that kind of maintenance.
func (h *harness) wantUnder(kind string, want ...string) {
	h.t.Helper()
	h.eventually(fmt.Sprintf("the nodes under %s to be %v", kind, want), func() (bool, string) {
		got := h.under(kind)
		return slices.Equal(got, want) || len(got) == 0 && len(want) == 0, fmt.Sprint(got)
	})
}

// windowName returns the name of a window of the test.
func (h *harness) windowName(name string) string { return h.scope + "-" + name }

// window creates a window of the reboot kind that is always in progress,
// selects every node of the test, has no trigger and a concurrency of 1.
func (h *harness) window(name string, change func(*kumacov1alpha1.MaintenanceWindowSpec)) *kumacov1alpha1.MaintenanceWindow {
	h.t.Helper()
	concurrency := int32(1)
	window := &kumacov1alpha1.MaintenanceWindow{}
	window.Name = h.windowName(name)
	window.Labels = map[string]string{scopeLabel: h.scope}
	window.Spec = kumacov1alpha1.MaintenanceWindowSpec{
		StartTime:     always,
		Duration:      metav1.Duration{Duration: time.Hour},
		ConditionType: reboot,
		Concurrency:   &concurrency,
	}
	if change != nil {
		change(&window.Spec)
	}
	if err := api.Create(context.Background(), window); err != nil {
		h.t.Fatalf("creating window %s: %v", name, err)
	}
	h.t.Cleanup(func() { _ = api.Delete(context.Background(), window) })
	return window
}

// kuredWindow creates a window with the triggers documented for Kured.
func (h *harness) kuredWindow(name string, concurrency int32) *kumacov1alpha1.MaintenanceWindow {
	h.t.Helper()
	return h.window(name, func(spec *kumacov1alpha1.MaintenanceWindowSpec) {
		spec.StartTriggers, spec.StopTriggers, spec.Concurrency = kuredStart, kuredStop, &concurrency
	})
}

// edit changes the spec of a window.
func (h *harness) edit(name string, change func(*kumacov1alpha1.MaintenanceWindowSpec)) {
	h.t.Helper()
	window := h.getWindow(name)
	change(&window.Spec)
	if err := api.Update(context.Background(), window); err != nil {
		h.t.Fatalf("editing window %s: %v", name, err)
	}
}

func (h *harness) getWindow(name string) *kumacov1alpha1.MaintenanceWindow {
	h.t.Helper()
	window := &kumacov1alpha1.MaintenanceWindow{}
	if err := api.Get(context.Background(), client.ObjectKey{Name: h.windowName(name)}, window); err != nil {
		h.t.Fatalf("reading window %s: %v", name, err)
	}
	return window
}

// wantStatus waits until the status of the window satisfies the condition, for
// the latest generation of its spec.
func (h *harness) wantStatus(name, what string, condition func(kumacov1alpha1.MaintenanceWindowStatus) bool) kumacov1alpha1.MaintenanceWindowStatus {
	h.t.Helper()
	var status kumacov1alpha1.MaintenanceWindowStatus
	h.eventually(fmt.Sprintf("the status of window %s to be %s", name, what), func() (bool, string) {
		window := h.getWindow(name)
		status = window.Status
		return status.ObservedGeneration == window.Generation && condition(status), fmt.Sprintf("%+v", status)
	})
	return status
}

// events returns the reasons and notes of the events on a window.
func (h *harness) events(window string) []string {
	h.t.Helper()
	var list eventsv1.EventList
	if err := api.List(context.Background(), &list, client.InNamespace(metav1.NamespaceDefault)); err != nil {
		h.t.Fatal(err)
	}
	var result []string
	for _, event := range list.Items {
		if event.Regarding.Kind == "MaintenanceWindow" && event.Regarding.Name == h.windowName(window) {
			result = append(result, event.Type+" "+event.Reason+": "+event.Note)
		}
	}
	return result
}

// wantEvent waits for an event on the window, with that type and reason, whose
// note contains the given parts.
func (h *harness) wantEvent(window, typeAndReason string, parts ...string) {
	h.t.Helper()
	h.eventually(fmt.Sprintf("an event %q on window %s mentioning %v", typeAndReason, window, parts), func() (bool, string) {
		events := h.events(window)
		for _, event := range events {
			if strings.HasPrefix(event, typeAndReason+": ") && containsAll(event, parts) {
				return true, ""
			}
		}
		return false, strings.Join(events, " | ")
	})
}

func containsAll(text string, parts []string) bool {
	for _, part := range parts {
		if !strings.Contains(text, part) {
			return false
		}
	}
	return true
}

// metric returns the value of a metric with these labels ("name=value"), and
// whether it exists.
func (r *running) metric(t *testing.T, name string, labelPairs ...string) (float64, bool) {
	t.Helper()
	families, err := r.metrics.Gather()
	if err != nil {
		t.Fatalf("gathering the metrics: %v", err)
	}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.GetMetric() {
			var pairs []string
			for _, label := range metric.GetLabel() {
				pairs = append(pairs, label.GetName()+"="+label.GetValue())
			}
			if !containsAll(strings.Join(pairs, ","), labelPairs) {
				continue
			}
			switch {
			case metric.GetGauge() != nil:
				return metric.GetGauge().GetValue(), true
			case metric.GetCounter() != nil:
				return metric.GetCounter().GetValue(), true
			case metric.GetHistogram() != nil:
				return float64(metric.GetHistogram().GetSampleCount()), true
			}
		}
	}
	return 0, false
}

// isNotFound is used by the tests that delete objects.
func isNotFound(err error) bool { return apierrors.IsNotFound(err) }

func newClientset(config *rest.Config) (kubernetes.Interface, error) {
	return kubernetes.NewForConfig(config)
}
