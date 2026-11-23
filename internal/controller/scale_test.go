package controller

import (
	"context"
	"fmt"
	"runtime"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/flowcontrol"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"

	kumacov1alpha1 "github.com/kubereboot/kumaco/api/v1alpha1"
	"github.com/kubereboot/kumaco/internal/maintenance"
)

// scaleNodes is the cluster size kumaco is designed for (AGENTS.md, "Scale
// and evolution").
const scaleNodes = 10_000

// The triggers that the README documents for Kured.
var (
	scaleStartTriggers = []string{
		`node.status.conditions.exists(c, c.type == 'detectors.kured.dev/RebootRequired' && c.status == 'True')`,
		`node.status.conditions.exists(c, c.type == 'detectors.kured.dev/NoPodInhibitor' && c.status == 'True')`,
		`node.status.conditions.exists(c, c.type == 'detectors.kured.dev/NoAlertInhibitor' && c.status == 'True')`,
	}
	scaleStopTriggers = []string{
		`!node.status.conditions.exists(c, c.type == 'detectors.kured.dev/RebootRequired' && c.status == 'True') &&
node.status.conditions.exists(c, c.type == 'Ready' && c.status == 'True')`,
	}
)

// representativeNode returns a node as a real cluster holds it: the labels,
// annotations, addresses, capacity and images that a kubelet reports, the
// kubelet's conditions, and those of the Kured reporters. One node in a
// hundred requires a reboot.
func representativeNode(index int) corev1.Node {
	since := metav1.NewTime(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC))
	node := corev1.Node{}
	node.Name = fmt.Sprintf("node-%05d", index)
	node.UID = types.UID(node.Name)
	node.ResourceVersion = "1"
	node.CreationTimestamp = since
	node.Labels = map[string]string{
		"kubernetes.io/hostname": node.Name, "kubernetes.io/os": "linux", "kubernetes.io/arch": "amd64",
		"topology.kubernetes.io/region": "eu-west-1", "topology.kubernetes.io/zone": fmt.Sprintf("eu-west-1%c", 'a'+rune(index%3)),
		"node.kubernetes.io/instance-type": "m7i.2xlarge", "team": fmt.Sprintf("team-%d", index%20),
	}
	node.Annotations = map[string]string{
		"node.alpha.kubernetes.io/ttl":                           "0",
		"volumes.kubernetes.io/controller-managed-attach-detach": "true",
	}
	node.Spec.ProviderID = "aws:///eu-west-1a/i-" + node.Name
	node.Spec.PodCIDRs = []string{fmt.Sprintf("10.%d.%d.0/24", index/256, index%256)}
	quantities := corev1.ResourceList{
		corev1.ResourceCPU: resource.MustParse("8"), corev1.ResourceMemory: resource.MustParse("32Gi"),
		corev1.ResourcePods: resource.MustParse("110"), corev1.ResourceEphemeralStorage: resource.MustParse("100Gi"),
	}
	node.Status.Capacity, node.Status.Allocatable = quantities, quantities
	node.Status.Addresses = []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.0.0.1"}, {Type: corev1.NodeHostName, Address: node.Name}}
	node.Status.NodeInfo = corev1.NodeSystemInfo{KubeletVersion: "v1.37.0", OSImage: "Ubuntu 26.04", ContainerRuntimeVersion: "containerd://2.2.0"}
	for image := range 50 {
		node.Status.Images = append(node.Status.Images, corev1.ContainerImage{
			Names:     []string{fmt.Sprintf("registry.example.com/team/image-%d@sha256:%064d", image, image), fmt.Sprintf("registry.example.com/team/image-%d:v1.2.3", image)},
			SizeBytes: 123456789,
		})
	}
	add := func(conditionType string, status corev1.ConditionStatus) {
		node.Status.Conditions = append(node.Status.Conditions, corev1.NodeCondition{
			Type: corev1.NodeConditionType(conditionType), Status: status, Reason: "Reported", Message: "reported by its owner",
			LastHeartbeatTime: since, LastTransitionTime: since,
		})
	}
	for _, pressure := range []string{"MemoryPressure", "DiskPressure", "PIDPressure", "NetworkUnavailable"} {
		add(pressure, corev1.ConditionFalse)
	}
	add("Ready", corev1.ConditionTrue)
	requiresReboot := corev1.ConditionFalse
	if index%100 == 0 {
		requiresReboot = corev1.ConditionTrue
	}
	add("detectors.kured.dev/RebootRequired", requiresReboot)
	add("detectors.kured.dev/NoPodInhibitor", corev1.ConditionTrue)
	add("detectors.kured.dev/NoAlertInhibitor", corev1.ConditionTrue)
	add(string(corev1.NodeMaintenanceInProgress), corev1.ConditionFalse)
	return node
}

