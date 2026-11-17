# Agent development contract

These rules apply to all contributions, including AI-generated changes and automated
version bumps. `README.md` is the authoritative behavioural specification; tests
are where we enforce its contracts. If the specification is ambiguous or conflicts
with observed upstream behaviour, stop and ask rather than silently redefining it.

## Scope and authority

- Read the relevant specification and connected code before editing. Identify the
  affected callers, tests, fixtures, configuration, documentation, and public APIs.
- Make one minimal, atomic change for one requested outcome. Include everything
  necessary for that outcome, but no unrelated cleanup, speculative features,
  dependency upgrades, or architectural rewrites.
- Proceed autonomously within agreed scope. Ask before expanding it, introducing a
  dependency exception, changing a public contract, or performing destructive work.
  An explicitly requested destructive e2e run is limited to its disposable cluster.
- Preserve unrelated work. Do not commit automatically or push without explicit
  permission.
- Unless already authorized by the task, obtain maintainer approval before adding,
  removing, or changing public contracts: CRD schemas and validation, scheduling
  semantics, condition names and ownership, heartbeat contracts, CLI flags,
  compatibility guarantees, metrics (names, types, labels, and meaning), and Go
  packages' exported functions, variables, constants, types, and methods.
- Until v1.0.0, metrics, the CRD API, and exported Go symbols carry no
  compatibility guarantee and may change between releases. Such changes still
  need maintainer approval and must be visible in the release notes (see
  "Versioning and release policy").
- Maintainers are the people listed for this project in
  [maintainers.yaml](https://github.com/kubereboot/.project/blob/main/maintainers.yaml).
  "Maintainer approval" means approval by one of them, given where it can be
  referenced later (a PR review or an issue comment).
- Changes to this file need human maintainer approval. An agent may propose them,
  but must not treat its own proposal as agreed.

## TDD and atomic changes

For each production behaviour change:

1. Write a focused test for the required behaviour. A bug fix starts with a
   regression test.
2. Run it and establish that it fails for the intended reason, not because of an
   unrelated setup or compilation error.
3. Make the smallest production change that passes the test.
4. Refactor only where needed, keeping the tests green.
5. Run the affected checks and review the complete diff before proposing a commit.

Record the red/green commands and results in the change or PR description; no
separate evidence framework is required. Documentation-only changes are exempt.
If the red/green workflow genuinely cannot apply, ask before taking an exception.

Self-review is mandatory, even if another reviewer will review the change:

- Does every changed line serve the requested outcome?
- Are all affected callers, contracts, fixtures, and documentation accounted for?
- Can unrelated cleanup, abstractions, options, or exports be removed?
- Does the change preserve security, error handling, and compatibility?
- Is this one coherent change, or should independent outcomes be split?

Do not reduce scope by omitting necessary tests, validation, or failure handling.
Do not make a failing test pass by weakening an agreed contract.

## Domain-driven design without ceremony

- Use the domain vocabulary in `README.md`: maintenance windows, kinds of
  maintenance (`conditionType`), selectors, triggers, occurrences, concurrency,
  records, starts, and releases.
- Put maintenance decisions and their invariants in domain logic, separate from
  Kubernetes I/O, CLI handling, and process lifecycle. Kubernetes types are not
  categorically forbidden in domain code.
- Prefer pure functions wherever practical, especially for domain decisions: the
  same explicit inputs produce the same outputs, without I/O, hidden clock/global
  state, or mutation of inputs. Keep side effects at the boundaries. Never mutate
  shared informer objects; construct desired changes separately.
- Keep domain decisions testable without a running cluster. Status, metrics, and
  other derived views must not become alternative sources of truth.
- Keep cognitive complexity low: use clear names, focused functions, guard clauses,
  and shallow control flow. Extract meaningful decisions, not arbitrary helpers
  just to satisfy a metric. Do not add abstractions that make a simple flow harder
  to follow.
- Prefer cohesive packages and explicit responsibilities. Introduce interfaces at
  real boundaries when needed, not one per type. Do not require repositories,
  factories, services, or a fixed directory hierarchy merely to claim DDD.
- Reuse existing code and standard-library facilities. Add an abstraction only
  when the current change demonstrates its need.

## Logging

Logs are for the kumaco deployer debugging the controller. Maintenance managers
get events, status, and node condition messages instead (see `README.md`).

- Use the logr API without importing `github.com/go-logr/logr` directly. Get
  loggers with `sigs.k8s.io/controller-runtime/pkg/log.FromContext(ctx)` (or
  `klog.FromContext`), and name the type as `klog.Logger` from `k8s.io/klog/v2`,
  which is an alias of `logr.Logger`. This is the approved reading of the
  dependency policy for logging; it is not a precedent for other modules.
- Keep the logging backend Kubebuilder scaffolds: controller-runtime's zap backend
  (`sigs.k8s.io/controller-runtime/pkg/log/zap`, set with `ctrl.SetLogger`, with
  its flags bound by `zap.Options.BindFlags`). It is an approved exception (see
  "Approved exceptions"). Also pass that logger to `klog.SetLogger`, so that
  client-go logs use the same output and verbosity. Never import
  `go.uber.org/zap` packages directly.
- Domain code never logs. It returns its decisions, and the reconciler or other
  boundary code logs them, so domain functions stay pure.
- In reconcilers, log through the logger in the reconcile `ctx`. It already
  carries controller-runtime's `controller`, `name`, and `reconcileID` keys, so
  do not add them again.
- Do not log an error that you also return: controller-runtime already logs every
  error returned by `Reconcile` ("Reconciler error"). Log only errors that are
  handled locally.
- Messages are constant strings. Put variable data in key/value pairs with
  lowerCamelCase keys, as Kubernetes structured logging does. Use the same key for
  the same concept everywhere (for example `node`, `conditionType`).
- Log object names and UIDs, never whole objects, annotation values, credentials,
  or tokens.

Verbosity levels:

| Level | Content |
|---|---|
| `Error` and `V(0)` | Errors handled locally. Decisions: maintenance started, node released and why, window `Invalid`, start rejected by a write conflict, leadership changes, startup sweep milestones. |
| `V(1)` | Reconcile summaries: window state, node counts, next requeue. |
| `V(2)` | Per-node trigger results. |
| `V(4)` | Details of API writes (condition types and statuses sent, preconditions). |

`mise run lint` runs Kubernetes' `logcheck` (`sigs.k8s.io/logtools`, installed
through mise) to check logging calls, including their key/value pairs, which
`go vet` does not check for logr.

