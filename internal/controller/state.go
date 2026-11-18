package controller

import (
	"slices"
	"sync"

	"k8s.io/apimachinery/pkg/util/sets"

	kumacov1alpha1 "github.com/kubereboot/kumaco/api/v1alpha1"
	"github.com/kubereboot/kumaco/internal/maintenance"
)

// The situations of status.nodeCounts.
var situations = []string{
	kumacov1alpha1.SituationUnderMaintenance,
	kumacov1alpha1.SituationUnmetTrigger,
	kumacov1alpha1.SituationMaxConcurrencyReached,
}

// state is what the reconciler and the sweep share in memory. All of it is
// derived: it can be dropped at any time, and no decision depends on it.
type state struct {
	// swept is closed when the startup sweep is done: reconciles wait for it.
	swept chan struct{}

	mu sync.Mutex
	// notStarted is, per window in progress and per node it selects, why
	// maintenance did not start: the messages of MaintenanceInProgress.
	notStarted map[string]map[string]string
	// awaited are the windows that existed at startup and were not reconciled
	// yet. Heartbeats wait for them: a record left over from before the
	// downtime must not look fresh before it has been checked.
	awaited sets.Set[string]
	// heartbeats says that every awaited window was reconciled.
	heartbeats chan struct{}
}

func newState() *state {
	return &state{
		swept:      make(chan struct{}),
		notStarted: map[string]map[string]string{},
		heartbeats: make(chan struct{}),
	}
}

// startupSwept records the windows to reconcile before heartbeats start, and
// lets the reconciles run.
func (s *state) startupSwept(windows []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.awaited = sets.New(windows...)
	s.heartbeatsIfReady()
	close(s.swept)
}

// reconciled records that a window was reconciled.
func (s *state) reconciled(window string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.awaited.Delete(window)
	s.heartbeatsIfReady()
}

func (s *state) heartbeatsIfReady() {
	if s.awaited.Len() > 0 {
		return
	}
	select {
	case <-s.heartbeats:
	default:
		close(s.heartbeats)
	}
}

// heartbeatsStarted says whether heartbeats may be refreshed.
func (s *state) heartbeatsStarted() bool {
	select {
	case <-s.heartbeats:
		return true
	default:
		return false
	}
}

// remember keeps why the window did not start maintenance on the nodes it
// selects. A nil map forgets the window.
func (s *state) remember(window string, nodeSituations map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if nodeSituations == nil {
		delete(s.notStarted, window)
		return
	}
	notStarted := map[string]string{}
	for node, situation := range nodeSituations {
		if situation != kumacov1alpha1.SituationUnderMaintenance {
			notStarted[node] = situation
		}
	}
	s.notStarted[window] = notStarted
}

// notStartedOn returns the windows in progress that did not start maintenance
// on the node.
func (s *state) notStartedOn(node string) []maintenance.NotStarted {
	s.mu.Lock()
	defer s.mu.Unlock()
	var result []maintenance.NotStarted
	for window, nodes := range s.notStarted {
		if situation, ok := nodes[node]; ok {
			result = append(result, maintenance.NotStarted{Window: window, Situation: situation})
		}
	}
	slices.SortFunc(result, func(a, b maintenance.NotStarted) int { return cmpString(a.Window, b.Window) })
	return result
}

func cmpString(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}
