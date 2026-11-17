# codewiki

**A local MCP server that gives AI coding agents structured, deterministic context about a codebase.**

When an agent like Claude Code needs to know *where rate limiting is implemented*, *what `ProcessPayment` looks like* or *what breaks if this function changes*, it usually greps, opens a dozen files and guesses. `codewiki` parses the code once into an index of functions, methods, types and the calls between them, then answers those questions directly over the [Model Context Protocol](https://modelcontextprotocol.io/).

- **Deterministic:** answers come from the parsed source, not from a model's guess.
- **Local:** one binary and one SQLite file. No cloud service, no API keys, no embeddings.
- **Agent-native:** exposed as MCP tools, so any MCP client can use it.

> Status: v0.6. Supports **Go, Java and Python**; see the [roadmap](#roadmap).

## MCP tools

| Tool | What it answers |
|---|---|
| `search_code` | Ranked keyword search over symbol names, signatures and doc comments. Splits `camelCase` and `snake_case`, stems words and matches prefixes, so `payment` finds `ProcessPayment`. Optional `kind`, `lang` and `path` filters. |
| `get_symbol` | Signature, doc comment, `file:line` range, source, direct callers and callees of a function, method or type, by qualified name (`internal/store.Store.Search`) or short name (`Search`). A short name that matches several symbols returns the candidates instead of the source of each. |
| `get_dependency_graph` | What a symbol calls, or what calls it, several levels deep. Given an external package such as `database/sql`, lists every place that calls into it. |
| `get_implementations` | What implements an interface, extends a class or overrides a method, or with `direction: up`, what a type or method implements and extends. Answers "where does this interface call actually go?": `Gateway.Charge` lists `StripeGateway.Charge`. Also lists repository types implementing an outside type such as `java.io.Serializable`. |
| `find_tests` | Which tests exercise a function, method or type: tests that call it, call something that calls it, or reach it through helpers in test files, nearest first, with the call each one goes through. `get_symbol` lists the nearest few inline. |
| `get_file_summary` | A file at a glance: package doc, imports, every declaration with its signature, and which files it calls into or is called from. |
| `get_architecture_overview` | The repository at a glance: languages, packages and what they are for, entry points, HTTP frameworks, package-to-package dependencies, most-called functions and most-used external libraries. |
| `list_apis` | Every HTTP endpoint with method, route and handler: Go `net/http`, gin, echo, chi and fiber; Spring and JAX-RS; Flask and FastAPI, including router, group and class-level prefixes. |

## Quick start

### Download

Each [release](https://github.com/anjanikumarjangid/codewiki/releases) has archives for Linux (amd64, arm64; statically linked, so they run on any distribution), macOS (Apple silicon, Intel) and Windows (amd64), and a `checksums.txt` with their SHA-256 sums. Unpack one and put `codewiki` on your `PATH`. macOS may refuse to run a binary downloaded with a browser until you remove the quarantine flag: `xattr -d com.apple.quarantine codewiki`.

### Build from source

Requires Go 1.24+ and a C compiler (SQLite and tree-sitter are C libraries, used through cgo).

```bash
git clone https://github.com/anjanikumarjangid/codewiki && cd codewiki
make build

# index a repository (writes PATH/.codewiki/index.db)
./bin/codewiki index /path/to/repo

# try it from the terminal
cd /path/to/repo
/path/to/codewiki/bin/codewiki query rate limiter
/path/to/codewiki/bin/codewiki symbol TokenBucket.Allow
```

### Use with Claude Code

From inside the repository you want to work on:

```bash
claude mcp add codewiki -- /abs/path/to/codewiki/bin/codewiki serve
```

No index needs to exist beforehand. `serve` finds the repository root (the nearest directory above the working directory with a `.codewiki/` index or `.git`) and builds the index in the background on first start. Until the first build is done, tools reply that the index is still being built instead of returning empty results. After that it catches up on edits made while it was not running and keeps the index current as files change; pass `--watch=false` to serve the index as it is. The index is written to `.codewiki/`, which gets its own `.gitignore` so it never shows up in `git status`.

For a client that does not start servers inside the project, pass `--root /abs/path/to/repo` (or `--db FILE` for an index stored elsewhere). Then ask something like: *"Using codewiki, where is the retry logic for outgoing webhooks?"* or *"What are the HTTP endpoints of this service and which ones write to the database?"*

### CLI

```
codewiki index  [--db FILE] [--include-tests] [--exclude dir1,dir2] PATH
codewiki query  [--db FILE] [--kind KIND] [--limit N] [--json] TEXT...
codewiki symbol [--db FILE] [--json] NAME
codewiki reindex [--db FILE] [PATH]
codewiki deps   [--db FILE] [--callers] [--depth N] [--external] NAME
codewiki impl   [--db FILE] [--up] [--depth N] [--json] NAME
codewiki tests  [--db FILE] [--depth N] [--json] NAME
codewiki file   [--db FILE] [--json] PATH
codewiki apis   [--db FILE] [--method M] [--json] [ROUTE]
codewiki overview [--db FILE] [--json]
codewiki watch  [--db FILE] [--interval 2s] [PATH]
codewiki serve  [--db FILE] [--root PATH] [--watch=false]
```

`reindex`, `watch` and `serve` reuse the `--include-tests` and `--exclude` settings the index was built with; flags passed to `reindex` override them.

## How it works

```
Go ──▶ go/ast ─────┐
Java ─▶ tree-sitter ┼─▶ symbols, imports, fields, raw call sites
Python ▶ tree-sitter┘
                                        │
                                        ▼
                    SQLite: symbols + FTS5 index + raw calls
                                        │
                                  call resolver ──▶ edges
                                        │
Claude Code ◀── MCP over stdio ◀── codewiki serve
```

1. **Index:** walks the repo, skips `vendor/`, `testdata/`, hidden directories and generated files (`// Code generated ... DO NOT EDIT.`), and parses files in parallel: Go with the standard library `go/ast`, Java and Python with [tree-sitter](https://tree-sitter.github.io/). Every language produces the same model, so search and the call graph work identically across them.
2. **Store:** writes everything to SQLite in one transaction. A contentless FTS5 table indexes names, split name words, signatures and docs, ranked with BM25 and weighted towards names.
3. **Resolve:** turns raw call sites into a call graph (see below).
4. **Serve:** exposes the index through the official [MCP Go SDK](https://github.com/modelcontextprotocol/go-sdk) over stdio.

### Call resolution

Resolution is syntactic, so it works on any checkout, even one that does not build or whose dependencies are not downloaded. Each call site is resolved by the first rule that applies:

| Rule | Example | Result |
|---|---|---|
| Same package or method receiver | `newOrder(id)`, `s.validate()` | exact edge |
| Package import (inside the module, via `go.mod`) | `payments.NewPaymentService(gw)` | exact edge |
| Field of the receiver with a declared type | `s.gw.Charge(...)` with `gw Gateway` | exact edge to `Gateway.Charge` |
| Value whose type is stated in the code | parameter `d time.Duration`, `var c Client`, `x := &Cache{}` | exact or external edge |
| Python name re-exported by a package | `requests.get(...)` with `from .api import get` in `requests/__init__.py` | exact edge to `requests.api.get` |
| Package outside the repository | `sql.Open(...)` | external reference `database/sql.Open` |
| Method on a value of unknown type | `svc.ProcessPayment(...)` | edge only if exactly one method has that name, marked *matched by method name* |

Java and Python follow the same rules with their own syntax: `this.field` and `self.attr` with a known type, typed parameters and locals (`Order order = ...`, `repo: ItemRepository`), imports and static imports, relative Python imports, and constructor calls (`new Order(...)`, `Item(...)`). Java overloads share one node in the graph.

Anything ambiguous is dropped rather than guessed. Raw call sites are stored, so the graph can be rebuilt without re-parsing files.

### Type hierarchy

Java `extends`/`implements` and Python base classes are recorded as declared. Go has no `implements` clause, so a Go type implements an interface when its method set has every method of the interface with the same parameter and result types:

- the method set includes methods promoted from embedded types, following Go's rules: the shallowest embedding wins, and two at the same depth cancel out;
- type names in signatures are resolved through each file's package and imports, so `payments.Card` and `Card` inside package `payments` match while `fs.File` and `http.File` do not; `byte`/`uint8`, `rune`/`int32` and `interface{}`/`any` are treated as the same type, and type aliases declared in the repository are followed;
- an interface that embeds an interface from outside the repository has a method set the index cannot see in full, so nothing is said to implement it.

Measured against the Go type checker (`go/types`) on the Go 1.24 standard library, for the 263 non-generic interfaces and 2,921 non-generic concrete types it type-checks (see [Accuracy](#accuracy)):

| | |
|---|---|
| Precision | 99.97% (3,392 of 3,393 reported implementations are real) |
| Recall, where both types are indexed | 96.7% (3,392 of 3,509) |
| Recall, overall | 85.4% (3,392 of 3,970) |

The one false positive is a type declared differently per operating system: files are indexed regardless of build tags, so their methods are merged. Most missed implementations are methods in generated files (for example `stringer` output and the bundled HTTP/2 code), which are skipped by design. Generic types are not matched.

### Tests

Test files (`*_test.go`; `*Test.java`, `*Tests.java`, `*IT.java` and `src/test/`; `test_*.py`, `*_test.py` and `tests/`) are parsed too, but kept apart from the code: their functions are not searchable symbols, are not part of the call graph or the type hierarchy, and a test double's method can never make a call in the code ambiguous. What is kept is each test function and the calls it makes, resolved against the code and against helper functions in test files. `find_tests` then walks up from a symbol through its callers (3 calls by default) and through test helpers to the tests the runner calls: Go `Test`, `Benchmark`, `Fuzz` and `Example` functions, JUnit `@Test` methods (and JUnit 3 `test` methods), and Python `test_` functions and methods. Tests that reach code only through calls the index cannot resolve (an HTTP request through Spring's `MockMvc`, a function value) are not found.

`--include-tests` additionally indexes test code as ordinary code, so it shows up in search and the call graph.

Parsing tests costs index time. Best of three runs, measured side by side on the machine described under [Benchmarks](#benchmarks):

| Repository | Test files | Tests | Index without tests | With tests |
|---|---|---|---|---|
| Go standard library `src/` (Go 1.24) | 1,519 | 10,419 | 3.44 s | 5.17 s |
| Guava | 1,111 | 15,503 | 3.36 s | 6.00 s |
| requests | 15 | 347 | 0.06 s | 0.11 s |
| Spring PetClinic | 20 | 81 | 0.02 s | 0.05 s |

("Without tests" is the version before test files were parsed, which also skipped the standard library's 25 `go/build` and `cmd/dist` files.)

`find_tests` for the most-called functions answers in 0.07 s (`fmt.Sprintf`, 3,449 tests) and 0.13 s (Guava's `Preconditions.checkNotNull`, 4,570 tests).

### Incremental re-index

`codewiki reindex` hashes every file, re-parses only those whose content changed, removes deleted ones, and then rebuilds the call graph from the stored call sites. Edges *into* a changed file stay correct even though the files calling it were not re-parsed.

`codewiki watch` (and `serve`, unless started with `--watch=false`) first runs one incremental re-index to catch up on edits made while nothing was watching, then polls file sizes and modification times every couple of seconds and runs the same incremental re-index when something changes. The baseline for polling is taken before the catch-up run, so a file saved during it is not missed. Polling needs no platform-specific file events and stays cheap on large trees.

Search input is always tokenised and quoted before it reaches FTS5, so user text can never inject query syntax. A query first requires every word to match, then falls back to matching any word.

### Output size

Every tool result lands in the agent's context window, so results are capped and always say what was left out and how to get it: `get_symbol` shows the first 200 lines of source and the file range of the rest, and at most 15 callers and callees; an ambiguous name lists up to 30 candidates with the total count; `get_file_summary` lists the 20 most-connected related files in each direction. Every tool is annotated as read-only.

On the Go standard library, `get_symbol String` went from 12.1 KB of text (the full source of 20 of its matches) to 4.1 KB (a list of candidates), and the longest function (`cmd/internal/obj/arm64.ctxt7.asmout`, 2,457 lines) from 60 KB to 6.5 KB.

## Design decisions

| Decision | Choice | Why |
|---|---|---|
| Parser | `go/ast` for Go, tree-sitter for Java and Python | Exact for Go; one robust, error-tolerant parser for the rest, all behind the same symbol model. |
| Storage | SQLite + FTS5 | Zero config, a single portable file, fast ranked search. |
| Search | Keyword + BM25, no embeddings | Deterministic, explainable results with no model or network dependency. |
| Call graph | Syntactic resolution, no type checker | Works on any checkout without building it; precision is marked per edge. |
| Transport | stdio | What local MCP clients like Claude Code use. |

## Accuracy

The call graph and the Go type hierarchy are checked against the Go type checker on the Go 1.24.7 standard library: `scripts/accuracy` type-checks every package and compares the result with an index of `$(go env GOROOT)/src`.

| | Precision | Recall | Recall where both ends are indexed |
|---|---|---|---|
| Call graph (distinct caller → callee pairs) | 97.3% (33,667 of 34,595) | 75.4% (33,667 of 44,654) | 84.6% (33,667 of 39,787) |
| Go implementations | 99.97% (3,392 of 3,393) | 85.4% (3,392 of 3,970) | 96.7% (3,392 of 3,509) |

- The type checker sees one platform's files (linux/amd64); codewiki indexes every platform's, and a function declared once per operating system is one symbol. 891 of the 928 call edges counted as wrong are calls made by another platform's version of the caller, so they exist in the code; without them, call graph precision is 99.89%.
- Edges matched only by a unique method name (*matched by method name*) are as precise as the rest: 97.5% (1,795 of 1,841).
- Most missed edges are code in generated files, which is not indexed (4,867), and method calls on values whose type the syntax does not state, such as package-level variables, results of other calls and deeper field chains (4,827 calls to methods, 1,181 to interface methods).

To reproduce:

```bash
make build && ./bin/codewiki index --db /tmp/std.db "$(go env GOROOT)/src"
cd scripts/accuracy && mkdir -p /tmp/truth
go run ./calls /tmp/truth && go run ./implements /tmp/truth
python3 compare.py calls /tmp/std.db /tmp/truth
python3 compare.py implements /tmp/std.db /tmp/truth
```

## Benchmarks

Measured on a Linux cloud VM with 4 vCPUs (Intel Xeon @ 2.10 GHz) and 15 GB of RAM, Go 1.24.7. Times are the best of three runs; runs vary by about 10%. "Query" is a whole `codewiki query` process, start to exit.

| Repository | Files | Test files | Symbols | Call edges | Full index | Query |
|---|---|---|---|---|---|---|
| [MCP Go SDK](https://github.com/modelcontextprotocol/go-sdk) v1.1.0 | 60 | 42 | 949 | 669 | 0.10 s | 6 ms |
| Go standard library `src/` (Go 1.24.7) | 3,286 | 1,519 | 41,524 | 130,267 | 4.65 s | 8 ms |
| [Guava](https://github.com/google/guava) `fce80fd` (Java, 495k lines) | 2,156 | 1,111 | 36,632 | 37,839 | 5.97 s | 11 ms |
| [requests](https://github.com/psf/requests) `3c7baaa` (Python) | 22 | 15 | 306 | 282 | 0.11 s | 6 ms |
| [Spring PetClinic](https://github.com/spring-projects/spring-petclinic) `500158f` (Java, 17 endpoints) | 30 | 20 | 118 | 91 | 0.05 s | 7 ms |

Re-indexing the standard library after changing one file takes 2.1 s: one file is parsed, then the call graph, the type hierarchy and the test edges are rebuilt from stored data.

## Development

```bash
make test   # unit, golden-file and MCP protocol tests
make vet
```

Releases are built by `.github/workflows/release.yml` when a `v*` tag is pushed: one job per platform (Linux and Windows cross-compiled on Ubuntu, macOS on a macOS runner), a smoke test on the native ones, and a GitHub release with the archives and their checksums.

`scripts/accuracy` is a separate Go module, so its tools are not part of the build; see [Accuracy](#accuracy).

`scripts/eval/run.py` asks Claude Code the same questions about the Go standard library with and without codewiki (which functions call X, which types implement I), generated from the `go/types` ground truth, and scores each answer's precision and recall along with the tokens, turns and time it took. Running it spends Claude usage; `--dry-run` shows the questions without calling Claude. No results are published here yet.

- `internal/indexer`: golden-file tests over a fixture repo in `testdata/sample`, for the extracted symbols, the full call graph and the type hierarchy. After an intended change, run `go test -tags sqlite_fts5 ./internal/indexer -update` and review the diff.
- `internal/server`: drives the real MCP protocol through an in-memory client session.

## Roadmap

- [x] **v0.1:** Go indexer, SQLite + FTS5 store, `index` / `query` / `symbol` CLI
- [x] **v0.2:** MCP server over stdio with `search_code` and `get_symbol`
- [x] **v0.3:** call graph: `get_dependency_graph`, callers and callees in `get_symbol`
- [x] **v0.4:** `get_file_summary`, incremental re-index using file hashes
- [x] **v0.5:** Java and Python via tree-sitter
- [x] **v0.6:** `get_architecture_overview`, `list_apis`, watch mode
- [ ] Later: multi-repo indexes, TypeScript and Rust, optional semantic search with local embeddings

## License

MIT
