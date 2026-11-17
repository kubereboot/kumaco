# Testing kumaco

The rules are in [AGENTS.md](../AGENTS.md) ("Tests and required checks"). This page
says how to run the tests, and where a new test goes.

All tools are installed by [mise](https://mise.jdx.dev), at the versions pinned in
`mise.toml` and `mise.lock`: run `mise install` once.

| Task | What it runs | Needs |
|---|---|---|
| `mise run test` | Unit tests. | Nothing. |
| `mise run test:race` | Unit tests with the race detector. | A C compiler. |
| `mise run test:envtest` | Unit tests and the envtest suite. | Nothing: mise installs the control-plane binaries. |
| `mise run lint:contracts` | The links between `README.md` and the tests. | Nothing. |

A missing prerequisite fails a task. It never skips tests.

## Where does a test go?

Use the lowest level that can prove the behaviour:

1. **Unit tests** for domain decisions: explicit inputs, no API server. They are ordinary
   `_test.go` files, next to the code.
2. **envtest** for what depends on API server semantics (server-side apply, `resourceVersion`
   conflicts, CRD validation, status subresources, RBAC). Give the file the build tag
   `//go:build envtest`, and start the control plane in `TestMain` with `testenv.Start()`
   (see `test/testenv/apiserver_test.go`).

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
