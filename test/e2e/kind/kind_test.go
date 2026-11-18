package kind

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// These tests enforce the cluster lifecycle rules of AGENTS.md ("E2e:
// provision everything, never break Kured"), with a fake kind binary.

const testCluster = "kumaco-e2e"

// fakeKind records the commands of a run, and answers them like kind would.
type fakeKind struct {
	commands  []string // every command, as one string
	clusters  string   // output of `kind get clusters`
	nodes     string   // output of `kind get nodes`
	apiNodes  []string // nodes the API server of the kubeconfig serves
	createErr error
	onCreate  func() // called when `kind create cluster` runs
}

func (f *fakeKind) dependencies() dependencies {
	return dependencies{
		run: func(_ context.Context, name string, args ...string) (string, error) {
			command := name + " " + strings.Join(args, " ")
			f.commands = append(f.commands, command)
			switch {
			case strings.HasPrefix(command, "kind get clusters"):
				return f.clusters, nil
			case strings.HasPrefix(command, "kind get nodes"):
				return f.nodes, nil
			case strings.HasPrefix(command, "kind create cluster"):
				if f.onCreate != nil {
					f.onCreate()
				}
				return "", f.createErr
			}
			return "", nil
		},
		apiNodes: func(context.Context, string) ([]string, error) {
			return f.apiNodes, nil
		},
		verifyTimeout: 50 * time.Millisecond,
		pollInterval:  time.Millisecond,
	}
}

// ran returns the recorded commands starting with prefix.
func (f *fakeKind) ran(prefix string) []string {
	var matching []string
	for _, command := range f.commands {
		if strings.HasPrefix(command, prefix) {
			matching = append(matching, command)
		}
	}
	return matching
}

// noCluster is a host on which the cluster of the run does not exist.
func noCluster() *fakeKind {
	return &fakeKind{
		clusters: "another-cluster\n",
		nodes:    testCluster + "-worker\n" + testCluster + "-control-plane\n",
		apiNodes: []string{testCluster + "-control-plane", testCluster + "-worker"},
	}
}

// runningCluster is a host on which a cluster with the name of the run exists.
func runningCluster() *fakeKind {
	fake := noCluster()
	fake.clusters += testCluster + "\n"
	return fake
}

func options(t *testing.T) Options {
	t.Helper()
	return Options{Name: testCluster, ConfigPath: "kind.yaml", StateDir: t.TempDir(), Log: t.Logf}
}

// sentinel is where a run records that it created the cluster.
func sentinel(opts Options) string {
	return filepath.Join(opts.StateDir, opts.Name)
}

// recordEarlierRun leaves the sentinel of an earlier run, with a file in it.
func recordEarlierRun(t *testing.T, opts Options) (leftover string) {
	t.Helper()
	if err := os.MkdirAll(sentinel(opts), 0o700); err != nil {
		t.Fatal(err)
	}
	leftover = filepath.Join(sentinel(opts), "leftover")
	if err := os.WriteFile(leftover, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return leftover
}

func exists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return err == nil
}

func mustCreate(t *testing.T, fake *fakeKind, opts Options) *Cluster {
	t.Helper()
	cluster, err := create(t.Context(), opts, fake.dependencies())
	if err != nil {
		t.Fatalf("create() error: %v", err)
	}
	return cluster
}

func TestCreateRequiresAnExplicitClusterName(t *testing.T) {
	for _, name := range []string{"", "-keep", "Has Capitals", "under_score", "../escape"} {
		t.Run(name, func(t *testing.T) {
			fake := noCluster()
			opts := options(t)
			opts.Name = name

			if _, err := create(t.Context(), opts, fake.dependencies()); err == nil {
				t.Error("create() error = nil, want an error about the cluster name")
			}
			if len(fake.commands) != 0 {
				t.Errorf("commands = %q, want none", fake.commands)
			}
		})
	}
}

func TestCreateRequiresAStateDirectory(t *testing.T) {
	fake := noCluster()
	opts := options(t)
	opts.StateDir = ""

	if _, err := create(t.Context(), opts, fake.dependencies()); err == nil {
		t.Error("create() error = nil, want an error: the run must record the cluster it creates")
	}
	if len(fake.commands) != 0 {
		t.Errorf("commands = %q, want none", fake.commands)
	}
}

func TestCreateProvidesAClusterWhenNoneIsRunning(t *testing.T) {
	fake := noCluster()
	opts := options(t)
	// The run records the cluster before creating it, so that even a run
	// killed during the creation leaves a cluster known as its own.
	var recordedBeforeCreation bool
	fake.onCreate = func() { recordedBeforeCreation = exists(t, sentinel(opts)) }

	cluster := mustCreate(t, fake, opts)

	created := fake.ran("kind create cluster")
	if len(created) != 1 {
		t.Fatalf("create commands = %q, want exactly one", created)
	}
	for _, want := range []string{
		" --name " + testCluster + " ",
		" --config kind.yaml ",
		" --kubeconfig " + cluster.Kubeconfig() + " ",
		" --wait ",
	} {
		if !strings.Contains(created[0]+" ", want) {
			t.Errorf("create command = %q, want it to contain %q", created[0], want)
		}
	}
	if strings.Contains(created[0], "--retain") {
		t.Errorf("create command = %q, want no --retain without Keep", created[0])
	}
	if !recordedBeforeCreation {
		t.Errorf("%s did not exist when `kind create cluster` ran", sentinel(opts))
	}
	// The developer's kubeconfig is never written: the run has its own file.
	if want := filepath.Join(sentinel(opts), "kubeconfig"); cluster.Kubeconfig() != want {
		t.Errorf("Kubeconfig() = %q, want %q", cluster.Kubeconfig(), want)
	}
}

