package testenv

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A missing prerequisite fails the tests: it never skips them, and never falls
// back to control-plane binaries that mise does not pin (AGENTS.md, "Tests and
// required checks").
func TestStartFailsWithoutThePinnedBinaries(t *testing.T) {
	onlyEtcd := t.TempDir()
	if err := os.WriteFile(filepath.Join(onlyEtcd, "etcd"), nil, 0o755); err != nil {
		t.Fatal(err)
	}

	for _, tt := range []struct {
		name   string
		assets string
		want   string
	}{
		{name: "the variable is not set", assets: "", want: "mise run test:envtest"},
		{name: "the directory does not exist", assets: filepath.Join(t.TempDir(), "missing"), want: "etcd"},
		{name: "a binary is missing", assets: onlyEtcd, want: "kube-apiserver"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(assetsEnv, tt.assets)

			config, stop, err := Start()
			if err == nil {
				_ = stop()
				t.Fatalf("Start() = %v, want an error", config)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Start() error = %q, want it to mention %q", err, tt.want)
			}
		})
	}
}
