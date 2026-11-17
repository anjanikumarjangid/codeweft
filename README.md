# codewiki

**A local MCP server that gives AI coding agents structured, deterministic context about a codebase.**

When an agent like Claude Code needs to know *where rate limiting is implemented* or *what `ProcessPayment` looks like*, it usually greps, opens a dozen files and guesses. `codewiki` parses the code once into an index of functions, methods and types, then answers those questions directly over the [Model Context Protocol](https://modelcontextprotocol.io/).

- **Deterministic:** answers come from the parsed source, not from a model's guess.
- **Local:** one binary and one SQLite file. No cloud service, no API keys, no embeddings.
- **Agent-native:** exposed as MCP tools, so any MCP client can use it.

> Status: v0.2. Go repositories are supported today; see the [roadmap](#roadmap).

## MCP tools

| Tool | What it answers |
|---|---|
| `search_code` | Ranked keyword search over symbol names, signatures and doc comments. Splits `camelCase` and `snake_case`, stems words and matches prefixes, so `payment` finds `ProcessPayment`. Optional `kind` filter. |
| `get_symbol` | Signature, doc comment, `file:line` range and source of a function, method or type, by qualified name (`internal/store.Store.Search`) or short name (`Search`). |

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
codewiki serve  [--db FILE]
```

## How it works

```
Go source ──▶ go/ast parser ──▶ symbols (name, kind, signature, doc, lines, source)
                                        │
                                        ▼
                         SQLite: symbols table + FTS5 index
                                        │
Claude Code ◀── MCP over stdio ◀── codewiki serve
```

1. **Index:** walks the repo, skips `vendor/`, `testdata/`, hidden directories, generated files (`// Code generated ... DO NOT EDIT.`) and tests by default, and parses each file with the standard library `go/ast`.
2. **Store:** writes symbols to SQLite in one transaction. A contentless FTS5 table indexes names, split name words, signatures and docs, ranked with BM25 and weighted towards names.
3. **Serve:** exposes the index through the official [MCP Go SDK](https://github.com/modelcontextprotocol/go-sdk) over stdio.

Search input is always tokenised and quoted before it reaches FTS5, so user text can never inject query syntax. A query first requires every word to match, then falls back to matching any word.

## Design decisions

| Decision | Choice | Why |
|---|---|---|
| Parser | `go/ast` | Exact for Go, no external grammar. Other languages will use tree-sitter behind the same symbol model. |
| Storage | SQLite + FTS5 | Zero config, a single portable file, fast ranked search. |
| Search | Keyword + BM25, no embeddings | Deterministic, explainable results with no model or network dependency. |
| Transport | stdio | What local MCP clients like Claude Code use. |

## Benchmarks

Measured on a 2-vCPU Linux cloud VM:

| Repository | Files | Symbols | Full index | Query (whole process) |
|---|---|---|---|---|
| MCP Go SDK v1.1.0 | 60 | 896 | 39 ms | < 10 ms |
| Go standard library `src/` (Go 1.24) | 3,285 | 40,746 | 2.35 s | 6–7 ms |

## Development

```bash
make test   # unit, golden-file and MCP protocol tests
make vet
```

- `internal/indexer`: golden-file test over a fixture repo in `testdata/sample`. After an intended change, run `go test -tags sqlite_fts5 ./internal/indexer -update` and review the diff.
- `internal/server`: drives the real MCP protocol through an in-memory client session.

## Roadmap

- [x] **v0.1:** Go indexer, SQLite + FTS5 store, `index` / `query` / `symbol` CLI
- [x] **v0.2:** MCP server over stdio with `search_code` and `get_symbol`
- [ ] **v0.3:** call graph: `get_dependency_graph`, callers and callees in `get_symbol`
- [ ] **v0.4:** `get_file_summary`, incremental re-index using file hashes
- [ ] **v0.5:** Java and Python via tree-sitter
- [ ] Later: watch mode, architecture overview, REST endpoint listing, multi-repo indexes

## License

MIT
