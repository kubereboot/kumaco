// Package kind manages the disposable kind cluster of an e2e run, following
// AGENTS.md ("E2e: provision everything, never break Kured"):
//
//   - the cluster is named explicitly. A run creates it when it is not
//     running, and records that in a sentinel: a directory named after the
//     cluster, in the state directory of the runs;
//   - a running cluster is only used when its sentinel exists, which means that
//     an earlier run created it and kept it. Any other cluster is neither used
//     nor deleted;
//   - the run has its own kubeconfig file, in the sentinel, so it can only
//     reach that cluster, and the developer's kubeconfig and current context
//     are left alone;
//   - the cluster and its sentinel are deleted when the run ends, unless Keep
//     is set.
package kind

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// Bounds of the kind commands.
const (
	listTimeout   = time.Minute
	createTimeout = 10 * time.Minute
	deleteTimeout = 5 * time.Minute
	exportTimeout = 5 * time.Minute
	loadTimeout   = 5 * time.Minute

	// controlPlaneWait is how long `kind create cluster` waits for the control
	// plane to be ready.
	controlPlaneWait = "5m"
)

// The names kind accepts, without a leading "-" that kind would read as a
// flag. Such a name is also a safe directory name for the sentinel.
var validName = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]*$`)

// Options designate the cluster of an e2e run.
type Options struct {
	// Name of the kind cluster. Required: a run never chooses its cluster.
	Name string
	// ConfigPath is the kind cluster configuration file.
	ConfigPath string
	// Keep preserves the cluster and its workloads for debugging, instead of
	// deleting them when the run ends. The next run reuses the cluster.
	Keep bool
	// StateDir is where the runs record the clusters they created. Required.
	StateDir string
	// Log receives the progress of the run.
	Log func(format string, args ...any)
}

// Cluster is the kind cluster of a run, returned by Create.
type Cluster struct {
	name     string
	sentinel string
	keep     bool
	log      func(format string, args ...any)
	run      commandRunner
}

// commandRunner runs a command and returns its standard output.
type commandRunner func(ctx context.Context, name string, args ...string) (string, error)

// dependencies are what the unit tests replace: the kind binary and the API
// server.
type dependencies struct {
	run commandRunner
	// apiNodes returns the names of the nodes served by the API server that a
	// kubeconfig file points to.
	apiNodes      func(ctx context.Context, kubeconfig string) ([]string, error)
	verifyTimeout time.Duration
	pollInterval  time.Duration
}

// Create provides the designated kind cluster: it creates it, or reuses the
// one an earlier run kept. It then verifies that the kubeconfig of the run
// reaches it. On error, the cluster is deleted unless Keep is set.
func Create(ctx context.Context, opts Options) (*Cluster, error) {
	return create(ctx, opts, dependencies{
		run:           runCommand,
		apiNodes:      apiNodeNames,
		verifyTimeout: 2 * time.Minute,
		pollInterval:  2 * time.Second,
	})
}

func create(ctx context.Context, opts Options, deps dependencies) (*Cluster, error) {
	if !validName.MatchString(opts.Name) {
		return nil, fmt.Errorf("the kind cluster of the run must be named explicitly, "+
			"with lowercase letters, digits, \".\" and \"-\": got %q", opts.Name)
	}
	if opts.StateDir == "" {
		return nil, errors.New("the directory recording the clusters created by e2e runs is not set")
	}
	stateDir, err := filepath.Abs(opts.StateDir)
	if err != nil {
		return nil, err
	}
	if opts.Log == nil {
		opts.Log = func(string, ...any) {}
	}
	cluster := &Cluster{
		name:     opts.Name,
		sentinel: filepath.Join(stateDir, opts.Name),
		keep:     opts.Keep,
		log:      opts.Log,
		run:      deps.run,
	}

	running, err := cluster.isRunning(ctx)
	if err != nil {
		return nil, err
	}
	recorded, err := cluster.isRecorded()
	if err != nil {
		return nil, err
	}
	if running && !recorded {
		return nil, fmt.Errorf("kind cluster %q is running, but was not created by an e2e run (%s does not exist): "+
			"a run only uses, and only deletes, a cluster that a run created. "+
			"Choose another name, or delete it with `kind delete cluster --name %s` if it is disposable",
			cluster.name, cluster.sentinel, cluster.name)
	}
	cluster.announce(running)

	if running {
		err = cluster.reuse(ctx)
	} else {
		err = cluster.create(ctx, opts.ConfigPath)
	}
	if err == nil {
		err = cluster.verify(ctx, deps)
	}
	if err != nil {
		// The sentinel says that the cluster is the one of an e2e run.
		return nil, errors.Join(err, cluster.Teardown(context.WithoutCancel(ctx)))
	}
	return cluster, nil
}

func (c *Cluster) isRunning(ctx context.Context) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, listTimeout)
	defer cancel()
	clusters, err := c.run(ctx, "kind", "get", "clusters")
	return slices.Contains(strings.Fields(clusters), c.name), err
}

func (c *Cluster) isRecorded() (bool, error) {
	_, err := os.Stat(c.sentinel)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

// announce states the deletion policy before the run changes anything.
func (c *Cluster) announce(running bool) {
	start := "will be created"
	if running {
		start = "was kept by an earlier run: it will be reused"
	}
	end := "It will be deleted when the run ends, whether it succeeds or fails. Set the keep option to preserve it."
	if c.keep {
		end = fmt.Sprintf("It will be kept after the run: delete it with `kind delete cluster --name %s`.", c.name)
	}
	c.log("kind cluster %q "+start+". "+end, c.name)
}

// create records the cluster in its sentinel, then creates it: even a run
// killed during the creation leaves a cluster known as the one of a run.
func (c *Cluster) create(ctx context.Context, configPath string) error {
	// A sentinel without a cluster is a leftover: the cluster was deleted by hand.
	if err := os.RemoveAll(c.sentinel); err != nil {
		return err
	}
	if err := os.MkdirAll(c.sentinel, 0o700); err != nil {
		return err
	}
	args := []string{
		"create", "cluster",
		"--name", c.name,
		"--config", configPath,
		"--kubeconfig", c.Kubeconfig(),
		"--wait", controlPlaneWait,
	}
	if c.keep {
		// Keep the nodes of a cluster that fails to start, too.
		args = append(args, "--retain")
	}
	return c.kind(ctx, createTimeout, args...)
}

// reuse writes the kubeconfig of the run again: the run that created the
// cluster may have been killed before it had one.
func (c *Cluster) reuse(ctx context.Context) error {
	return c.kind(ctx, exportTimeout, "export", "kubeconfig", "--name", c.name, "--kubeconfig", c.Kubeconfig())
}

// verify waits until the API server of the kubeconfig serves exactly the nodes
// of the kind cluster, which proves that the run targets that cluster.
func (c *Cluster) verify(ctx context.Context, deps dependencies) error {
	ctx, cancel := context.WithTimeout(ctx, listTimeout+deps.verifyTimeout)
	defer cancel()
	out, err := c.run(ctx, "kind", "get", "nodes", "--name", c.name)
	if err != nil {
		return err
	}
	want := strings.Fields(out)
	slices.Sort(want)
	if len(want) == 0 {
		return fmt.Errorf("kind cluster %q has no nodes", c.name)
	}

	var got []string
	var apiErr error
	err = wait.PollUntilContextTimeout(ctx, deps.pollInterval, deps.verifyTimeout, true,
		func(ctx context.Context) (bool, error) {
			got, apiErr = deps.apiNodes(ctx, c.Kubeconfig())
			slices.Sort(got)
			return apiErr == nil && slices.Equal(got, want), nil
		})
	if err != nil {
		return fmt.Errorf("%s does not reach kind cluster %q: its API server has the nodes %q, "+
			"the kind cluster has %q (last API error: %v)", c.Kubeconfig(), c.name, got, want, apiErr)
	}
	return nil
}

// Name returns the name of the kind cluster.
func (c *Cluster) Name() string { return c.name }

// Kubeconfig returns the kubeconfig file of the run. It only contains this
// cluster.
func (c *Cluster) Kubeconfig() string { return filepath.Join(c.sentinel, "kubeconfig") }

// RESTConfig returns a client configuration for this cluster, read from the
// kubeconfig file of the run only.
func (c *Cluster) RESTConfig() (*rest.Config, error) {
	return restConfig(c.Kubeconfig())
}

// ExportLogs writes the logs of the cluster (nodes, kubelets, containers) to
// dir. Call it before Teardown when a run fails.
func (c *Cluster) ExportLogs(ctx context.Context, dir string) error {
	return c.kind(ctx, exportTimeout, "export", "logs", dir, "--name", c.name)
}

// LoadImageArchive loads the images of an archive (`docker save`) into every
// node of the cluster, so that pods run them without pulling.
func (c *Cluster) LoadImageArchive(ctx context.Context, archive string) error {
	return c.kind(ctx, loadTimeout, "load", "image-archive", archive, "--name", c.name)
}

// Teardown deletes the cluster and its sentinel, unless Keep was set.
func (c *Cluster) Teardown(ctx context.Context) error {
	if c.keep {
		c.log("kind cluster %q is kept: the next run reuses it. Use it with KUBECONFIG=%s, "+
			"and delete it with `kind delete cluster --name %s`.", c.name, c.Kubeconfig(), c.name)
		return nil
	}
	c.log("Deleting kind cluster %q.", c.name)
	if err := c.kind(ctx, deleteTimeout, "delete", "cluster", "--name", c.name, "--kubeconfig", c.Kubeconfig()); err != nil {
		// The cluster may still exist: keep its sentinel, so that the next run
		// knows it as the cluster of a run.
		return err
	}
	return os.RemoveAll(c.sentinel)
}

func (c *Cluster) kind(ctx context.Context, timeout time.Duration, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	_, err := c.run(ctx, "kind", args...)
	return err
}

// runCommand runs a command until ctx is done. Its error holds what the
// command wrote, as a diagnostic.
func runCommand(ctx context.Context, name string, args ...string) (string, error) {
	var stdout, output bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &output
	// Do not wait forever for a child process holding the output open.
	cmd.WaitDelay = 10 * time.Second
	if err := cmd.Run(); err != nil {
		output.Write(stdout.Bytes())
		return "", fmt.Errorf("%s %s: %w\n%s", name, strings.Join(args, " "), err, output.String())
	}
	return stdout.String(), nil
}

// restConfig reads a kubeconfig file, and nothing else: neither KUBECONFIG nor
// the default kubeconfig is consulted.
func restConfig(kubeconfig string) (*rest.Config, error) {
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{ExplicitPath: kubeconfig},
		&clientcmd.ConfigOverrides{},
	).ClientConfig()
}

func apiNodeNames(ctx context.Context, kubeconfig string) ([]string, error) {
	config, err := restConfig(kubeconfig)
	if err != nil {
		return nil, err
	}
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	nodes, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(nodes.Items))
	for _, node := range nodes.Items {
		names = append(names, node.Name)
	}
	return names, nil
}