## Dependencies and versions

### Allowed dependencies

Direct Go modules and imports are limited to:

- This repository's own packages.
- The Go standard library.
- Modules and packages hosted at `k8s.io` or any of its subdomains, including
  `sigs.k8s.io/controller-runtime`.
- Explicitly approved, documented exceptions.

This applies to production code, tests, tools, and generated code, including
supported build-tag variants. Match the host, not a substring: `sigs.k8s.io` is
allowed; `k8s.io.example.org` is not. Do not vendor, copy, or shell out to a library
to bypass this policy.

Necessary transitive dependencies are permitted. Review changes to the module
graph and `go.sum`; do not import a forbidden package directly merely because it
is already present transitively.

Implementation must provide a required dependency-policy lint gate covering both
direct modules and imports. Test the lint rule against the allowed root domain
and subdomains, lookalike hosts, and forbidden dependencies, including a forbidden
direct import of a transitive dependency. It must reject unapproved dependencies
and overdue exception reviews.

### Exceptions

Obtain maintainer approval before introducing an exception. Keep approved
exceptions in a machine-readable allowlist consumed by the lint gate, recording:

- Exact module/import scope and permitted use (production, tests, or tools).
- Why the standard library and allowed Kubernetes modules are insufficient.
- Approval reference, review date, and next review due date.

Review each exception at least annually; its next review is due no later than one
year after approval or the previous review. Remove exceptions when no longer needed.
A package on an allowed host that wraps a non-allowed module also needs an
exception, and the lint gate must reject it without one. The lint gate keeps a
list of such packages, starting with `sigs.k8s.io/controller-runtime/pkg/log/zap`
and `k8s.io/apiserver/pkg/cel/...`.

#### Approved exceptions

Until the machine-readable allowlist exists, approved exceptions are recorded
here. Move them into the allowlist when the lint gate lands.

- **`sigs.k8s.io/controller-runtime/pkg/log/zap`**, which links
  `go.uber.org/zap` and `go.uber.org/zap/zapcore`.
  - Scope: production (the manager entry point) and tests may import
    `sigs.k8s.io/controller-runtime/pkg/log/zap`. Direct imports of
    `go.uber.org/zap` packages stay forbidden.
  - Reason: it is the logging backend that Kubebuilder scaffolds and
    controller-runtime documents. A standard-library `log/slog` backend would
    work, but keeping the default keeps the generated code standard, which makes
    long-term maintenance and scaffold upgrades simpler.
  - Approval: approved by maintainer evrardjp on 2026-11-17; the reference is the
    review of the change that adds this entry. Reviewed 2026-11-17, next review due
    2027-11-17.
