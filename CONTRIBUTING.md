# Contributing to codeweft

Thanks for helping. Bug reports, accuracy problems on real repositories, new language or framework support, and documentation fixes are all welcome.

## Before you start

- **Bugs and wrong answers:** open an issue with the repository (or a small reproduction), the tool call and what you expected. Wrong call-graph edges or missing service links are bugs too.
- **Larger changes** (a new language, a new kind of service link, a change to a tool's output): open an issue first to agree on the approach.
- **Security problems:** do not open a public issue; see [SECURITY.md](SECURITY.md).

## Development

Requires Go (see `go.mod`) and a C compiler: SQLite and tree-sitter are used through cgo.

```bash
make build   # bin/codeweft
make test    # unit, golden-file, MCP protocol and server tests
make vet
```

Before sending a pull request:

```bash
gofmt -l .                                  # must print nothing
make vet
go test -race -tags sqlite_fts5 ./...
```

## How the code is organized

| Package | What it does |
|---|---|
| `internal/indexer` | Parses Go (`go/ast`), Java, Python and `.proto` (tree-sitter) into symbols, call sites, routes and contracts, and resolves calls into the call graph. |
| `internal/store` | SQLite + FTS5. Bump `SchemaVersion` in `store.go` on any schema change: older indexes are then rebuilt. |
| `internal/links` | Matches contracts (HTTP, gRPC, Kafka, Temporal) into links between services. |
| `internal/server` | The MCP tools, through the official Go SDK. Tool outputs are plain structs (no recursive types) and never null arrays. |
| `internal/hosted` | `codeweft server`: Git sync, webhooks, HTTP transport, auth. |
| `cmd/codeweft` | The CLI. Flags may come before or after positional arguments. |

## Tests

- Every change comes with tests.
- `testdata/sample` is a small multi-language repository the tests index. Extend it rather than adding separate fixtures, then update the golden files with `go test -tags sqlite_fts5 ./internal/indexer -update` and **review the diff**: every added or removed edge should be one you meant.
- MCP tools are tested through an in-memory MCP client (`internal/server`), the hosted server end to end over HTTP with a local Git repository (`internal/hosted`).
- Validate resolution changes on a real repository as well. `scripts/accuracy` measures the call graph against each language's compiler or analyser, and `make bench` measures speed on a pinned corpus. Numbers in the README must be measured, and say on what.

## Pull requests

- One topic per pull request, with commits in the [Conventional Commits](https://www.conventionalcommits.org/) style (`feat:`, `fix:`, `perf:`, `docs:` ...) and a body explaining what changed and why.
- Update the README when behavior, flags or numbers change, and add a line to `CHANGELOG.md` under *Unreleased*.
- CI runs vet, the tests and a smoke test; it must pass.

By contributing you agree that your contributions are licensed under the [Apache License 2.0](LICENSE).
