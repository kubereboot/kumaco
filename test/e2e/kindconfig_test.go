package e2e

import (
	"os"
	"regexp"
	"testing"

	"sigs.k8s.io/yaml"
)

// kindConfigPath is the configuration of the e2e cluster.
const kindConfigPath = "kind.yaml"

// An e2e run must be reproducible: every node runs the same image, pinned by
// digest (AGENTS.md, "Environment and lifecycle"). Without an image, kind
// would fall back to the default of whichever kind binary runs.
func TestKindNodeImagesArePinnedByDigest(t *testing.T) {
	data, err := os.ReadFile(kindConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Nodes []struct {
			Role  string `json:"role"`
			Image string `json:"image"`
		} `json:"nodes"`
	}
	if err := yaml.Unmarshal(data, &config); err != nil {
		t.Fatalf("parsing %s: %v", kindConfigPath, err)
	}
	if len(config.Nodes) == 0 {
		t.Fatalf("%s declares no nodes", kindConfigPath)
	}

	pinned := regexp.MustCompile(`^kindest/node:v\d+\.\d+\.\d+@sha256:[0-9a-f]{64}$`)
	for i, node := range config.Nodes {
		if !pinned.MatchString(node.Image) {
			t.Errorf("node %d (%s): image = %q, want kindest/node:v<version>@sha256:<digest>", i, node.Role, node.Image)
		}
		if node.Image != config.Nodes[0].Image {
			t.Errorf("node %d (%s): image = %q, want the image of the other nodes, %q",
				i, node.Role, node.Image, config.Nodes[0].Image)
		}
	}
}
