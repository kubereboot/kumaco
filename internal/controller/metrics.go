package controller

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kumacov1alpha1 "github.com/kubereboot/kumaco/api/v1alpha1"
	"github.com/kubereboot/kumaco/internal/maintenance"
)

// The results of node writes and of reconciles.
const (
	resultOK       = "ok"
	resultConflict = "conflict"
	resultError    = "error"
)

// collectTimeout bounds the read of the cache when metrics are scraped.
const collectTimeout = 2 * time.Second

// metrics are the kumaco_* metrics of README.md, "Metrics".
type metrics struct {
	windowActive      *prometheus.GaugeVec
	windowNodes       *prometheus.GaugeVec
	windowInvalid     *prometheus.GaugeVec
	reconciles        *prometheus.CounterVec
	reconcileDuration prometheus.Histogram
	nodeWrites        *prometheus.CounterVec
	leader            prometheus.Gauge
}

func newMetrics(registry prometheus.Registerer, nodes *nodeCollector) (*metrics, error) {
	m := &metrics{
		windowActive: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kumaco_maintenance_window_active",
			Help: "1 while the schedule of the window is active, 0 otherwise.",
		}, []string{"window"}),
		windowNodes: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kumaco_maintenance_window_nodes",
			Help: "Number of nodes matching the window, per situation (as status.nodeCounts).",
		}, []string{"window", "state"}),
		windowInvalid: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kumaco_maintenance_window_invalid",
			Help: "1 while the window is Invalid, 0 otherwise.",
		}, []string{"window"}),
		reconciles: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kumaco_reconcile_total",
			Help: "Number of reconciles, per window and result (ok, error).",
		}, []string{"window", "result"}),
		reconcileDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "kumaco_reconcile_duration_seconds",
			Help: "Duration of reconciles.",
		}),
		nodeWrites: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kumaco_node_write_total",
			Help: "Number of node condition writes, per result (ok, conflict, error).",
		}, []string{"result"}),
		leader: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "kumaco_leader",
			Help: "1 on the replica currently leading, 0 otherwise.",
		}),
	}
	collectors := []prometheus.Collector{
		m.windowActive, m.windowNodes, m.windowInvalid, m.reconciles, m.reconcileDuration, m.nodeWrites, m.leader, nodes,
	}
	for _, collector := range collectors {
		if err := registry.Register(collector); err != nil {
			return nil, err
		}
	}
	return m, nil
}

// reconciled counts one reconcile of a window.
func (m *metrics) reconciled(window string, duration time.Duration, err error) {
	result := resultOK
	if err != nil {
		result = resultError
	}
	m.reconciles.WithLabelValues(window, result).Inc()
	m.reconcileDuration.Observe(duration.Seconds())
}

// nodeWritten counts one node write.
func (m *metrics) nodeWritten(result string) {
	m.nodeWrites.WithLabelValues(result).Inc()
}

// windowReported sets the gauges of a window from its status.
func (m *metrics) windowReported(window string, status *kumacov1alpha1.MaintenanceWindowStatus) {
	m.windowActive.WithLabelValues(window).Set(boolValue(status.State == kumacov1alpha1.StateInProgress))
	m.windowInvalid.WithLabelValues(window).Set(boolValue(status.State == kumacov1alpha1.StateInvalid))
	for _, situation := range situations {
		m.windowNodes.WithLabelValues(window, situation).Set(float64(status.NodeCounts[situation]))
	}
}

// windowDeleted removes the series of a window.
func (m *metrics) windowDeleted(window string) {
	labels := prometheus.Labels{"window": window}
	m.windowActive.Delete(labels)
	m.windowInvalid.Delete(labels)
	m.windowNodes.DeletePartialMatch(labels)
	m.reconciles.DeletePartialMatch(labels)
}

func boolValue(value bool) float64 {
	if value {
		return 1
	}
	return 0
}

// nodeCollector derives the per-node metrics from the nodes of the cache when
// they are scraped, so that they are correct as soon as the cache is synced.
type nodeCollector struct {
	reader client.Reader
	prefix string
	now    func() time.Time

	slotOccupied       *prometheus.Desc
	oldestHeartbeatAge *prometheus.Desc
}

func newNodeCollector(reader client.Reader, prefix string, now func() time.Time) *nodeCollector {
	return &nodeCollector{
		reader: reader, prefix: prefix, now: now,
		slotOccupied: prometheus.NewDesc("kumaco_node_slot_occupied",
			"1 if MaintenanceInProgress is True on the managed node, 0 otherwise.", []string{"node"}, nil),
		oldestHeartbeatAge: prometheus.NewDesc("kumaco_oldest_heartbeat_age_seconds",
			"Age of the oldest heartbeat of the conditions kumaco owns.", nil, nil),
	}
}

func (c *nodeCollector) Describe(descriptions chan<- *prometheus.Desc) {
	descriptions <- c.slotOccupied
	descriptions <- c.oldestHeartbeatAge
}

func (c *nodeCollector) Collect(collected chan<- prometheus.Metric) {
	// Before the cache is started, there is nothing to derive from.
	ctx, cancel := context.WithTimeout(context.Background(), collectTimeout)
	defer cancel()
	nodes, err := listNodes(ctx, c.reader)
	if err != nil {
		return
	}
	now := c.now()
	oldest := now
	for _, node := range nodes {
		occupied := false
		for _, condition := range node.Status.Conditions {
			if !maintenance.Owned(condition.Type, c.prefix) {
				continue
			}
			if condition.LastHeartbeatTime.Time.Before(oldest) {
				oldest = condition.LastHeartbeatTime.Time
			}
			if condition.Type == corev1.NodeMaintenanceInProgress {
				occupied = condition.Status == corev1.ConditionTrue
			}
		}
		collected <- prometheus.MustNewConstMetric(c.slotOccupied, prometheus.GaugeValue, boolValue(occupied), node.Name)
	}
	collected <- prometheus.MustNewConstMetric(c.oldestHeartbeatAge, prometheus.GaugeValue, now.Sub(oldest).Seconds())
}
