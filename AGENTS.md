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
- Keep domain decisions testable without a running cluster. Status, metrics, and
  other derived views must not become alternative sources of truth.
- Prefer cohesive packages and explicit responsibilities. Introduce interfaces at
  real boundaries when needed, not one per type. Do not require repositories,
  factories, services, or a fixed directory hierarchy merely to claim DDD.
- Reuse existing code and standard-library facilities. Add an abstraction only
  when the current change demonstrates its need.

## Dependencies and versions

### Allowed dependencies

Direct Go modules and imports are limited to:

- This repository's own packages.
- The Go standard library.
- Modules and packages under `k8s.io/`.
- Explicitly approved, documented exceptions.

This applies to production code, tests, tools, and generated code, including
supported build-tag variants. `sigs.k8s.io/` and other namespaces are not implicitly
allowed. Do not vendor, copy, or shell out to a library to bypass this policy.

Necessary transitive dependencies are permitted. Review changes to the module
graph and `go.sum`; do not import a forbidden package directly merely because it
is already present transitively.

Implementation must provide a required dependency-policy lint gate covering both
direct modules and imports. Test the lint rule against allowed and forbidden
cases, including a forbidden direct import of a transitive dependency. It must
reject unapproved dependencies and overdue exception reviews.

### Exceptions

Obtain maintainer approval before introducing an exception. Keep approved
exceptions in a machine-readable allowlist consumed by the lint gate, recording:

- Exact module/import scope and permitted use (production, tests, or tools).
- Why standard-library and `k8s.io/` alternatives are insufficient.
- Approval reference, review date, and next review due date.

Review each exception at least annually; its next review is due no later than one
year after approval or the previous review. Remove exceptions when no longer needed.
CEL is an anticipated exception, not blanket permission for unspecified modules:
record and approve its concrete dependency scope before adding it.

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

Keep determinism simple:

- Pass an explicit time value into time-dependent decision logic where needed;
  do not invent a general clock or scheduler framework without a demonstrated need.
- In e2e tests, observe conditions with watches or bounded polling and deadlines,
  not arbitrary sleeps that assume convergence has finished.
- Bound waits and external commands; include useful diagnostics on timeout.

Before merging implementation changes, require clean formatting, `go vet`, unit
tests, race tests, dependency-policy lint, and the compatibility/e2e suites. Add
focused fuzz tests for parsers and scheduling inputs. Benchmarks and scale tests
are required for performance-sensitive changes. Document the actual commands as
the tooling is introduced; this file does not imply they already exist.

Required checks must not silently skip missing infrastructure or components. A
missing prerequisite cannot produce a successful compatibility gate. Do not merge
an implementation change with required gates unavailable or failing.

## E2e: provision everything, never break Kured

### Environment and lifecycle

- Use Go tests with approved Go dependencies. CLIs such as `kind`, `kubectl`, and
  `helm`, their runtime prerequisites, and deployed integration components are
  allowed; they do not grant exceptions to the Go dependency policy.
- Run against an explicitly supplied, disposable kind cluster. Require an explicit
  target and verify it; never fall back to the developer's current kubeconfig
  context. Document prerequisites and make the setup reproducible for open-source
  contributors without private infrastructure.
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
- Node readiness controller, including readiness-dependent release after reboot.
- Node problem detector, including condition publication and coexistence with
  kumaco-owned conditions.

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
specified contract. Measure before adding optimization machinery.

Keep contracts and package responsibilities clear so changes remain local. Do not
prebuild extension points for hypothetical future requirements.

## Commit messages

Each commit must be atomic: it does one thing, and only one. Include that outcome's
necessary tests and documentation, and leave applicable checks passing. Split
independent outcomes into separate commits. The TDD red phase is demonstrated
locally, not left as a broken intermediate commit.

Use a Conventional Commits title describing what the change does. Keep the entire
title, including its type and optional scope, to at most 50 characters. Example:

```text
fix: retain live target metadata (manifests)
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

Finish every commit message with `Signed-Off-By: Full Name <email>`, using the
contributor's actual identity.