- **`github.com/google/cel-go`**, packages `cel` and `common/types`, and the
  Kubernetes packages that wrap it, `k8s.io/apiserver/pkg/cel/...`.
  - Scope: production and tests, to compile and evaluate the CEL triggers
    described in `README.md` ("CEL triggers"). Other `cel-go` packages stay
    forbidden.
  - Reason: `k8s.io/apiserver/pkg/cel/environment` provides Kubernetes' CEL
    environment and libraries, so triggers behave like Kubernetes' own CEL
    features, but declaring the `node` variable and setting the cost limit need
    `cel-go`'s API (`cel.EnvOption`, program options, result values). The
    standard library has no CEL implementation.
  - Approval: approved by maintainer evrardjp on 2026-11-17; the reference is the
    review of the change that adds this entry. Reviewed 2026-11-17, next review due
    2027-11-17.

### Version selection

Target the latest released Go, Kubernetes, and README integration components at
initial version selection; verify what is available rather than guessing versions.
Kured and its reporters will evolve for this design. Where required upstream
support is unreleased, agree on and pin the compatible upstream commits rather
than weakening the README contract.

Pin exact tool, module, and image versions in the appropriate project files so
runs are reproducible; do not resolve floating `latest` versions during tests.
Future automation may bump the toolchain and integrations, but must update related
pins coherently, review graph changes, and pass the same compatibility gates.

## Tests and required checks

Tests enforce observable behaviour, not incidental implementation details. Each
implemented README contract needs coverage; a changed contract needs approved
specification and test changes together. Use Go's `testing` package, not an
additional testing or assertion framework.

Link each test to the README sections it enforces with a comment naming the
section's heading anchor, for example `// README: #heartbeat-contract`; a test may
name several sections. `mise run lint:contracts` (standard library only) checks
these links:

- It always fails on a link to a section that does not exist, which catches
  renamed or removed sections.
- It fails on a README section that no test links to, unless the section is
  listed, with a reason, as `pending` (its behaviour is not implemented yet) or
  `informative` (it describes no testable behaviour). The list lives next to the
  task.
- It fails on a `pending` section that tests already link to, so the list stays
  current.

Use the lowest test level that can prove a behaviour:

- Unit tests for pure domain decisions, with explicit inputs and no API server.
- envtest (`sigs.k8s.io/controller-runtime/pkg/envtest`, a real kube-apiserver and
  etcd) for behaviour that depends on API server semantics and that fakes do not
  model faithfully: server-side apply ownership and merging, `resourceVersion`
  preconditions and conflicts, CRD schema and CEL validation, status subresource
  writes, and RBAC denials. Install and pin the envtest control-plane binaries
  through mise.
- Kind-based e2e for real-component integration (see "E2e").

Keep determinism simple:

- Pass an explicit time value into time-dependent decision logic where needed;
  do not invent a general clock or scheduler framework without a demonstrated need.
- In e2e tests, observe conditions with watches or bounded polling and deadlines,
  not arbitrary sleeps that assume convergence has finished.
- Bound waits and external commands; include useful diagnostics on timeout.

Run the relevant `mise run` tasks locally before proposing an implementation
change; do not rely on GitHub Actions to discover whether it works. Before merge,
require clean formatting, `go vet`, unit tests, race tests, dependency-policy lint,
cognitive-complexity lint, `lint:contracts`, security/license checks (Snyk is
CI-only, see "Security and license scanning"), `generate:check`, and
compatibility/e2e suites.
Agree on the complexity lint tool and threshold before introducing it; keep its
configuration identical locally and in CI.

Add focused fuzz tests for parsers and scheduling inputs. Benchmarks and scale
tests are required for performance-sensitive changes.

Required checks must not silently skip missing infrastructure or components. A
missing prerequisite cannot produce a successful compatibility gate. Do not merge
an implementation change with required gates unavailable or failing.

## Tooling, CI, and releases

### Local-first tooling

- Use `mise` for development tool/runtime installation and version management,
  pinned in `mise.toml` and the associated lockfile where applicable. Go module
  dependencies remain managed by Go modules and the policy above.
- Use `mise run <task>` as the entry point for building, generation, formatting,
  linting, tests, scans, release previews, and dependency-update validation. Do not
  introduce Makefiles or a parallel task system. Replace Kubebuilder-generated
  Makefile recipes with mise tasks rather than requiring Make behind the scenes.