// scaleCache stands for the informer cache at scale: it hands out its nodes
// without copying them, as the cache does for the controller. It is less work
// than the real cache, which builds the list with reflection.
type scaleCache struct {
	client.Client
	nodes   []corev1.Node
	byName  map[string]*corev1.Node
	windows []kumacov1alpha1.MaintenanceWindow
}

func newScaleCache(nodes int, windows ...kumacov1alpha1.MaintenanceWindow) *scaleCache {
	cache := &scaleCache{byName: map[string]*corev1.Node{}, windows: windows}
	for index := range nodes {
		node := representativeNode(index)
		// As the cache of the controller keeps it.
		node.Status = corev1.NodeStatus{Conditions: node.Status.Conditions}
		cache.nodes = append(cache.nodes, node)
	}
	for i := range cache.nodes {
		cache.byName[cache.nodes[i].Name] = &cache.nodes[i]
	}
	return cache
}

func (c *scaleCache) List(_ context.Context, list client.ObjectList, _ ...client.ListOption) error {
	switch list := list.(type) {
	case *corev1.NodeList:
		list.Items = append(list.Items[:0], c.nodes...)
	case *kumacov1alpha1.MaintenanceWindowList:
		list.Items = append(list.Items[:0], c.windows...)
	default:
		return fmt.Errorf("unexpected list of %T", list)
	}
	return nil
}

func (c *scaleCache) Get(_ context.Context, key client.ObjectKey, object client.Object, _ ...client.GetOption) error {
	node, ok := c.byName[key.Name]
	if !ok {
		return apierrors.NewNotFound(schema.GroupResource{Resource: "nodes"}, key.Name)
	}
	*object.(*corev1.Node) = *node
	return nil
}

func (c *scaleCache) Status() client.SubResourceWriter { return scaleStatusWriter{} }

type scaleStatusWriter struct{ client.SubResourceWriter }

func (scaleStatusWriter) Patch(context.Context, client.Object, client.Patch, ...client.SubResourcePatchOption) error {
	return nil
}

// ApplyStatus records the write in the cache at once: this cache does not lag.
func (c *scaleCache) ApplyStatus(_ context.Context, apply *corev1ac.NodeApplyConfiguration, _ metav1.ApplyOptions) (*corev1.Node, error) {
	node := c.byName[*apply.Name]
	var conditions []corev1.NodeCondition
	for _, condition := range node.Status.Conditions {
		if !maintenance.Owned(condition.Type, unitPrefix) {
			conditions = append(conditions, condition)
		}
	}
	for _, condition := range apply.Status.Conditions {
		conditions = append(conditions, corev1.NodeCondition{
			Type: *condition.Type, Status: *condition.Status, Reason: *condition.Reason, Message: *condition.Message,
			LastTransitionTime: *condition.LastTransitionTime, LastHeartbeatTime: *condition.LastHeartbeatTime,
		})
	}
	node.Status.Conditions = conditions
	return node, nil
}

