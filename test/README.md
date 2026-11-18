# Testing kumaco

The rules are in [AGENTS.md](../AGENTS.md) ("Tests and required checks" and "E2e"). This
page says how to run the tests, and where a new test goes.

All tools are installed by [mise](https://mise.jdx.dev), at the versions pinned in
`mise.toml` and `mise.lock`: run `mise install` once. Nothing else is needed, except for the
e2e suite (see below).

| Task | What it runs | Needs |
|---|---|---|
| `mise run test` | Unit tests. | Nothing. |
| `mise run test:race` | Unit tests with the race detector. | A C compiler. |
| `mise run bench` | Benchmarks: the schedule, and the controller with 10,000 representative nodes. | Nothing. |
| `mise run test:fuzz` | Fuzz tests of the parsers, 30 seconds each. | Nothing. |
| `mise run test:envtest` | Unit tests and the envtest suite, including the rendering of the kustomize manifests and of the Helm chart. | Nothing: mise installs the control-plane binaries, `kustomize` and `helm`. |
| `mise run image` | Builds the kumaco image for this machine, as `ghcr.io/kubereboot/kumaco:dev`. | Docker or Podman (`CONTAINER_TOOL`, by default the provider of kind). |
| `mise run e2e` | Kind e2e suite, after `mise run image`. | Docker or Podman, and network access to pull the node image. |
| `mise run lint:contracts` | The links between `README.md` and the tests. | Nothing. |
| `mise run lint:deps` | The dependency policy: direct modules, imports, and the reviews of the exceptions in `hack/lint-deps/exceptions.json`. | Nothing. |
| `mise run lint:complexity` | The cognitive complexity of every function, at most 15 (`gocognit`, through golangci-lint). | Nothing. |
| `mise run lint` | Formatting, `go vet`, and `logcheck` (Kubernetes' checker of logging calls). | Nothing. |
| `mise run generate:check` | `mise run generate`, failing if the generated code or manifests were not up to date. | Nothing. |

| `mise run scan:trivy` | Trivy scans of the repository, of its configuration (Dockerfile, manifests, chart) and of the image, after `mise run image`. HIGH and CRITICAL findings fail, vulnerabilities only when a fix exists; exceptions are in `.trivyignore.yaml`. | Docker or Podman, and network access to download the vulnerability database. |
| `mise run scan:snyk` | Snyk dependency and license scan. CI only. | `SNYK_TOKEN`: it fails without it. |

A missing prerequisite fails a task. It never skips tests.

## Where does a test go?

Use the lowest level that can prove the behaviour:

1. **Unit tests** for domain decisions: explicit inputs, no API server. They are ordinary
   `_test.go` files, next to the code.
2. **envtest** for what depends on API server semantics (server-side apply, `resourceVersion`
   conflicts, CRD validation, status subresources, RBAC). Give the file the build tag
   `//go:build envtest`, and start the control plane in `TestMain` with `testenv.Start()`
   (see `test/testenv/apiserver_test.go`).
3. **Kind e2e** for the integration with real components. Scenarios go in `test/e2e`, with
   the build tag `//go:build e2e`.

Use Go's `testing` package only.

## Linking tests to the README

`README.md` is the specification. A test names the sections it enforces with a comment
holding their heading anchors (the ones GitHub generates):

```go
// README: #heartbeat-contract
// README: #time-zones, #cel-triggers
```

`mise run lint:contracts` fails when:
- a link names a section that does not exist (it was renamed or removed);
- no test links to a section, unless `hack/lint-contracts/unlinked-sections.txt` lists it as
  `pending` (not implemented yet) or `informative` (no testable behaviour), with a reason;
- a test links to a section that is still listed as `pending`.

So the change that adds the first test of a section also removes its `pending` line.

## The e2e cluster

```sh
mise run e2e                           # create the cluster, test, delete it
mise run e2e -- -keep                  # keep the cluster for debugging
mise run e2e -- -cluster=other-name    # use another cluster name
```

- **The cluster is named explicitly.** The tests have no default: `go test` fails without
  `-cluster`. The mise task designates the kind cluster `kumaco-e2e`; a `-cluster` argument
  replaces it.
- **The run creates the cluster when it is not running**, and records that in a sentinel: the
  directory `test/e2e/.clusters/<name>`, which git ignores.
- **A running cluster is only used when its sentinel exists**, which means that an earlier run
  created it and kept it. The run fails without touching any other cluster of that name: it
  only uses, and only deletes, a cluster that a run created.
- **The cluster is deleted when the run ends**, whether it succeeded, failed, or was
  interrupted with Ctrl-C. This applies to a reused cluster too. The run says so before it
  starts.
- **`-keep` preserves the cluster and its workloads.** The next run reuses them, without
  creating a cluster, so its scenarios see what the previous ones left. A run without `-keep`
  deletes the cluster at its end, and the run after it starts from a new one. You can also
  delete the cluster yourself, with `kind delete cluster --name <name>`.
- **Your kubeconfig is not used.** The run writes its own kubeconfig file in the sentinel
  (`test/e2e/.clusters/<name>/kubeconfig`), and reaches the cluster through that file only:
  your current context is neither read nor changed. Before any scenario runs, the run checks
  that this kubeconfig reaches the nodes of its kind cluster.
- **A failed run exports the logs of the cluster** (`kind export logs`) before the teardown,
  into a new temporary directory, or into `-diagnostics-dir=<dir>`. It prints the path.

**The run provisions kumaco**: it loads the image that `mise run image` built into the nodes,
and deploys it with the Helm chart of the repository, with two replicas. Nothing is pulled
from a registry. Kured and its reporters are not deployed yet: the scenarios publish the
conditions that triggers read themselves, and the Kured compatibility gate of AGENTS.md is
not met (see `hack/lint-contracts/unlinked-sections.txt`).

The cluster is described by `test/e2e/kind.yaml`. Its node image is the default image of the
pinned kind release, by digest: bump both together.

A run that Go kills (the `-timeout` of `go test`, 45 minutes in the task) cannot clean up.
Its cluster keeps its sentinel, so the next run reuses it, then deletes it.

In a scenario, wait with `eventually` (`test/e2e/main_test.go`): it polls with a deadline,
and reports what it last observed when the deadline passes. Do not sleep for a fixed time.