- Keep checks runnable locally with the same inputs, configuration, and failure
  criteria as CI. Document prerequisites and required network access/credentials;
  locally runnable does not necessarily mean offline.
- These requested CLI tools are development infrastructure, not blanket exceptions
  for importing their libraries into the Go module.
- Install every development binary through mise, including linters, code
  generators (`controller-gen`), envtest binaries, `kind`, `kubectl`, `helm`,
  scanners, and release tools. Prefer prebuilt binaries from the project's own
  release artifacts, verified by checksums in the mise lockfile. Build from source
  with mise's `go:` backend, pinned to an exact version, only when no such binary
  exists. Do not install tools with ad-hoc `go install`, `curl | sh`, `tools.go`,
  or the `tool` directive in `go.mod`.

#### Standard tasks

These task names are fixed, so contributors and CI converge on the same entry
points. Add each task when its tooling lands; this list does not mean that they
already exist.

| Task | Purpose |
|---|---|
| `fmt` | Format the code in place. |
| `lint` | Check formatting and run `go vet`, `logcheck`, and the other linters. |
| `lint:deps` | Dependency-policy gate (modules, imports, exception reviews). |
| `lint:complexity` | Cognitive-complexity gate. |
| `lint:contracts` | Links between README sections and tests (see "Tests and required checks"). |
| `generate` | Regenerate all generated code and manifests (`controller-gen` output: deepcopy, CRDs, RBAC). |
| `generate:check` | Run `generate` and fail if the working tree changed. |
| `test` | Unit tests. |
| `test:race` | Unit tests with the race detector. |
| `test:fuzz` | Fuzz tests, with a bounded fuzz time. |
| `test:envtest` | envtest suite. |
| `e2e` | Kind e2e suite (see "E2e"). |
| `scan:trivy` | Trivy repository, configuration, and image scans. |
| `scan:snyk` | Snyk dependency and license scan (CI only, see below). |
| `release:check` | Validate the GoReleaser configuration. |
| `release:snapshot` | Non-publishing snapshot release. |

Generated code is committed. Run `mise run generate` in every commit that
changes its inputs (API types, markers, configuration). `generate:check` is a
required check, locally and in CI. Contributors can run it before every commit
with the hook installed by `mise generate git-pre-commit --write --task
generate:check`.

### Controller development

Use Kubebuilder to scaffold and develop the controller, keeping reconciliation
and Kubernetes plumbing separate from pure domain decisions. Inspect generated
code and dependencies; do not accept unnecessary scaffold features or its default
test-framework dependencies instead of Go's `testing` package.

Kubebuilder's `sigs.k8s.io/controller-runtime` dependency is allowed by the
Kubernetes-domain policy; no exception is needed. Review all other direct modules
and imports introduced by generated code or tooling against the same rules.

### Security and license scanning

- Use Trivy for repository/configuration and container-image security scanning.
  Provide local mise tasks, and scan release candidate images before publication.
- Use Snyk for dependency security and license scanning. It is an approved
  exception to local-first checks: it needs a Snyk account token, so it runs only
  in CI. Contributors are not required to run it locally. The `mise run
  scan:snyk` task stays the single implementation and fails, rather than skips,
  without `SNYK_TOKEN`. CI runs it only in trusted contexts (pushes to `main`,
  scheduled runs, and release tags), never with the token on untrusted fork code.
  Pull requests from forks therefore get no Snyk result before merge. A Snyk
  failure on `main` blocks the next release until it is resolved or a documented
  exception is approved.
- GitHub Actions must invoke these same scan tasks, not provide the only scanning
  implementation. Missing credentials or scan failures must not masquerade as
  passing required checks. Agree on finding thresholds and documented exceptions
  before enforcing scan policy.

### GitHub Actions and maintenance automation

- Use GitHub Actions to run the same `mise run` tasks contributors run locally.
  Workflows may handle runner setup, caching, authentication, and artifacts; do not
  duplicate build/test/scan logic in workflow-only commands.