func newScaleReconciler(tb testing.TB, cache *scaleCache) *Reconciler {
	tb.Helper()
	options := Options{ControllerName: "kumaco", ConditionPrefix: unitPrefix, HeartbeatInterval: 5 * time.Minute, Now: time.Now}
	metrics, err := newMetrics(prometheus.NewRegistry(), newNodeCollector(cache, unitPrefix, time.Now))
	if err != nil {
		tb.Fatal(err)
	}
	state := newState()
	state.startupSwept(nil)
	return &Reconciler{
		client: cache, writer: newNodeWriter(cache, cache, metrics), state: state, metrics: metrics,
		recorder: events.NewFakeRecorder(1000), options: options, enqueue: make(chan event.GenericEvent, 1000),
		known: map[string]knownWindow{}, compiled: map[types.UID]compiledSpec{},
		failing: map[string]sets.Set[string]{}, statusWritten: map[string]time.Time{},
	}
}

func scaleWindow() kumacov1alpha1.MaintenanceWindow {
	window := unitWindow("reboots", 10, scaleStartTriggers...)
	window.Spec.StopTriggers = scaleStopTriggers
	return *window
}

// One reconcile of a window selecting 10,000 nodes, with the triggers
// documented for Kured, in the steady state of a rollout: 10 nodes under
// maintenance, 90 waiting for a slot, 9,900 that need no reboot. Every node
// event of the cluster causes such a reconcile.
//
// README: #api-request-rate, #what-does-it-reconcile-and-when
func BenchmarkReconcile10000Nodes(b *testing.B) {
	cache := newScaleCache(scaleNodes, scaleWindow())
	reconciler := newScaleReconciler(b, cache)
	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: "reboots"}}
	if _, err := reconciler.Reconcile(b.Context(), request); err != nil {
		b.Fatal(err)
	}
	under := 0
	for _, node := range cache.nodes {
		if _, ok := maintenance.Under(&node, unitReboot); ok {
			under++
		}
	}
	if under != 10 {
		b.Fatalf("%d nodes under maintenance after the first reconcile, want the 10 slots filled", under)
	}

	b.ReportAllocs()
	for b.Loop() {
		if _, err := reconciler.Reconcile(b.Context(), request); err != nil {
			b.Fatal(err)
		}
	}
}

// The memory of 10,000 representative nodes in the informer cache of the
// controller: as the API server sends them, and as the cache keeps them.
func BenchmarkCacheOf10000Nodes(b *testing.B) {
	for name, transform := range map[string]func(any) (any, error){
		"as-sent": func(object any) (any, error) { return object, nil },
		"as-kept": trim,
	} {
		b.Run(name, func(b *testing.B) {
			var before, after runtime.MemStats
			for b.Loop() {
				runtime.GC()
				runtime.ReadMemStats(&before)
				nodes := make([]*corev1.Node, scaleNodes)
				for index := range nodes {
					node := representativeNode(index)
					if _, err := transform(&node); err != nil {
						b.Fatal(err)
					}
					nodes[index] = &node
				}
				runtime.GC()
				runtime.ReadMemStats(&after)
				b.ReportMetric(float64(after.HeapAlloc-before.HeapAlloc)/(1<<20), "MiB-heap")
				runtime.KeepAlive(nodes)
			}
		})
	}
}

// The cache keeps what the controller and the triggers read of a node, and
// nothing else of its status.
//
// README: #cel-triggers, #what-does-it-reconcile-and-when
func TestTheCacheKeepsOnlyWhatIsRead(t *testing.T) {
	node := representativeNode(0)
	node.ManagedFields = []metav1.ManagedFieldsEntry{{Manager: "kubelet"}}
	want := representativeNode(0)

	kept, err := trim(&node)
	if err != nil {
		t.Fatal(err)
	}
	got := kept.(*corev1.Node)
	if got.Name != want.Name || len(got.Labels) != len(want.Labels) || len(got.Annotations) != len(want.Annotations) ||
		got.Spec.ProviderID != want.Spec.ProviderID || len(got.Status.Conditions) != len(want.Status.Conditions) {
		t.Errorf("the cache lost a part of the node that is read: %+v", got)
	}
	if got.ManagedFields != nil || got.Status.Images != nil || got.Status.Capacity != nil || got.Status.Addresses != nil {
		t.Errorf("the cache keeps a part of the node that nothing reads: %+v", got.Status)
	}

	window := &kumacov1alpha1.MaintenanceWindow{}
	window.Status.State = kumacov1alpha1.StateInProgress
	if kept, err := trim(window); err != nil || kept.(*kumacov1alpha1.MaintenanceWindow).Status.State != kumacov1alpha1.StateInProgress {
		t.Errorf("the cache changed a window: %+v, %v", kept, err)
	}
}

