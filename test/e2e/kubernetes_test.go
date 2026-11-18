//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilversion "k8s.io/apimachinery/pkg/util/version"
	"k8s.io/component-base/version"
)

// kumaco is tested end to end on the node image of the latest kind release,
// which runs the Kubernetes release of the client libraries kumaco is built
// with.
//
// README: #kubernetes-versions
func TestNodesRunTheSupportedKubernetesRelease(t *testing.T) {
	supported := utilversion.MustParseGeneric(version.DefaultKubeBinaryVersion)

	nodes := readyNodes(t)

	for _, node := range nodes {
		kubelet, err := utilversion.ParseGeneric(node.Status.NodeInfo.KubeletVersion)
		if err != nil {
			t.Errorf("node %s: kubelet version %q: %v", node.Name, node.Status.NodeInfo.KubeletVersion, err)
			continue
		}
		if kubelet.Major() != supported.Major() || kubelet.Minor() != supported.Minor() {
			t.Errorf("node %s runs Kubernetes %s, want %s, the release of the client libraries",
				node.Name, node.Status.NodeInfo.KubeletVersion, version.DefaultKubeBinaryVersion)
		}
	}

	server, err := client.Discovery().ServerVersion()
	if err != nil {
		t.Fatalf("reading the API server version: %v", err)
	}
	if got := server.Major + "." + server.Minor; got != version.DefaultKubeBinaryVersion {
		t.Errorf("the API server runs Kubernetes %s (%s), want %s, the release of the client libraries",
			got, server.GitVersion, version.DefaultKubeBinaryVersion)
	}
}

// readyNodes waits until every node is Ready, and returns the nodes.
func readyNodes(t *testing.T) []corev1.Node {
	t.Helper()
	var nodes []corev1.Node
	eventually(t, 5*time.Minute, "every node to be Ready", func(ctx context.Context) (bool, string) {
		list, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
		if err != nil {
			return false, err.Error()
		}
		nodes = list.Items
		var notReady []string
		for _, node := range nodes {
			if !isReady(node) {
				notReady = append(notReady, node.Name)
			}
		}
		return len(nodes) > 0 && len(notReady) == 0,
			fmt.Sprintf("%d nodes, not Ready: [%s]", len(nodes), strings.Join(notReady, ", "))
	})
	return nodes
}

func isReady(node corev1.Node) bool {
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}
