//go:build e2e

package e2e

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	kumacov1alpha1 "github.com/kubereboot/kumaco/api/v1alpha1"
)

const (
	generic = string(corev1.NodeMaintenanceInProgress)
	// e2eKind is the kind of maintenance of the scenarios.
	e2eKind = "kumaco.kured.dev/E2EInProgress"
	// maintenanceRequired is a condition that the scenarios publish on nodes,
	// as a reporter does. It is not the condition of a real reporter.
	maintenanceRequired = "e2e.example.com/MaintenanceRequired"
	reporter            = "e2e-reporter"

	controlPlaneLabel = "node-role.kubernetes.io/control-plane"
)

// windows returns a client for MaintenanceWindows.
func windows(t *testing.T) ctrlclient.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := kumacov1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	client, err := ctrlclient.New(restConfig, ctrlclient.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func condition(node *corev1.Node, conditionType string) *corev1.NodeCondition {
	for i := range node.Status.Conditions {
		if string(node.Status.Conditions[i].Type) == conditionType {
			return &node.Status.Conditions[i]
		}
	}
	return nil
}

func isTrue(node *corev1.Node, conditionType string) bool {
	found := condition(node, conditionType)
	return found != nil && found.Status == corev1.ConditionTrue
}

func listNodes(ctx context.Context) ([]corev1.Node, error) {
	list, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	return list.Items, nil
}

// workers returns the names of the worker nodes.
func workers(t *testing.T) []string {
	t.Helper()
	nodes, err := listNodes(runContext)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, node := range nodes {
		if _, isControlPlane := node.Labels[controlPlaneLabel]; !isControlPlane {
			names = append(names, node.Name)
		}
	}
	slices.Sort(names)
	return names
}

// report publishes the scenarios' condition on a node, as a reporter does:
// with its own field manager. An empty status removes the condition.
func report(t *testing.T, node string, status corev1.ConditionStatus) {
	t.Helper()
	nodeStatus := corev1ac.NodeStatus()
	if status != "" {
		nodeStatus.WithConditions(corev1ac.NodeCondition().
			WithType(maintenanceRequired).WithStatus(status).WithReason("E2E").
			WithLastTransitionTime(metav1.Now()).WithLastHeartbeatTime(metav1.Now()))
	}
	_, err := client.CoreV1().Nodes().ApplyStatus(context.WithoutCancel(runContext), corev1ac.Node(node).WithStatus(nodeStatus),
		metav1.ApplyOptions{FieldManager: reporter, Force: true})
	if err != nil {
		t.Fatalf("reporting %s=%s on node %s: %v", maintenanceRequired, status, node, err)
	}
}

// under returns the nodes that are under the scenarios' kind of maintenance.
func under(ctx context.Context) ([]string, error) {
	nodes, err := listNodes(ctx)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, node := range nodes {
		if isTrue(&node, e2eKind) {
			names = append(names, node.Name)
		}
	}
	slices.Sort(names)
	return names, nil
}

func wantUnder(t *testing.T, want ...string) {
	t.Helper()
	eventually(t, 2*time.Minute, fmt.Sprintf("the nodes under %s to be %v", e2eKind, want), func(ctx context.Context) (bool, string) {
		got, err := under(ctx)
		if err != nil {
			return false, err.Error()
		}
		return slices.Equal(got, want) || len(got) == 0 && len(want) == 0, fmt.Sprint(got)
	})
}

// leader returns the pod holding the leader-election Lease of kumaco.
func leader(ctx context.Context) (string, error) {
	lease, err := client.CoordinationV1().Leases(kumacoNamespace).Get(ctx, kumacoRelease, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	if lease.Spec.HolderIdentity == nil {
		return "", fmt.Errorf("the Lease %s has no holder", lease.Name)
	}
	// The identity is the name of the pod, and a unique suffix.
	pod, _, _ := strings.Cut(*lease.Spec.HolderIdentity, "_")
	return pod, nil
}

// kumaco runs from its scratch image, with two replicas, without restarts.
//
// README: #replicas-of-the-same-controller, #configuration
func TestKumacoRunsWithTwoReplicas(t *testing.T) {
	eventually(t, 3*time.Minute, "two kumaco pods to be ready", func(ctx context.Context) (bool, string) {
		pods, err := client.CoreV1().Pods(kumacoNamespace).List(ctx, metav1.ListOptions{LabelSelector: "app.kubernetes.io/name=kumaco"})
		if err != nil {
			return false, err.Error()
		}
		var observed []string
		ready := 0
		for _, pod := range pods.Items {
			for _, status := range pod.Status.ContainerStatuses {
				observed = append(observed, fmt.Sprintf("%s: image %s, ready %v, %d restarts", pod.Name, status.Image, status.Ready, status.RestartCount))
				if status.Ready && status.RestartCount == 0 && strings.HasSuffix(status.Image, "kubereboot/kumaco:dev") {
					ready++
				}
			}
		}
		return ready == 2, strings.Join(observed, "; ")
	})
	if _, err := leader(runContext); err != nil {
		t.Errorf("no leader: %v", err)
	}
}

// noMaintenance returns the heartbeat of MaintenanceInProgress on each node,
// and the nodes that do not have it False, next to a Ready kubelet.
func noMaintenance(ctx context.Context) (heartbeats map[string]metav1.Time, missing []string, err error) {
	nodes, err := listNodes(ctx)
	if err != nil {
		return nil, nil, err
	}
	heartbeats = map[string]metav1.Time{}
	for _, node := range nodes {
		found := condition(&node, generic)
		if found == nil || found.Status != corev1.ConditionFalse || found.Reason != "KumacoNoMaintenance" || !isTrue(&node, "Ready") {
			missing = append(missing, node.Name)
			continue
		}
		heartbeats[node.Name] = found.LastHeartbeatTime
	}
	return heartbeats, missing, nil
}

// Every node gets MaintenanceInProgress, whose heartbeat kumaco refreshes,
// next to the conditions of the kubelet, which stay untouched.
//
// README: #what-does-it-look-like-on-my-node, #heartbeat-contract, #startup-and-periodic-sweep
func TestEveryNodeHasMaintenanceInProgress(t *testing.T) {
	var first map[string]metav1.Time
	eventually(t, 2*time.Minute, "MaintenanceInProgress=False on every node", func(ctx context.Context) (bool, string) {
		heartbeats, missing, err := noMaintenance(ctx)
		if err != nil {
			return false, err.Error()
		}
		first = heartbeats
		return len(heartbeats) > 0 && len(missing) == 0, fmt.Sprintf("not there yet: %v", missing)
	})

	eventually(t, 2*time.Minute, "a later heartbeat on every node", func(ctx context.Context) (bool, string) {
		heartbeats, missing, err := noMaintenance(ctx)
		if err != nil || len(missing) > 0 {
			return false, fmt.Sprint(missing, err)
		}
		var stale []string
		for node, beat := range heartbeats {
			if !beat.After(first[node].Time) {
				stale = append(stale, node)
			}
		}
		return len(stale) == 0, fmt.Sprintf("no new heartbeat on: %v", stale)
	})
}

// rolloutWindow creates a window that is always in progress, in a time zone,
// for the workers that the scenarios' condition marks, one at a time. It is
// deleted when the test ends.
func rolloutWindow(t *testing.T, api ctrlclient.Client) *kumacov1alpha1.MaintenanceWindow {
	t.Helper()
	window := &kumacov1alpha1.MaintenanceWindow{}
	window.Name = "e2e-rollout"
	window.Spec = kumacov1alpha1.MaintenanceWindowSpec{
		StartTime:     "CRON_TZ=Europe/Brussels 0 * * * * ? *",
		Duration:      metav1.Duration{Duration: time.Hour},
		ConditionType: e2eKind,
		NodeSelector: metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
			{Key: controlPlaneLabel, Operator: metav1.LabelSelectorOpDoesNotExist},
		}},
		StartTriggers: []string{
			`node.status.conditions.exists(c, c.type == 'e2e.example.com/MaintenanceRequired' && c.status == 'True')`,
			`node.status.conditions.exists(c, c.type == 'Ready' && c.status == 'True')`,
		},
		StopTriggers: []string{
			`!node.status.conditions.exists(c, c.type == 'e2e.example.com/MaintenanceRequired' && c.status == 'True')`,
		},
		Concurrency: new(int32(1)),
	}
	if err := api.Create(runContext, window); err != nil {
		t.Fatalf("creating the window: %v", err)
	}
	t.Cleanup(func() {
		if err := api.Delete(context.WithoutCancel(runContext), window); err != nil && !apierrors.IsNotFound(err) {
			t.Errorf("deleting the window: %v", err)
		}
	})
	return window
}

