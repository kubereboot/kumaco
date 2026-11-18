//go:build envtest

// Package manifests checks the two ways kumaco is shipped: the kustomize
// manifests (config/) and the Helm chart (charts/kumaco). Both are rendered
// with the CLIs that mise pins, compared with the generated CRD and role, and
// validated by a real API server.
package manifests

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubereboot/kumaco/test/testenv"
)

var apiServer client.Client

func TestMain(m *testing.M) {
	os.Exit(runTests(m))
}

func runTests(m *testing.M) int {
	config, stop, err := testenv.Start()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer func() {
		if err := stop(); err != nil {
			fmt.Fprintln(os.Stderr, "stopping the envtest control plane:", err)
		}
	}()
	if apiServer, err = client.New(config, client.Options{}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return m.Run()
}

// root is the root of the repository.
func root() string {
	_, thisFile, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(thisFile), "..", "..")
}

// render runs a pinned CLI at the root of the repository, and parses the
// manifests it prints. A missing CLI fails the test.
func render(t *testing.T, name string, args ...string) []*unstructured.Unstructured {
	t.Helper()
	command := exec.CommandContext(t.Context(), name, args...)
	command.Dir = root()
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s(run this test with `mise run test:envtest`, which provides the pinned %s)",
			name, strings.Join(args, " "), err, stderr.String(), name)
	}
	var objects []*unstructured.Unstructured
	decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(output), 4096)
	for {
		object := &unstructured.Unstructured{}
		if err := decoder.Decode(&object.Object); errors.Is(err, io.EOF) {
			return objects
		} else if err != nil {
			t.Fatalf("parsing the output of %s: %v", name, err)
		}
		if len(object.Object) > 0 {
			objects = append(objects, object)
		}
	}
}

func kustomize(t *testing.T, directory string) []*unstructured.Unstructured {
	t.Helper()
	return render(t, "kustomize", "build", directory)
}

// helm renders the chart with the given values file content.
func helm(t *testing.T, release, values string) []*unstructured.Unstructured {
	t.Helper()
	file := filepath.Join(t.TempDir(), "values.yaml")
	if err := os.WriteFile(file, []byte(values), 0o600); err != nil {
		t.Fatal(err)
	}
	return render(t, "helm", "template", release, "charts/kumaco", "--values", file)
}

// The values of a second controller, for team A, next to the default one.
const teamValues = `
crd:
  install: false
controller:
  nodeSelector: team=a
  windowLabelSelector: MaintenanceWindowTeam=A
rbac:
  nodeNames: [team-a-node-1, team-a-node-2]
`

func key(object *unstructured.Unstructured) string {
	return object.GetKind() + "/" + object.GetName()
}

func keys(objects []*unstructured.Unstructured) []string {
	result := make([]string, len(objects))
	for i, object := range objects {
		result[i] = key(object)
	}
	slices.Sort(result)
	return result
}

func find(t *testing.T, objects []*unstructured.Unstructured, wanted string) *unstructured.Unstructured {
	t.Helper()
	for _, object := range objects {
		if key(object) == wanted {
			return object
		}
	}
	t.Fatalf("no %s in %v", wanted, keys(objects))
	return nil
}

// generated reads a file that `mise run generate` writes.
func generated(t *testing.T, path string) *unstructured.Unstructured {
	t.Helper()
	objects := render(t, "cat", path)
	if len(objects) != 1 {
		t.Fatalf("%s holds %d objects, want 1", path, len(objects))
	}
	return objects[0]
}

func field(t *testing.T, object *unstructured.Unstructured, path ...string) any {
	t.Helper()
	value, found, err := unstructured.NestedFieldNoCopy(object.Object, path...)
	if err != nil || !found {
		t.Fatalf("%s has no %s (%v)", key(object), strings.Join(path, "."), err)
	}
	return value
}

// container returns the only container of a Deployment.
func container(t *testing.T, deployment *unstructured.Unstructured) map[string]any {
	t.Helper()
	containers := field(t, deployment, "spec", "template", "spec", "containers").([]any)
	if len(containers) != 1 {
		t.Fatalf("%s has %d containers, want 1", key(deployment), len(containers))
	}
	return containers[0].(map[string]any)
}

func args(t *testing.T, deployment *unstructured.Unstructured) []string {
	t.Helper()
	var result []string
	for _, arg := range container(t, deployment)["args"].([]any) {
		result = append(result, arg.(string))
	}
	return result
}