// The periodic sweep of 10,000 nodes, when no heartbeat is due: the cost of
// going over the nodes, without the writes and without the pacing.
func BenchmarkSweepDecisions10000Nodes(b *testing.B) {
	cache := newScaleCache(scaleNodes, scaleWindow())
	reconciler := newScaleReconciler(b, cache)
	desired := maintenance.Desired{Prefix: unitPrefix, WindowTypes: sets.New(unitReboot), Now: time.Now()}
	b.ReportAllocs()
	for b.Loop() {
		for i := range cache.nodes {
			if _, err := reconciler.writer.sweep(b.Context(), cache.nodes[i].Name, func() maintenance.Desired { return desired }); err != nil {
				b.Fatal(err)
			}
		}
	}
}

// With the defaults of the README, the heartbeats of 10,000 nodes fit in the
// request rate: the sweep paces its writes below --kube-api-qps, and leaves
// room for starts, releases and status writes. The rate limiter is the one the
// entry point gives to every client; the sweep is replayed against it on a
// virtual clock, without sleeping.
//
// README: #api-request-rate, #heartbeat-contract
func TestTheDefaultRequestRateCoversTheHeartbeatsOf10000Nodes(t *testing.T) {
	const (
		qps, burst = 50, 100 // the defaults of --kube-api-qps and --kube-api-burst
		interval   = 5 * time.Minute
	)
	clock := &virtualClock{now: time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)}
	limiter := flowcontrol.NewTokenBucketRateLimiterWithClock(qps, burst, clock)

	// Three sweeps: every node is written once per interval, in its slot.
	// Between the slots, other requests (starts, releases, status writes)
	// take what is left of the rate.
	start := clock.now
	others := 0
	for sweep := range 3 {
		sweepStart := start.Add(time.Duration(sweep) * interval)
		for node := range scaleNodes {
			slotTime := sweepStart.Add(slot(node, scaleNodes, interval))
			for clock.now.Before(slotTime) && limiter.TryAccept() {
				others++
			}
			clock.now = slotTime
			if !limiter.TryAccept() {
				t.Fatalf("sweep %d, node %d: the heartbeat write has to wait for the rate limiter, so the sweep falls behind", sweep, node)
			}
		}
	}
	elapsed := 3 * interval
	heartbeatRate := float64(3*scaleNodes) / elapsed.Seconds()
	otherRate := float64(others) / elapsed.Seconds()
	if heartbeatRate < 33 || heartbeatRate > 34 {
		t.Errorf("%.1f heartbeat writes per second, want about 33, as the README says", heartbeatRate)
	}
	if otherRate < 10 {
		t.Errorf("%.1f other requests per second fit next to the heartbeats, want room for starts, releases and status writes", otherRate)
	}
	t.Logf("10,000 nodes, a heartbeat interval of %s, %d requests per second: %.1f heartbeat writes and %.1f other requests per second",
		interval, qps, heartbeatRate, otherRate)
}

// virtualClock is a clock that the test moves.
type virtualClock struct{ now time.Time }

func (c *virtualClock) Now() time.Time                  { return c.now }
func (c *virtualClock) Sleep(duration time.Duration)    { c.now = c.now.Add(duration) }
func (c *virtualClock) Since(t time.Time) time.Duration { return c.now.Sub(t) }