- Use [cncf/prow-github-actions](https://github.com/cncf/prow-github-actions) for
  issue/PR workflow automation. Configure only the needed functionality; it must
  not bypass required checks or approvals. Do not enable automatic merging without
  maintainer approval.
- Pin third-party actions to immutable commit SHAs. Keep token permissions minimal
  and never execute untrusted PR code with privileged tokens or release secrets.
- Use Updatecli to propose updates to GitHub Actions (including SHA pins), container
  images, `mise.toml`, Go modules, and the Go toolchain (the `go` and `toolchain`
  directives in `go.mod` together with the Go version in `mise.toml`). Move
  coupled modules together, for example `k8s.io/*` with
  `sigs.k8s.io/controller-runtime`. Kind images bumped with kind binary.
  Update related lockfiles/pins coherently and run the same validation gates.
  Keep update PRs atomic; automation is not a review exemption.

### Releases and images

Use GoReleaser for releases, informed by the
[Kured release configuration](https://github.com/kubereboot/kured/blob/main/.config/goreleaser.yaml)
and its [tag workflow](https://github.com/kubereboot/kured/blob/main/.github/workflows/on-tag.yaml).
Adopt the relevant build, scan, and publication flow, not unrelated artifacts,
platform targets, dependencies, or Makefile conventions.

Provide locally runnable mise tasks for configuration validation and non-publishing
snapshot/release previews. Publish container images to GitHub Container Registry
(`ghcr.io`) through the release process, after the required checks and scans pass.
Keep credentialed publication restricted to explicitly authorized release runs;
local tests and previews must not publish artifacts.

Build images `FROM scratch` with a static (`CGO_ENABLED=0`) binary, running as a
numeric non-root user. Anything the binary needs at runtime must then be
embedded or mounted, not taken from a base image (time zone data comes from the
embedded `time/tzdata` package, see `README.md`, "Time zones"). A base image
other than `scratch` needs maintainer approval and a documented reason.

Ship the deployment as a Helm chart and as kustomize manifests. Both deploy
kumaco into the `kube-system` namespace by default. Generate both from the same
`controller-gen` CRD and RBAC output so they cannot drift, and validate both in
the required checks. Both must support the per-team setups in
`README.md`, including RBAC restricted to named nodes. Pkl and CUE packaging may
follow later; do not add them before they are agreed.

#### Versioning and release policy

- Release tags follow SemVer (`vX.Y.Z`, release candidates `vX.Y.Z-rc.N`). They are
  cut from `main`, only on a commit where every required check passed (Snyk
  included), and the tags are signed.
- Before v1.0.0, a minor release may contain breaking changes to metrics, the CRD
  API, and exported Go symbols. Patch releases contain fixes only, never a
  contract change.
- Mark a breaking change with `!` in its commit title (`feat!: ...`). Release notes
  group changes by Conventional Commits type and list breaking changes first,
  with migration notes. They also state the Go version the release is built with,
  which determines its embedded time zone database.
- Only the latest release receives fixes; nothing is backported before v1.0.0.
  Each release supports the latest Kubernetes minor release only, as `README.md`
  ("Compatibility") documents.
- Publish multi-arch images for `linux/amd64` and `linux/arm64`, tagged with the
  version and the short commit (never a floating `latest` tag). Publish them with
  SBOMs, build provenance, and keyless cosign signatures, as Kured does. Attach
  the checksums, CRD, Helm chart, and kustomize manifests to the GitHub release.

## E2e: provision everything, never break Kured

### Environment and lifecycle

- Use Go tests with approved Go dependencies. CLIs such as `kind`, `kubectl`, and
  `helm`, their runtime prerequisites, and deployed integration components are
  allowed; they do not grant exceptions to the Go dependency policy.
- Run against an explicitly supplied, disposable kind cluster. Require an explicit
  target and verify it; never fall back to the developer's current kubeconfig
  context. Document prerequisites and make the setup reproducible for open-source
  contributors without private infrastructure.
- Use the latest kind release and its default node image, which provides the only
  supported Kubernetes minor release. Pin kind in `mise.toml` and the node image
  by digest; Updatecli bumps both together. There is no Kubernetes version matrix.
- Provision all required workloads and configuration into that cluster, including
  kumaco and the real integration components. Do not assume a preinstalled release
  or manually prepared application state.
- Tear down the designated disposable test cluster by default, on success or
  failure. Provide an explicit keep option that preserves it and its workloads for
  debugging. Make this deletion policy clear before starting, and never delete a
  cluster not explicitly designated for the run.
- Capture failure diagnostics before teardown. Isolate test state and clean up so
  reruns do not depend on previous runs.
- Exercise actual Kured cordon/drain/reboot/uncordon cycles, including initially
  cordoned nodes. A reboot must genuinely restart the disposable kind worker and
  recover it; a no-op reboot command does not count. Never reboot the host. If the
  upstream reboot path cannot safely do this in kind, stop and resolve the setup
  with the maintainer rather than silently substituting a simulation.

### Compatibility and safety coverage

Maintain fast contract tests with controlled inputs **and** real-component e2e
coverage. Fakes and manually patched conditions alone do not prove integration.
Cover the README behaviour with:

- Kured, `node-reboot-required-reporter`, and `reboot-inhibitors-reporter`.
  Required.
- Node readiness controller, including readiness-dependent release after reboot.
- Node problem detector, including condition publication and coexistence with
  kumaco-owned conditions.

The node readiness controller and node problem detector integrations are not
implemented in e2e yet (`README.md` says so). Until they are, they are not merge
gates, and their absence must not be reported as a passing check. Once a
scenario for either one lands, it becomes required like the Kured suite, and
`README.md` drops its "not in the e2e test yet" note in the same change.

Kured compatibility is a required merge gate. Do not narrow the suite, skip a
scenario, or relax expectations to accommodate an implementation regression.
In particular, enforce these README safety contracts across appropriate test levels:

- Maintenance starts only when the schedule, selector, start/stop triggers, and
  applicable concurrency permit it. CEL failures fail closed as specified.
- Windows of the same kind share records and count the same selected nodes;
  different kinds remain distinct. Lowering concurrency does not evict work.
- Consumers act on their kind's condition, not generic `MaintenanceInProgress`.
  Missing/False/Unknown inhibitor reports do not authorize Kured's example flow;
  an inhibitor appearing after drain starts does not prematurely release it.
- Rebooting or not-yet-ready nodes retain their slots until the documented release
  conditions hold. Preserve an initially cordoned node's cordon state.
- Preserve other writers' conditions, enforce the reserved prefix and node scope,
  and test least-privilege access and separation between teams.
- Test cache lag, write conflicts, API failures, restart, leader handover, startup
  cleanup, and stale heartbeats. Recompute rejected starts from fresh input rather
  than replaying stale decisions. Do not refresh stale authorization on startup
  before the required window reconciliations.
- Cover schedule boundaries, timezone/DST behaviour, overlapping occurrences,
  invalid or deleted windows, edited selectors/types, and node deletion/recreation.

## Scale and evolution

Design and validate for clusters with **10,000+ nodes**. Use representative cached
node sets in repeatable benchmarks and scale tests; this does not require creating
10,000 real kind nodes. Agree on concrete latency/memory budgets when measurements
are available rather than inventing unsupported performance claims.

Follow the README's cache-based reconciliation, targeted enqueueing, bounded
reporting, and no-op-write rules. Avoid unnecessary full scans, API lists during
reconciliation, API writes, event storms, and metric cardinality beyond the
specified contract. Measure before adding optimization machinery. Validate the
default API request rate (`--kube-api-qps`, `--kube-api-burst`) and the heartbeat
sweep pacing documented in `README.md` with the scale benchmark.

Keep contracts and package responsibilities clear so changes remain local. Do not
prebuild extension points for hypothetical future requirements.

## Commit messages

Each commit must be atomic: it does one thing, and only one. Include that outcome's
necessary tests and documentation, and leave applicable checks passing. Split
independent outcomes into separate commits. The TDD red phase is demonstrated
locally, not left as a broken intermediate commit.

Use a Conventional Commits title describing what the change does, without a
scope: `type: description`, or `type!: description` for a breaking change. Keep
the entire title to at most 50 characters. Example:

```text
fix: retain live target metadata in manifests
```

Separate the title from the body with a blank line. The body is the most important
part; use these paragraph openings in order:

1. `Without this patch,` explain the behaviour or missing capability without the
   change.
2. `This is a problem because` explain its concrete consequences and why they
   matter.
3. `This fixes it by` explain the chosen approach, why it solves the problem, and
   why it was chosen over the alternatives considered and discarded.

Explain the motivation and design trade-offs, not a narration of the diff. Only
explain implementation details when the complexity genuinely requires it.

Aim to wrap body lines at no more than 83 characters. Links and the sign-off line
may exceed that limit, but put them on separate lines at the end. Refer to links
in the body as `[0]`, `[1]`, etc., and place their definitions before the final
sign-off, for example `[0]: https://example.org/reference`.

Finish every commit message with `Signed-off-by: Full Name <email>` (as
`git commit -s` writes it), using the contributor's actual identity.

The sign-off is a human's Developer Certificate of Origin attestation. AI-generated
changes need a human sign-off: the human who reviewed the change and takes
responsibility for it. An agent never signs off in its own name. It adds a human's
`Signed-off-by` only when that human has reviewed the change and asked for the
commit.
