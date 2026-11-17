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
| `get_symbol` | Signature, doc comment, `file:line` range, source, direct callers and callees of a function, method or type, by qualified name (`internal/store.Store.Search`) or short name (`Search`). |
| `get_dependency_graph` | What a symbol calls, or what calls it, several levels deep. Given an external package such as `database/sql`, lists every place that calls into it. |
| `get_file_summary` | A file at a glance: package doc, imports, every declaration with its signature, and which files it calls into or is called from. |
| `get_architecture_overview` | The repository at a glance: languages, packages and what they are for, entry points, HTTP frameworks, package-to-package dependencies, most-called functions and most-used external libraries. |
| `list_apis` | Every HTTP endpoint with method, route and handler: Go `net/http`, gin, echo, chi and fiber; Spring and JAX-RS; Flask and FastAPI, including router, group and class-level prefixes. |

## Quick start

Requires Go 1.24+ and a C compiler (the SQLite driver uses cgo).

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

```bash
claude mcp add codewiki -- /abs/path/to/codewiki/bin/codewiki serve --db /abs/path/to/repo/.codewiki/index.db
```

Add `--watch` after `serve` to keep the index up to date as you edit. Then ask something like: *"Using codewiki, where is the retry logic for outgoing webhooks?"* or *"What are the HTTP endpoints of this service and which ones write to the database?"*

### CLI

```
codewiki index  [--db FILE] [--include-tests] [--exclude dir1,dir2] PATH
codewiki query  [--db FILE] [--kind KIND] [--limit N] [--json] TEXT...
codewiki symbol [--db FILE] [--json] NAME
codewiki reindex [--db FILE] [PATH]
codewiki deps   [--db FILE] [--callers] [--depth N] [--external] NAME
codewiki file   [--db FILE] [--json] PATH
codewiki apis   [--db FILE] [--method M] [--json] [ROUTE]
codewiki overview [--db FILE] [--json]
codewiki watch  [--db FILE] [--interval 2s] [PATH]
codewiki serve  [--db FILE] [--watch]
```

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

1. **Index:** walks the repo, skips `vendor/`, `testdata/`, hidden directories, generated files (`// Code generated ... DO NOT EDIT.`) and tests by default, and parses files in parallel: Go with the standard library `go/ast`, Java and Python with [tree-sitter](https://tree-sitter.github.io/). Every language produces the same model, so search and the call graph work identically across them.
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
| Package outside the repository | `sql.Open(...)` | external reference `database/sql.Open` |
| Method on a value of unknown type | `svc.ProcessPayment(...)` | edge only if exactly one method has that name, marked *matched by method name* |

Java and Python follow the same rules with their own syntax: `this.field` and `self.attr` with a known type, typed parameters and locals (`Order order = ...`, `repo: ItemRepository`), imports and static imports, relative Python imports, and constructor calls (`new Order(...)`, `Item(...)`). Java overloads share one node in the graph.

Anything ambiguous is dropped rather than guessed. Raw call sites are stored, so the graph can be rebuilt without re-parsing files.

### Incremental re-index

`codewiki reindex` hashes every file, re-parses only those whose content changed, removes deleted ones, and then rebuilds the call graph from the stored call sites. Edges *into* a changed file stay correct even though the files calling it were not re-parsed.

`codewiki watch` (or `serve --watch`) polls file sizes and modification times every couple of seconds and runs the same incremental re-index when something changes. Polling needs no platform-specific file events and stays cheap on large trees.

Search input is always tokenised and quoted before it reaches FTS5, so user text can never inject query syntax. A query first requires every word to match, then falls back to matching any word.

## Design decisions

| Decision | Choice | Why |
|---|---|---|
| Parser | `go/ast` for Go, tree-sitter for Java and Python | Exact for Go; one robust, error-tolerant parser for the rest, all behind the same symbol model. |
| Storage | SQLite + FTS5 | Zero config, a single portable file, fast ranked search. |
| Search | Keyword + BM25, no embeddings | Deterministic, explainable results with no model or network dependency. |
| Call graph | Syntactic resolution, no type checker | Works on any checkout without building it; precision is marked per edge. |
| Transport | stdio | What local MCP clients like Claude Code use. |

## Benchmarks

Measured on a 2-vCPU Linux cloud VM:

| Repository | Files | Symbols | Full index | Query (whole process) |
|---|---|---|---|---|
| MCP Go SDK v1.1.0 | 60 | 949 (669 call edges) | 0.11 s | < 10 ms |
| Go standard library `src/` (Go 1.24) | 3,260 | 41,150 (77,286 call edges) | 4.3 s | 6–7 ms |
| [Guava](https://github.com/google/guava) (Java, ~477k lines) | 2,156 | 36,632 (37,839 call edges) | 5.3 s | < 10 ms |
| [requests](https://github.com/psf/requests) (Python) | 22 | 306 (282 call edges) | 0.07 s | < 10 ms |
| [Spring PetClinic](https://github.com/spring-projects/spring-petclinic) (Java) | 30 | 118 (91 call edges, 17 endpoints) | 0.02 s | < 10 ms |

Re-indexing the standard library after changing one file takes 1.6 s (one file parsed, the call graph rebuilt).

## Development

```bash
make test   # unit, golden-file and MCP protocol tests
make vet
```

- `internal/indexer`: golden-file tests over a fixture repo in `testdata/sample`, for both the extracted symbols and the full call graph. After an intended change, run `go test -tags sqlite_fts5 ./internal/indexer -update` and review the diff.
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