func TestCreateRefusesAClusterNoRunCreatedAndLeavesItAlone(t *testing.T) {
	fake := runningCluster()
	opts := options(t)

	_, err := create(t.Context(), opts, fake.dependencies())
	if err == nil || !strings.Contains(err.Error(), "not created by an e2e run") {
		t.Errorf("create() error = %v, want a \"not created by an e2e run\" error", err)
	}
	if want := []string{"kind get clusters"}; !slices.Equal(fake.commands, want) {
		t.Errorf("commands = %q, want %q: the cluster is neither used nor deleted", fake.commands, want)
	}
	if exists(t, sentinel(opts)) {
		t.Errorf("%s exists: the run must not claim a cluster it did not create", sentinel(opts))
	}
}

func TestCreateReusesTheClusterOfAnEarlierRun(t *testing.T) {
	fake := runningCluster()
	opts := options(t)
	recordEarlierRun(t, opts)

	cluster := mustCreate(t, fake, opts)

	if created := fake.ran("kind create cluster"); len(created) != 0 {
		t.Errorf("create commands = %q, want none: the cluster is running", created)
	}
	// The kubeconfig of the run is written again, as the earlier run may have
	// been killed before it had one.
	want := "kind export kubeconfig --name " + testCluster + " --kubeconfig " + cluster.Kubeconfig()
	if exported := fake.ran("kind export kubeconfig"); !slices.Equal(exported, []string{want}) {
		t.Errorf("export commands = %q, want %q", exported, want)
	}
	if verified := fake.ran("kind get nodes"); len(verified) != 1 {
		t.Errorf("`kind get nodes` commands = %q, want one: a reused cluster is verified too", verified)
	}

	// It is the cluster of an e2e run: this run deletes it like its own.
	if err := cluster.Teardown(t.Context()); err != nil {
		t.Fatalf("Teardown() error: %v", err)
	}
	if deleted := fake.ran("kind delete cluster --name " + testCluster); len(deleted) != 1 {
		t.Errorf("delete commands = %q, want one for %s", deleted, testCluster)
	}
	if exists(t, sentinel(opts)) {
		t.Errorf("%s still exists after Teardown", sentinel(opts))
	}
}

func TestCreateReplacesTheRecordOfAClusterThatIsGone(t *testing.T) {
	fake := noCluster()
	opts := options(t)
	leftover := recordEarlierRun(t, opts)

	mustCreate(t, fake, opts)

	if created := fake.ran("kind create cluster"); len(created) != 1 {
		t.Errorf("create commands = %q, want exactly one", created)
	}
	if exists(t, leftover) {
		t.Errorf("%s, of a cluster that no longer exists, was not removed", leftover)
	}
}

func TestCreateFailsWhenTheKubeconfigServesAnotherCluster(t *testing.T) {
	for name, fake := range map[string]*fakeKind{
		"new cluster":    noCluster(),
		"reused cluster": runningCluster(),
	} {
		t.Run(name, func(t *testing.T) {
			fake.apiNodes = []string{"some-other-control-plane"}
			opts := options(t)
			if name == "reused cluster" {
				recordEarlierRun(t, opts)
			}

			_, err := create(t.Context(), opts, fake.dependencies())
			if err == nil || !strings.Contains(err.Error(), "some-other-control-plane") {
				t.Errorf("create() error = %v, want an error naming the unexpected nodes", err)
			}
			if deleted := fake.ran("kind delete cluster"); len(deleted) != 1 {
				t.Errorf("delete commands = %q, want the cluster of the run to be deleted", deleted)
			}
			if exists(t, sentinel(opts)) {
				t.Errorf("%s still exists after the deletion", sentinel(opts))
			}
		})
	}
}

func TestCreateFailsWhenKindReportsNoNodes(t *testing.T) {
	fake := noCluster()
	fake.nodes = ""
	fake.apiNodes = nil

	if _, err := create(t.Context(), options(t), fake.dependencies()); err == nil {
		t.Error("create() error = nil, want an error: a cluster without nodes is not verified")
	}
}