// The kustomize manifests and the Helm chart deploy the same objects into
// kube-system by default, with the generated CRD and role, leader election,
// and a container that needs nothing from its image: a numeric non-root user
// and a read-only root file system.
//
// README: #configuration, #security-and-access-rights, #replicas-of-the-same-controller
func TestTheDefaultDeployment(t *testing.T) {
	fromKustomize := kustomize(t, "config/default")
	fromHelm := helm(t, "kumaco", "")
	if !slices.Equal(keys(fromKustomize), keys(fromHelm)) {
		t.Errorf("kustomize deploys %v\nHelm deploys      %v\nwant the same objects", keys(fromKustomize), keys(fromHelm))
	}
	for tool, objects := range map[string][]*unstructured.Unstructured{"kustomize": fromKustomize, "helm": fromHelm} {
		t.Run(tool, func(t *testing.T) {
			wantNamespace(t, objects, "kube-system")
			wantGenerated(t, objects)
			wantHardenedDeployment(t, find(t, objects, "Deployment/kumaco"))
			wantLeaseAccess(t, find(t, objects, "Role/kumaco-leader-election"), "kumaco")
		})
	}
}

// wantNamespace checks that every namespaced object is in the namespace.
func wantNamespace(t *testing.T, objects []*unstructured.Unstructured, namespace string) {
	t.Helper()
	for _, object := range objects {
		clusterScoped := strings.HasPrefix(object.GetKind(), "Cluster") || object.GetKind() == "CustomResourceDefinition"
		if !clusterScoped && object.GetNamespace() != namespace {
			t.Errorf("%s is in the namespace %q, want %s", key(object), object.GetNamespace(), namespace)
		}
	}
}

// wantGenerated checks that the CRD and the rules of the ClusterRole are the
// ones that `mise run generate` wrote: the two packagings cannot drift.
func wantGenerated(t *testing.T, objects []*unstructured.Unstructured) {
	t.Helper()
	crd := generated(t, "config/crd/bases/kumaco.kured.dev_maintenancewindows.yaml")
	role := generated(t, "config/rbac/role.yaml")
	if got := field(t, find(t, objects, "CustomResourceDefinition/maintenancewindows.kumaco.kured.dev"), "spec"); !reflect.DeepEqual(got, crd.Object["spec"]) {
		t.Errorf("the CRD is not the one that `mise run generate` wrote")
	}
	if got := field(t, find(t, objects, "ClusterRole/kumaco"), "rules"); !reflect.DeepEqual(got, role.Object["rules"]) {
		t.Errorf("the rules of the ClusterRole are %v, want the generated ones: %v", got, role.Object["rules"])
	}
}

func wantArgs(t *testing.T, deployment *unstructured.Unstructured, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !slices.Contains(args(t, deployment), want) {
			t.Errorf("the arguments are %v, want %s among them", args(t, deployment), want)
		}
	}
}

func wantHardenedDeployment(t *testing.T, deployment *unstructured.Unstructured) {
	t.Helper()
	wantArgs(t, deployment, "--leader-elect", "--controller-name=kumaco", "--metrics-bind-address=:8443")
	if image := container(t, deployment)["image"].(string); !strings.HasPrefix(image, "ghcr.io/kubereboot/kumaco:") || strings.HasSuffix(image, ":latest") {
		t.Errorf("the image is %q, want a tag of ghcr.io/kubereboot/kumaco that is not latest", image)
	}
	pod := field(t, deployment, "spec", "template", "spec", "securityContext").(map[string]any)
	security := container(t, deployment)["securityContext"].(map[string]any)
	if pod["runAsNonRoot"] != true || fmt.Sprint(pod["runAsUser"]) != "65532" ||
		security["readOnlyRootFilesystem"] != true || security["allowPrivilegeEscalation"] != false {
		t.Errorf("security contexts: pod %v, container %v; want a numeric non-root user and a read-only root file system", pod, security)
	}
	if name := field(t, deployment, "spec", "template", "spec", "serviceAccountName"); name != "kumaco" {
		t.Errorf("the service account is %q, want kumaco, as in the FlowSchema of the README", name)
	}
}

// wantLeaseAccess checks that the Role only gives access to the Lease named
// after the controller.
func wantLeaseAccess(t *testing.T, role *unstructured.Unstructured, lease string) {
	t.Helper()
	rule := field(t, role, "rules").([]any)[1].(map[string]any)
	if !reflect.DeepEqual(rule["resourceNames"], []any{lease}) {
		t.Errorf("the Lease access of %s is for %v, want only the Lease %s", key(role), rule["resourceNames"], lease)
	}
}

