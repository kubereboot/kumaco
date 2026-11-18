// Package controller reconciles MaintenanceWindows: it reads windows and nodes
// from the informer cache, asks internal/maintenance for the decisions, and
// writes node conditions, window statuses, events and metrics. See README.md,
// "How does it work?".
package controller

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kumacov1alpha1 "github.com/kubereboot/kumaco/api/v1alpha1"
)

// FieldManager is the server-side apply field manager of the node conditions.
const FieldManager = "kumaco"

// Options configure one controller.
type Options struct {
	// ControllerName names the controller: it is the source of its events.
	ControllerName string
	// ConditionPrefix is the reserved prefix of the condition types of windows.
	ConditionPrefix string
	// HeartbeatInterval is how often lastHeartbeatTime is refreshed.
	HeartbeatInterval time.Duration
	// Metrics receives the kumaco_* metrics.
	Metrics prometheus.Registerer
	// Now returns the current time. It defaults to time.Now.
	Now func() time.Time
}

// NodeStatusApplier applies the status of a node with server-side apply. The
// node client of client-go implements it.
type NodeStatusApplier interface {
	ApplyStatus(ctx context.Context, node *corev1ac.NodeApplyConfiguration, options metav1.ApplyOptions) (*corev1.Node, error)
}

// CacheOptions restricts the informer cache to the nodes the controller
// manages (--node-selector) and to its windows (--window-label-selector):
// "managed nodes" are then the nodes of the cache. The cache only keeps what
// the controller reads.
func CacheOptions(nodeSelector, windowSelector labels.Selector) cache.Options {
	return cache.Options{
		DefaultTransform: trim,
		ByObject: map[client.Object]cache.ByObject{
			&corev1.Node{}:                      {Label: nodeSelector},
			&kumacov1alpha1.MaintenanceWindow{}: {Label: windowSelector},
		},
	}
}

// trim drops, before an object enters the cache, what nothing reads: managed
// fields, and the status of nodes except their conditions (images, capacity,
// addresses...), which triggers cannot read either. At 10,000 nodes, this is
// most of the memory of the cache (see BenchmarkCacheOf10000Nodes).
func trim(object any) (any, error) {
	if node, isNode := object.(*corev1.Node); isNode {
		node.Status = corev1.NodeStatus{Conditions: node.Status.Conditions}
	}
	return cache.TransformStripManagedFields()(object)
}
