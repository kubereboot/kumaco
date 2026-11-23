package controller

import (
	"context"
	"slices"
	"time"

	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	kumacov1alpha1 "github.com/kubereboot/kumaco/api/v1alpha1"
	"github.com/kubereboot/kumaco/internal/maintenance"
)

// sweeper runs the startup sweep, then the periodic one. See README.md,
// "Startup and periodic sweep". The manager starts it when this replica
// becomes the leader, with the caches started.
type sweeper struct {
	reader  client.Reader
	writer  *nodeWriter
	state   *state
	options Options
}

// NeedLeaderElection makes the manager start the sweeper on the leader only.
func (s *sweeper) NeedLeaderElection() bool { return true }

// Start runs until the context is done.
func (s *sweeper) Start(ctx context.Context) error {
	log := logf.FromContext(ctx).WithName("sweep")
	ctx = logf.IntoContext(ctx, log)

	// The first sweep removes the records whose type no window has anymore,
	// and sets MaintenanceInProgress where it is missing or outdated. It
	// refreshes no heartbeat. Reconciles wait for it, so it is retried.
	var windows []string
	err := wait.ExponentialBackoffWithContext(ctx, wait.Backoff{Duration: time.Second, Factor: 2, Cap: 30 * time.Second, Steps: 1 << 30},
		func(ctx context.Context) (bool, error) {
			var err error
			if windows, err = s.sweepOnce(ctx, false); err != nil {
				log.Error(err, "Startup sweep failed, retrying")
				return false, nil
			}
			return true, nil
		})
	if err != nil {
		return ignoreDone(ctx, err)
	}
	log.Info("Startup sweep done, reconciling the windows", "windows", len(windows))
	s.state.startupSwept(windows)

	select {
	case <-s.state.heartbeats:
	case <-ctx.Done():
		return nil
	}
	log.Info("Every window was reconciled, starting heartbeats", "heartbeatInterval", s.options.HeartbeatInterval)

	ticker := time.NewTicker(s.options.HeartbeatInterval)
	defer ticker.Stop()
	for {
		if _, err := s.sweepOnce(ctx, true); err != nil && ctx.Err() == nil {
			log.Error(err, "Periodic sweep failed")
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return nil
		}
	}
}

// ignoreDone returns nil when the error is the end of the context.
func ignoreDone(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return nil
	}
	return err
}

// sweepOnce goes over every managed node, and returns the names of the
// windows it saw. The periodic sweep refreshes heartbeats, and spreads its
// writes evenly over the heartbeat interval; the startup sweep does neither.
func (s *sweeper) sweepOnce(ctx context.Context, periodic bool) ([]string, error) {
	var windows kumacov1alpha1.MaintenanceWindowList
	if err := s.reader.List(ctx, &windows); err != nil {
		return nil, err
	}
	names := make([]string, len(windows.Items))
	types := sets.New[string]()
	for i, window := range windows.Items {
		names[i] = window.Name
		types.Insert(window.Spec.ConditionType)
	}
	nodes, err := listNodes(ctx, s.reader)
	if err != nil {
		return nil, err
	}
	nodeNames := make([]string, len(nodes))
	for i, node := range nodes {
		nodeNames[i] = node.Name
	}
	slices.Sort(nodeNames)

	started := time.Now()
	if err := s.sweepNodes(ctx, nodeNames, types, periodic, started); err != nil {
		return names, err
	}
	logf.FromContext(ctx).V(1).Info("Sweep done", "nodes", len(nodeNames), "periodic", periodic,
		"duration", time.Since(started).String())
	return names, nil
}

// sweepNodes sweeps the nodes in order, each in its slot when the sweep is
// periodic. The nodes whose last write was not in the cache yet get a second
// chance at the end.
func (s *sweeper) sweepNodes(ctx context.Context, nodes []string, types sets.Set[string], periodic bool, started time.Time) error {
	var skipped []string
	for i, name := range nodes {
		if periodic {
			if err := sleepUntil(ctx, started.Add(slot(i, len(nodes), s.options.HeartbeatInterval))); err != nil {
				return err
			}
		}
		wasSkipped, err := s.sweepNode(ctx, name, types, periodic)
		if err != nil {
			return err
		}
		if wasSkipped {
			skipped = append(skipped, name)
		}
	}
	for _, name := range skipped {
		if _, err := s.sweepNode(ctx, name, types, periodic); err != nil {
			return err
		}
	}
	return nil
}

func (s *sweeper) sweepNode(ctx context.Context, name string, types sets.Set[string], periodic bool) (skipped bool, err error) {
	return s.writer.sweep(ctx, name, func() maintenance.Desired {
		now := s.options.Now()
		desired := maintenance.Desired{
			Prefix:      s.options.ConditionPrefix,
			WindowTypes: types,
			NotStarted:  s.state.notStartedOn(name),
			Now:         now,
		}
		if periodic {
			desired.HeartbeatStaleBefore = now.Add(-heartbeatDue(s.options.HeartbeatInterval))
		}
		return desired
	})
}

// heartbeatDue is the age from which a heartbeat is refreshed. The sweep
// visits a node once per interval, a little earlier or later than the last
// time: the margin makes sure that it refreshes the heartbeat on each visit.
func heartbeatDue(interval time.Duration) time.Duration {
	return interval - interval/5
}

// slot is when, after the start of a periodic sweep, the node of that index is
// written: the writes are spread evenly over the interval.
func slot(index, nodes int, interval time.Duration) time.Duration {
	return time.Duration(int64(interval) / int64(nodes) * int64(index))
}

func sleepUntil(ctx context.Context, deadline time.Time) error {
	delay := time.Until(deadline)
	if delay <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