// handOver deletes the leader, and waits until another replica leads.
func handOver(t *testing.T) {
	t.Helper()
	before, err := leader(runContext)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.CoreV1().Pods(kumacoNamespace).Delete(runContext, before, metav1.DeleteOptions{}); err != nil {
		t.Fatalf("deleting the leader: %v", err)
	}
	eventually(t, 3*time.Minute, "another replica to lead", func(ctx context.Context) (bool, string) {
		now, err := leader(ctx)
		if err != nil {
			return false, err.Error()
		}
		return now != before, "the leader is " + now
	})
}

// startedAt returns when the scenarios' kind of maintenance started on a node.
func startedAt(t *testing.T, name string) metav1.Time {
	t.Helper()
	node, err := client.CoreV1().Nodes().Get(runContext, name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !isTrue(node, e2eKind) || !isTrue(node, generic) || !isTrue(node, "Ready") {
		t.Fatalf("conditions of %s = %+v, want %s, MaintenanceInProgress and the kubelet's Ready all True", name, node.Status.Conditions, e2eKind)
	}
	return condition(node, e2eKind).LastTransitionTime
}

// A maintenance rolls over the real worker nodes, one at a time, across a
// leader handover. The window uses a time zone, which the scratch image only
// knows from its embedded database. Kured is not part of this scenario: the
// test publishes the condition that the triggers read, and "does" the
// maintenance by clearing it.
//
// README: #reconciling-a-window, #replicas-of-the-same-controller, #time-zones, #debugging-with-kumaco-events
func TestAMaintenanceRollsOverTheWorkers(t *testing.T) {
	api := windows(t)
	nodes := workers(t)
	if len(nodes) != 2 {
		t.Fatalf("workers = %v, want the two workers of test/e2e/kind.yaml", nodes)
	}
	for _, node := range nodes {
		report(t, node, corev1.ConditionTrue)
		t.Cleanup(func() { report(t, node, "") })
	}
	window := rolloutWindow(t, api)

	// Maintenance starts on one worker only.
	wantUnder(t, nodes[0])
	eventually(t, time.Minute, "the status of the window", func(ctx context.Context) (bool, string) {
		if err := api.Get(ctx, ctrlclient.ObjectKeyFromObject(window), window); err != nil {
			return false, err.Error()
		}
		status := window.Status
		return status.State == kumacov1alpha1.StateInProgress && status.MatchingNodes == 2 &&
			slices.Equal(status.NodesUnderMaintenance, nodes[:1]) &&
			status.NodeCounts[kumacov1alpha1.SituationMaxConcurrencyReached] == 1, fmt.Sprintf("%+v", status)
	})
	started := startedAt(t, nodes[0])

	// The leader goes away: the other replica takes over from the records.
	handOver(t)
	wantUnder(t, nodes[0])
	if got := startedAt(t, nodes[0]); !got.Equal(&started) {
		t.Errorf("the maintenance of %s started at %s, then at %s after the handover: it was started again", nodes[0], started, got)
	}

	// The new leader finishes the rollout.
	report(t, nodes[0], corev1.ConditionFalse)
	wantUnder(t, nodes[1])
	report(t, nodes[1], corev1.ConditionFalse)
	wantUnder(t)

	eventually(t, time.Minute, "the events of the window", func(ctx context.Context) (bool, string) {
		events, err := client.EventsV1().Events(metav1.NamespaceDefault).List(ctx, metav1.ListOptions{})
		if err != nil {
			return false, err.Error()
		}
		var reasons []string
		for _, event := range events.Items {
			if event.Regarding.Kind == "MaintenanceWindow" && event.Regarding.Name == window.Name {
				reasons = append(reasons, event.Reason)
			}
		}
		return slices.Contains(reasons, "MaintenanceStarted") && slices.Contains(reasons, "MaintenanceReleased"), fmt.Sprint(reasons)
	})
}

// metricsReaderToken returns the token of a new service account that may read
// the metrics of kumaco. The account is deleted when the test ends.
func metricsReaderToken(t *testing.T) string {
	t.Helper()
	cleanup := context.WithoutCancel(runContext)
	account := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "e2e-scraper", Namespace: kumacoNamespace}}
	binding := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "e2e-scraper"},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: kumacoRelease + "-metrics-reader"},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: account.Name, Namespace: account.Namespace}},
	}
	if _, err := client.CoreV1().ServiceAccounts(account.Namespace).Create(runContext, account, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = client.CoreV1().ServiceAccounts(account.Namespace).Delete(cleanup, account.Name, metav1.DeleteOptions{})
	})
	if _, err := client.RbacV1().ClusterRoleBindings().Create(runContext, binding, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = client.RbacV1().ClusterRoleBindings().Delete(cleanup, binding.Name, metav1.DeleteOptions{})
	})
	token, err := client.CoreV1().ServiceAccounts(account.Namespace).CreateToken(runContext, account.Name, &authenticationv1.TokenRequest{}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return token.Status.Token
}

