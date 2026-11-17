//go:build e2e

// Package e2e holds the kind-based end-to-end tests. Run them with
// `mise run e2e`: see test/README.md.
package e2e

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"

	"github.com/kubereboot/kumaco/test/e2e/kind"
)

var (
	clusterName = flag.String("cluster", "",
		"name of the kind cluster of this run (required). It is created, unless an earlier run kept it.")
	keepCluster = flag.Bool("keep", false,
		"keep the cluster and its workloads after the run, for debugging: the next run reuses them")
	diagnosticsDir = flag.String("diagnostics-dir", "",
		"directory receiving the logs of the cluster when the run fails (default: a new temporary directory)")
)

// clusterStateDir holds one sentinel per cluster created by a run (see the
// kind package). It is ignored by git.
const clusterStateDir = ".clusters"

// What the scenarios share: the context of the run, cancelled when it is
// interrupted, and the cluster it created.
var (
	runContext context.Context
	client     kubernetes.Interface
)

func TestMain(m *testing.M) {
	flag.Parse()
	os.Exit(run(m))
}

func run(m *testing.M) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// After the first signal, a second one terminates the run immediately.
	go func() { <-ctx.Done(); stop() }()
	runContext = ctx

	cluster, err := kind.Create(ctx, kind.Options{
		Name:       *clusterName,
		ConfigPath: kindConfigPath,
		Keep:       *keepCluster,
		StateDir:   clusterStateDir,
		Log:        logf,
	})
	if err != nil {
		logf("%v", err)
		return 1
	}
	code := runScenarios(m, cluster)

	// The run may have been interrupted: clean up regardless.
	cleanup := context.WithoutCancel(ctx)
	if code != 0 {
		exportDiagnostics(cleanup, cluster)
	}
	if err := cluster.Teardown(cleanup); err != nil {
		logf("%v", err)
		return 1
	}
	return code
}

func runScenarios(m *testing.M, cluster *kind.Cluster) int {
	config, err := cluster.RESTConfig()
	if err == nil {
		client, err = kubernetes.NewForConfig(config)
	}
	if err != nil {
		logf("%v", err)
		return 1
	}
	return m.Run()
}

// exportDiagnostics captures the logs of a failed run before the teardown.
func exportDiagnostics(ctx context.Context, cluster *kind.Cluster) {
	dir := *diagnosticsDir
	if dir == "" {
		var err error
		if dir, err = os.MkdirTemp("", "kumaco-e2e-diagnostics-"); err != nil {
			logf("no diagnostics: %v", err)
			return
		}
	}
	if err := cluster.ExportLogs(ctx, dir); err != nil {
		logf("no diagnostics: %v", err)
		return
	}
	logf("The run failed: the logs of kind cluster %q are in %s", cluster.Name(), dir)
}

func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "e2e: "+format+"\n", args...)
}

// eventually polls check until it succeeds, for at most timeout. check returns
// what it observed, which is reported if the wait times out: a scenario never
// sleeps for a fixed time, and never waits without a bound.
func eventually(t *testing.T, timeout time.Duration, what string, check func(ctx context.Context) (done bool, observed string)) {
	t.Helper()
	var observed string
	err := wait.PollUntilContextTimeout(runContext, 2*time.Second, timeout, true,
		func(ctx context.Context) (bool, error) {
			var done bool
			done, observed = check(ctx)
			return done, nil
		})
	if err != nil {
		t.Fatalf("waiting for %s: %v after at most %s. Last observed: %s", what, err, timeout, observed)
	}
}