// Both support one controller per team, next to the others: it has its own
// name, node scope and window selector, does not ship the CRD again, and can
// only write the status of the nodes that its ClusterRole names.
//
// README: #the-harder-approach-the-service-account-limitations, #the-softer-approach-the-window-label-selector, #multi-tenancy-while-doing-self-service-is-not-an-afterthought
func TestTheDeploymentOfOneControllerPerTeam(t *testing.T) {
	defaults := keys(kustomize(t, "config/default"))
	for _, tt := range []struct {
		tool           string
		objects        []*unstructured.Unstructured
		leaderElection string
	}{
		{"kustomize", kustomize(t, "config/examples/team-a"), "Role/kumaco-leader-election-team-a"},
		{"helm", helm(t, "kumaco-team-a", teamValues), "Role/kumaco-team-a-leader-election"},
	} {
		t.Run(tt.tool, func(t *testing.T) {
			for _, name := range keys(tt.objects) {
				if slices.Contains(defaults, name) || strings.HasPrefix(name, "CustomResourceDefinition/") {
					t.Errorf("%s is also an object of the default deployment: the two cannot be deployed together", name)
				}
			}
			wantArgs(t, find(t, tt.objects, "Deployment/kumaco-team-a"),
				"--controller-name=kumaco-team-a", "--node-selector=team=a", "--window-label-selector=MaintenanceWindowTeam=A")
			wantNodeWritesRestricted(t, find(t, tt.objects, "ClusterRole/kumaco-team-a"), "team-a-node-1", "team-a-node-2")
			wantLeaseAccess(t, find(t, tt.objects, tt.leaderElection), "kumaco-team-a")
		})
	}
}

// wantNodeWritesRestricted checks that the rules are the generated ones, with
// only the node status writes restricted to the named nodes.
func wantNodeWritesRestricted(t *testing.T, role *unstructured.Unstructured, nodes ...any) {
	t.Helper()
	rules := field(t, role, "rules").([]any)
	wantRules := generated(t, "config/rbac/role.yaml").Object["rules"].([]any)
	if len(rules) != len(wantRules) {
		t.Fatalf("%d rules, want the %d generated ones", len(rules), len(wantRules))
	}
	for i := range rules {
		rule, want := rules[i].(map[string]any), wantRules[i].(map[string]any)
		names := rule["resourceNames"]
		delete(rule, "resourceNames")
		if !reflect.DeepEqual(rule, want) {
			t.Errorf("rule %d is %v, want the generated one: %v", i, rule, want)
		}
		wantNames := any(nil)
		if reflect.DeepEqual(want["resources"], []any{"nodes/status"}) {
			wantNames = nodes
		}
		if !reflect.DeepEqual(names, wantNames) {
			t.Errorf("rule %d (%v) is restricted to %v, want %v: only the node status writes are restricted", i, want["resources"], names, wantNames)
		}
	}
}

// The chart can give kumaco a newer time zone database than the one it embeds:
// a mounted zoneinfo.zip, and ZONEINFO.
//
// README: #time-zones
func TestTheChartMountsATimeZoneDatabase(t *testing.T) {
	objects := helm(t, "kumaco", `
extraEnv:
  - name: ZONEINFO
    value: /zoneinfo/zoneinfo.zip
extraVolumes:
  - name: zoneinfo
    configMap:
      name: zoneinfo
extraVolumeMounts:
  - name: zoneinfo
    mountPath: /zoneinfo
    readOnly: true
`)
	deployment := find(t, objects, "Deployment/kumaco")
	env := container(t, deployment)["env"].([]any)[0].(map[string]any)
	mount := container(t, deployment)["volumeMounts"].([]any)[0].(map[string]any)
	volume := field(t, deployment, "spec", "template", "spec", "volumes").([]any)[0].(map[string]any)
	if env["name"] != "ZONEINFO" || env["value"] != "/zoneinfo/zoneinfo.zip" || mount["mountPath"] != "/zoneinfo" || volume["name"] != "zoneinfo" {
		t.Errorf("env %v, mount %v, volume %v; want the time zone database mounted and ZONEINFO set", env, mount, volume)
	}
}

// A real API server accepts every object of every rendering.
//
// README: #configuration
func TestTheAPIServerAcceptsTheManifests(t *testing.T) {
	for name, objects := range map[string][]*unstructured.Unstructured{
		"kustomize, default":  kustomize(t, "config/default"),
		"kustomize, per team": kustomize(t, "config/examples/team-a"),
		"helm, default":       helm(t, "kumaco", ""),
		"helm, per team":      helm(t, "kumaco-team-a", teamValues),
	} {
		t.Run(name, func(t *testing.T) {
			for _, object := range objects {
				err := apiServer.Patch(t.Context(), object, client.Apply, client.DryRunAll, client.FieldOwner("manifests-test"), client.ForceOwnership)
				if err != nil {
					t.Errorf("applying %s (dry run): %v", key(object), err)
				}
			}
		})
	}
}
