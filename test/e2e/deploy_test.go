//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/kubereboot/kumaco/test/e2e/kind"
)

const (
	// The Helm chart deploys kumaco into kube-system by default.
	kumacoNamespace = "kube-system"
	kumacoRelease   = "kumaco"
	chartPath       = "../../charts/kumaco"

	// heartbeatInterval is short, so that the scenarios see heartbeats.
	heartbeatInterval = 10 * time.Second
)

// deployKumaco provisions kumaco into the cluster of the run: the image that
// `mise run image` built is loaded into the nodes, and the Helm chart of the
// repository deploys it, with two replicas. Nothing is pulled.
func deployKumaco(ctx context.Context, cluster *kind.Cluster) error {
	if _, err := os.Stat(*imageArchive); err != nil {
		return fmt.Errorf("the kumaco image is not built (%w): run the suite with `mise run e2e`, which builds it", err)
	}
	logf("Loading the kumaco image into kind cluster %q.", cluster.Name())
	if err := cluster.LoadImageArchive(ctx, *imageArchive); err != nil {
		return err
	}
	logf("Deploying kumaco with its Helm chart.")
	ctx, cancel := context.WithTimeout(ctx, 6*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, "helm", "upgrade", "--install", kumacoRelease, chartPath,
		"--kubeconfig", cluster.Kubeconfig(),
		"--namespace", kumacoNamespace,
		"--set", "image.pullPolicy=Never",
		"--set", "replicas=2",
		"--set", "controller.heartbeatInterval="+heartbeatInterval.String(),
		"--wait", "--timeout", "5m")
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	if err := command.Run(); err != nil {
		return fmt.Errorf("%s: %w\n%s", strings.Join(command.Args, " "), err, output.String())
	}
	return nil
}
