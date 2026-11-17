# codewiki

**A local MCP server that gives AI coding agents structured, deterministic context about a codebase.**

When an agent like Claude Code needs to know *where rate limiting is implemented*, *what `ProcessPayment` looks like* or *what breaks if this function changes*, it usually greps, opens a dozen files and guesses. `codewiki` parses the code once into an index of functions, methods, types and the calls between them, then answers those questions directly over the [Model Context Protocol](https://modelcontextprotocol.io/).

- **Deterministic:** answers come from the parsed source, not from a model's guess.
- **Local:** one binary and one SQLite file. No cloud service, no API keys, no embeddings.
- **Agent-native:** exposed as MCP tools, so any MCP client can use it.

> Status: v0.4. Go repositories are supported today; see the [roadmap](#roadmap).

## MCP tools

| Tool | What it answers |
|---|---|
| `search_code` | Ranked keyword search over symbol names, signatures and doc comments. Splits `camelCase` and `snake_case`, stems words and matches prefixes, so `payment` finds `ProcessPayment`. Optional `kind` filter. |
| `get_symbol` | Signature, doc comment, `file:line` range, source, direct callers and callees of a function, method or type, by qualified name (`internal/store.Store.Search`) or short name (`Search`). |
| `get_dependency_graph` | What a symbol calls, or what calls it, several levels deep. Given an external package such as `database/sql`, lists every place that calls into it. |
| `get_file_summary` | A file at a glance: package doc, imports, every declaration with its signature, and which files it calls into or is called from. |

## Quick start

Requires Go 1.24+ and a C compiler (the SQLite driver uses cgo).

```bash
git clone https://github.com/anjanikumarjangid/codewiki && cd codewiki
make build

# index a Go repository (writes PATH/.codewiki/index.db)
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

Then ask something like: *"Using codewiki, where is the retry logic for outgoing webhooks?"*

### CLI

```
codewiki index  [--db FILE] [--include-tests] [--exclude dir1,dir2] PATH
codewiki query  [--db FILE] [--kind KIND] [--limit N] [--json] TEXT...
codewiki symbol [--db FILE] [--json] NAME
codewiki reindex [--db FILE] [PATH]
codewiki deps   [--db FILE] [--callers] [--depth N] [--external] NAME
codewiki file   [--db FILE] [--json] PATH
codewiki serve  [--db FILE]
```

## How it works

```
Go source ──▶ go/ast parser ──▶ symbols, imports, struct fields, raw call sites
                                        │
                                        ▼
                    SQLite: symbols + FTS5 index + raw calls
                                        │
                                  call resolver ──▶ edges
                                        │
Claude Code ◀── MCP over stdio ◀── codewiki serve
```

1. **Index:** walks the repo, skips `vendor/`, `testdata/`, hidden directories, generated files (`// Code generated ... DO NOT EDIT.`) and tests by default, and parses files in parallel with the standard library `go/ast`.
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

Anything ambiguous is dropped rather than guessed. Raw call sites are stored, so the graph can be rebuilt without re-parsing files.

### Incremental re-index

`codewiki reindex` hashes every file, re-parses only those whose content changed, removes deleted ones, and then rebuilds the call graph from the stored call sites. Edges *into* a changed file stay correct even though the files calling it were not re-parsed.

Search input is always tokenised and quoted before it reaches FTS5, so user text can never inject query syntax. A query first requires every word to match, then falls back to matching any word.

## Design decisions

| Decision | Choice | Why |
|---|---|---|
| Parser | `go/ast` | Exact for Go, no external grammar. Other languages will use tree-sitter behind the same symbol model. |
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
- [ ] **v0.5:** Java and Python via tree-sitter
- [ ] Later: watch mode, architecture overview, REST endpoint listing, multi-repo indexes

## License

MIT
