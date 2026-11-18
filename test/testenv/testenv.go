// Package testenv starts the envtest control plane (a real kube-apiserver and
// etcd) for the tests of behaviour that depends on API server semantics. See
// AGENTS.md, "Tests and required checks", for what belongs at this level.
//
// The tests using it have the "envtest" build tag, and run with
// `mise run test:envtest`.
package testenv

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// assetsEnv is the variable in which envtest reads the directory of its
// control-plane binaries.
const assetsEnv = "KUBEBUILDER_ASSETS"

// Start starts the control plane that mise pins. It returns the configuration
// to reach its API server, and the function that stops it.
//
// Missing binaries are an error: the caller must fail, not skip its tests.
func Start() (*rest.Config, func() error, error) {
	if err := checkAssets(os.Getenv(assetsEnv)); err != nil {
		return nil, nil, err
	}
	environment := &envtest.Environment{}
	config, err := environment.Start()
	if err != nil {
		return nil, nil, fmt.Errorf("starting the envtest control plane: %w", err)
	}
	return config, environment.Stop, nil
}

// checkAssets requires the binaries to be designated explicitly. Without this,
// envtest would look for whatever is installed in /usr/local/kubebuilder/bin.
func checkAssets(dir string) error {
	if dir == "" {
		return fmt.Errorf("%s is not set: run these tests with `mise run test:envtest`, "+
			"which points it to the envtest binaries pinned in mise.toml", assetsEnv)
	}
	for _, binary := range []string{"etcd", "kube-apiserver"} {
		if _, err := os.Stat(filepath.Join(dir, binary)); err != nil {
			return fmt.Errorf("%s=%s has no %s binary (%w): install the pinned envtest binaries with `mise install`",
				assetsEnv, dir, binary, err)
		}
	}
	return nil
}

// InstallCRD installs the generated MaintenanceWindow CRD in the control plane,
// and waits until the API server serves it.
func InstallCRD(config *rest.Config) error {
	_, thisFile, _, _ := runtime.Caller(0)
	bases := filepath.Join(filepath.Dir(thisFile), "..", "..", "config", "crd", "bases")
	_, err := envtest.InstallCRDs(config, envtest.CRDInstallOptions{Paths: []string{bases}, ErrorIfPathMissing: true})
	if err != nil {
		return fmt.Errorf("installing the CRD of %s: %w", bases, err)
	}
	return nil
}