// leads says whether a kumaco pod reports that it leads. It also checks that
// the pod refuses to serve its metrics without a token.
func leads(ctx context.Context, pod, token string) (bool, error) {
	if status, _, err := scrape(ctx, pod, ""); err != nil || (status != http.StatusUnauthorized && status != http.StatusForbidden) {
		return false, fmt.Errorf("%s without a token: status %d, %v; want it refused", pod, status, err)
	}
	status, metrics, err := scrape(ctx, pod, token)
	if err != nil || status != http.StatusOK || !strings.Contains(metrics, "kumaco_leader") {
		return false, fmt.Errorf("%s with the token of a metrics reader: status %d, %v", pod, status, err)
	}
	return strings.Contains(metrics, "\nkumaco_leader 1\n"), nil
}

// The metrics endpoint only answers clients that the API server authenticates
// and authorizes. Exactly one replica leads.
//
// README: #metrics, #security-and-access-rights
func TestTheMetricsEndpointRequiresAuthorization(t *testing.T) {
	token := metricsReaderToken(t)
	eventually(t, 3*time.Minute, "the metrics of both replicas, one of them leading", func(ctx context.Context) (bool, string) {
		pods, err := client.CoreV1().Pods(kumacoNamespace).List(ctx, metav1.ListOptions{LabelSelector: "app.kubernetes.io/name=kumaco"})
		if err != nil || len(pods.Items) != 2 {
			return false, fmt.Sprintf("%d pods, %v", len(pods.Items), err)
		}
		leaders := 0
		for _, pod := range pods.Items {
			isLeader, err := leads(ctx, pod.Name, token)
			if err != nil {
				return false, err.Error()
			}
			if isLeader {
				leaders++
			}
		}
		return leaders == 1, fmt.Sprintf("%d replicas report kumaco_leader 1", leaders)
	})
}