func TestFailedCreateDeletesWhatItCreated(t *testing.T) {
	fake := noCluster()
	fake.createErr = errors.New("kind failed")
	opts := options(t)

	_, err := create(t.Context(), opts, fake.dependencies())
	if !errors.Is(err, fake.createErr) {
		t.Errorf("create() error = %v, want it to wrap %v", err, fake.createErr)
	}
	if deleted := fake.ran("kind delete cluster --name " + testCluster); len(deleted) != 1 {
		t.Errorf("delete commands = %q, want one for %s", deleted, testCluster)
	}
	if exists(t, sentinel(opts)) {
		t.Errorf("%s still exists after the deletion", sentinel(opts))
	}
}

func TestTeardownDeletesTheClusterByDefault(t *testing.T) {
	fake := noCluster()
	opts := options(t)
	cluster := mustCreate(t, fake, opts)

	if err := cluster.Teardown(t.Context()); err != nil {
		t.Fatalf("Teardown() error: %v", err)
	}
	want := "kind delete cluster --name " + testCluster + " --kubeconfig " + cluster.Kubeconfig()
	if deleted := fake.ran("kind delete cluster"); !slices.Equal(deleted, []string{want}) {
		t.Errorf("delete commands = %q, want %q", deleted, want)
	}
	if exists(t, sentinel(opts)) {
		t.Errorf("%s still exists after Teardown", sentinel(opts))
	}
}

func TestKeepPreservesTheCluster(t *testing.T) {
	t.Run("after a successful create", func(t *testing.T) {
		fake := noCluster()
		opts := options(t)
		opts.Keep = true
		cluster := mustCreate(t, fake, opts)

		if err := cluster.Teardown(t.Context()); err != nil {
			t.Fatalf("Teardown() error: %v", err)
		}
		if deleted := fake.ran("kind delete"); len(deleted) != 0 {
			t.Errorf("delete commands = %q, want none with Keep", deleted)
		}
		if !exists(t, sentinel(opts)) {
			t.Errorf("%s does not exist: the next run could not reuse the cluster", sentinel(opts))
		}
		// --retain keeps the nodes of a cluster that fails to start.
		if created := fake.ran("kind create cluster"); len(created) != 1 || !strings.Contains(created[0], "--retain") {
			t.Errorf("create commands = %q, want one with --retain", created)
		}
	})

	t.Run("after a failed create", func(t *testing.T) {
		fake := noCluster()
		fake.createErr = errors.New("kind failed")
		opts := options(t)
		opts.Keep = true

		if _, err := create(t.Context(), opts, fake.dependencies()); err == nil {
			t.Error("create() error = nil, want the kind failure")
		}
		if deleted := fake.ran("kind delete"); len(deleted) != 0 {
			t.Errorf("delete commands = %q, want none with Keep", deleted)
		}
		if !exists(t, sentinel(opts)) {
			t.Errorf("%s does not exist: the next run would not know the cluster as its own", sentinel(opts))
		}
	})
}

func TestExportLogs(t *testing.T) {
	fake := noCluster()
	cluster := mustCreate(t, fake, options(t))

	if err := cluster.ExportLogs(t.Context(), "/diagnostics"); err != nil {
		t.Fatalf("ExportLogs() error: %v", err)
	}
	want := "kind export logs /diagnostics --name " + testCluster
	if exported := fake.ran("kind export logs"); !slices.Equal(exported, []string{want}) {
		t.Errorf("export commands = %q, want %q", exported, want)
	}
}

func TestLoadImageArchive(t *testing.T) {
	fake := noCluster()
	cluster := mustCreate(t, fake, options(t))

	if err := cluster.LoadImageArchive(t.Context(), "/images/kumaco.tar"); err != nil {
		t.Fatalf("LoadImageArchive() error: %v", err)
	}
	want := "kind load image-archive /images/kumaco.tar --name " + testCluster
	if loaded := fake.ran("kind load"); !slices.Equal(loaded, []string{want}) {
		t.Errorf("load commands = %q, want %q", loaded, want)
	}
}

// The run says what it will do with the cluster before it changes anything:
// only `kind get clusters`, which tells whether the cluster is running, may
// come first.
func TestThePolicyIsAnnouncedBeforeStarting(t *testing.T) {
	for _, tt := range []struct {
		name    string
		running bool
		keep    bool
		want    []string
	}{
		{name: "create and delete", want: []string{"will be created", "will be deleted"}},
		{name: "create and keep", keep: true, want: []string{"will be created", "will be kept"}},
		{name: "reuse and delete", running: true, want: []string{"will be reused", "will be deleted"}},
		{name: "reuse and keep", running: true, keep: true, want: []string{"will be reused", "will be kept"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fake := noCluster()
			opts := options(t)
			opts.Keep = tt.keep
			if tt.running {
				fake = runningCluster()
				recordEarlierRun(t, opts)
			}
			var announcement string
			opts.Log = func(format string, _ ...any) {
				if slices.Equal(fake.commands, []string{"kind get clusters"}) {
					announcement += format
				}
			}

			mustCreate(t, fake, opts)

			for _, want := range tt.want {
				if !strings.Contains(announcement, want) {
					t.Errorf("announcement = %q, want it to contain %q", announcement, want)
				}
			}
		})
	}
}