// scrape reads the metrics of a kumaco pod through a port-forward, with a
// bearer token unless it is empty. The endpoint serves a self-signed
// certificate by default.
func scrape(ctx context.Context, pod, token string) (status int, body string, err error) {
	transport, upgrader, err := spdy.RoundTripperFor(restConfig)
	if err != nil {
		return 0, "", err
	}
	url := client.CoreV1().RESTClient().Post().Resource("pods").Namespace(kumacoNamespace).Name(pod).SubResource("portforward").URL()
	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, http.MethodPost, url)
	stop, ready := make(chan struct{}), make(chan struct{})
	defer close(stop)
	forwarder, err := portforward.New(dialer, []string{"0:8443"}, stop, ready, io.Discard, io.Discard)
	if err != nil {
		return 0, "", err
	}
	failed := make(chan error, 1)
	go func() { failed <- forwarder.ForwardPorts() }()
	select {
	case <-ready:
	case err := <-failed:
		return 0, "", fmt.Errorf("port-forward to %s: %w", pod, err)
	case <-ctx.Done():
		return 0, "", ctx.Err()
	}
	ports, err := forwarder.GetPorts()
	if err != nil {
		return 0, "", err
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("https://127.0.0.1:%d/metrics", ports[0].Local), nil)
	if err != nil {
		return 0, "", err
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	insecure := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	response, err := insecure.Do(request)
	if err != nil {
		return 0, "", err
	}
	defer response.Body.Close()
	content, err := io.ReadAll(response.Body)
	return response.StatusCode, string(content), err
}
