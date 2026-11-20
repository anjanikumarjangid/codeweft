# codeweft

**A local MCP server that gives AI coding agents structured, deterministic context about a codebase.**

*Formerly codewiki.* In weaving, the weft is the thread that runs across all the others and binds them into one cloth; codeweft binds functions, files, repositories and the services calling each other into one map an agent can read.

When an agent like Claude Code needs to know *where rate limiting is implemented*, *what `ProcessPayment` looks like* or *what breaks if this function changes*, it usually greps, opens a dozen files and guesses. `codeweft` parses the code once into an index of functions, methods, types and the calls between them, then answers those questions directly over the [Model Context Protocol](https://modelcontextprotocol.io/).

- **Deterministic:** answers come from the parsed source, not from a model's guess.
- **Local:** one binary and one SQLite file. No cloud service, no API keys, no embeddings.
- **Agent-native:** exposed as MCP tools, so any MCP client can use it.

> Status: v0.6. Supports **Go, Java, Python, TypeScript and JavaScript**, and Protocol Buffers service definitions; see the [roadmap](#roadmap).

## MCP tools

| Tool | What it answers |
|---|---|
| `search_code` | Ranked keyword search over symbol names, signatures and doc comments. Splits `camelCase` and `snake_case`, stems words and matches prefixes, so `payment` finds `ProcessPayment`. Also finds the services, RPC methods and messages of `.proto` files. Optional `kind`, `lang` and `path` filters. |
| `get_symbol` | Signature, doc comment, `file:line` range, source, direct callers and callees of a function, method or type, by qualified name (`internal/store.Store.Search`) or short name (`Search`). A short name that matches several symbols returns the candidates instead of the source of each. |
| `get_dependency_graph` | What a symbol calls, or what calls it, several levels deep. Given an external package such as `database/sql`, lists every place that calls into it. |
| `get_implementations` | What implements an interface, extends a class or overrides a method, or with `direction: up`, what a type or method implements and extends. Answers "where does this interface call actually go?": `Gateway.Charge` lists `StripeGateway.Charge`. Also lists repository types implementing an outside type such as `java.io.Serializable`. |
| `find_tests` | Which tests exercise a function, method or type: tests that call it, call something that calls it, or reach it through helpers in test files, nearest first, with the call each one goes through. `get_symbol` lists the nearest few inline. |
| `get_file_summary` | A file at a glance: package doc, imports, every declaration with its signature, and which files it calls into or is called from. |
| `get_architecture_overview` | The repository at a glance: languages, packages and what they are for, entry points, HTTP frameworks, package-to-package dependencies, most-called functions and most-used external libraries. |
| `get_service_links` | How services call each other: each HTTP route with the client calls that reach it, each gRPC method with the stubs calling it, each Kafka topic's producers with its consumers, RabbitMQ, SQS and Pub/Sub messages from producer to consumer, and each Temporal workflow and activity with the code starting it and its task queue, across languages and across repositories in a [workspace](#several-repositories). Also lists calls whose provider is not indexed. |
| `list_apis` | Every HTTP endpoint with method, route and handler: Go `net/http`, gin, echo, chi and fiber; Spring and JAX-RS; Flask and FastAPI, including router, group and class-level prefixes. |

## Quick start

### Install

```bash
npx -y codeweft version                       # npm: runs the prebuilt binary for your platform
brew install anjanikumarjangid/codeweft/codeweft   # Homebrew (macOS, Linux)
docker pull ghcr.io/anjanikumarjangid/codeweft    # the self-hosted server
```

With npm there is nothing else to install: `claude mcp add codeweft -- npx -y codeweft serve` from inside a repository is enough.

### Download

Each [release](https://github.com/anjanikumarjangid/codeweft/releases) has archives for Linux (amd64, arm64; statically linked, so they run on any distribution), macOS (Apple silicon, Intel) and Windows (amd64), and a `checksums.txt` with their SHA-256 sums. Unpack one and put `codeweft` on your `PATH`. macOS may refuse to run a binary downloaded with a browser until you remove the quarantine flag: `xattr -d com.apple.quarantine codeweft`.

### Build from source

Requires Go 1.24+ and a C compiler (SQLite and tree-sitter are C libraries, used through cgo).

```bash
git clone https://github.com/anjanikumarjangid/codeweft && cd codeweft
make build

# index a repository (writes PATH/.codeweft/index.db)
./bin/codeweft index /path/to/repo

# try it from the terminal
cd /path/to/repo
/path/to/codeweft/bin/codeweft query rate limiter
/path/to/codeweft/bin/codeweft symbol TokenBucket.Allow
```

### Use with Claude Code

From inside the repository you want to work on:

```bash
claude mcp add codeweft -- /abs/path/to/codeweft/bin/codeweft serve
```

No index needs to exist beforehand. `serve` finds the repository root (the nearest directory above the working directory with a `.codeweft/` index or `.git`) and builds the index in the background on first start. Until the first build is done, tools reply that the index is still being built instead of returning empty results. After that it catches up on edits made while it was not running and keeps the index current as files change; pass `--watch=false` to serve the index as it is. The index is written to `.codeweft/`, which gets its own `.gitignore` so it never shows up in `git status`.

#### Other MCP clients

codeweft speaks standard MCP, so any client works. `serve` finds the repository from the directory the client starts it in; clients that start servers elsewhere need `--root`.

| Client | Configuration |
|---|---|
| Claude Code | `claude mcp add codeweft -- npx -y codeweft serve` (from the repository) |
| VS Code (`.vscode/mcp.json`) | `{"servers": {"codeweft": {"type": "stdio", "command": "npx", "args": ["-y", "codeweft", "serve", "--root", "${workspaceFolder}"]}}}` |
| Cursor (`.cursor/mcp.json`) | `{"mcpServers": {"codeweft": {"command": "npx", "args": ["-y", "codeweft", "serve", "--root", "/abs/path/to/repo"]}}}` |
| Claude Desktop (`claude_desktop_config.json`) | `{"mcpServers": {"codeweft": {"command": "npx", "args": ["-y", "codeweft", "serve", "--root", "/abs/path/to/repo"]}}}` |
| A [self-hosted server](#self-hosted-server) | the URL `https://<host>/mcp` with the header `Authorization: Bearer <token>` (Streamable HTTP) |

For a client that does not start servers inside the project, pass `--root /abs/path/to/repo` (or `--db FILE` for an index stored elsewhere). Then ask something like: *"Using codeweft, where is the retry logic for outgoing webhooks?"* or *"What are the HTTP endpoints of this service and which ones write to the database?"*

### Several repositories

Services that call each other usually live in separate repositories. A workspace file lists them, with paths relative to the file:

```json
{"repos": [{"name": "orders", "path": "../orders-service"}, {"path": "../web"}]}
```

```bash
claude mcp add codeweft -- /abs/path/to/codeweft/bin/codeweft serve --workspace /abs/path/to/workspace.json
```

Each repository keeps its own index at `PATH/.codeweft/index.db`, built and watched as with one repository. Every tool then answers across all of them: results are labelled with their repository (`[orders] main.getOrder`), a name found in two repositories lists both, and every tool takes an optional `repo` argument to look in one. `get_service_links` links a client call in one repository to the route serving it in another. A repository's name defaults to its directory name. `codeweft links --workspace FILE` prints the same links from the terminal.

### Self-hosted server

`codeweft server` runs codeweft as a shared service for a team or an organization. It clones your repositories, keeps their indexes current from GitHub push webhooks (and a periodic sync), and serves all of them to every developer's MCP client over [Streamable HTTP](https://modelcontextprotocol.io/specification/2025-06-18/basic/transports). Nobody needs to index anything locally.

```bash
export CODEWEFT_REPOS="acme/orders acme/payments acme/web"   # or CODEWEFT_GITHUB_ORG=acme
export GITHUB_TOKEN=...              # clones private repositories
export GITHUB_WEBHOOK_SECRET=...     # enables /webhooks/github
export CODEWEFT_API_TOKENS=...       # what clients send as their bearer token
codeweft server --addr :8080 --data /var/lib/codeweft
```

Or run the container image (Linux, amd64 and arm64), which has everything it needs:

```bash
docker run -d -p 8080:8080 -v codeweft-data:/data \
  -e CODEWEFT_REPOS="acme/orders acme/payments" -e GITHUB_TOKEN -e GITHUB_WEBHOOK_SECRET -e CODEWEFT_API_TOKENS \
  ghcr.io/anjanikumarjangid/codeweft:latest
```

[`deploy/docker-compose.yml`](deploy/docker-compose.yml) runs it with Docker Compose, and [`deploy/helm/codeweft`](deploy/helm/codeweft) on Kubernetes (`helm install codeweft deploy/helm/codeweft --set repos="acme/orders acme/payments" --set existingSecret=codeweft`, with the secrets in that Secret). Put it behind TLS before exposing it.

Then, in each developer's `.mcp.json` (Claude Code; other clients take the same URL and header):

```json
{
  "mcpServers": {
    "codeweft": {
      "type": "http",
      "url": "https://codeweft.example.com/mcp",
      "headers": { "Authorization": "Bearer ${CODEWEFT_TOKEN}" }
    }
  }
}
```

In each repository's GitHub settings (or once for the organization), add a webhook to `https://codeweft.example.com/webhooks/github` with content type `application/json`, the same secret, and the *push* event. A push to a repository's default branch (a merged pull request, whatever the merge method) brings its clone to the new commit and re-indexes it: only the files the push changed are parsed, and when they keep their declarations only their own calls are resolved again, which takes about a second even on Kubernetes-sized repositories.

| Variable | Meaning |
|---|---|
| `CODEWEFT_REPOS` | Repositories, separated by spaces, commas or new lines: `owner/name` on GitHub, any Git URL, or an absolute path indexed in place. Prefix `name=` to choose the label, suffix `#branch` for a branch other than the default. |
| `CODEWEFT_GITHUB_ORG` | Also serve every repository of this organization or user (not archived ones, nor forks unless `CODEWEFT_GITHUB_INCLUDE_FORKS=true`). New repositories are picked up on their first push, or at the next periodic sync. |
| `GITHUB_TOKEN` | Clones private repositories and lists the organization. Passed to Git as an HTTP header through the environment: never written into a URL, a clone's configuration or a log. A fine-grained token with read access to *Contents* and *Metadata* is enough. |
| `GITHUB_WEBHOOK_SECRET` | Enables `/webhooks/github`. Deliveries without a valid signature are refused. |
| `CODEWEFT_API_TOKENS` | Bearer tokens clients must send, separated by commas. Required: the server refuses to start without, unless `CODEWEFT_ALLOW_UNAUTHENTICATED=true` (for a server only reachable behind something that authenticates). |
| `CODEWEFT_DATA_DIR` | Clones and indexes (default `./codeweft-data`); keep it on a persistent volume. |
| `CODEWEFT_DATABASE_URL` (or `DATABASE_URL`) | Optional: a `postgres://` URL to keep the indexes in PostgreSQL rather than SQLite files, so that several replicas can serve them (see below). |
| `CODEWEFT_SYNC_INTERVAL` | Periodic sync of every repository (default `10m`; `0` turns it off), which also catches missed webhooks. |
| `CODEWEFT_SYNC_CONCURRENCY` | Repositories indexed at once (default 2). |
| `CODEWEFT_GITHUB_URL`, `CODEWEFT_GITHUB_API_URL` | For GitHub Enterprise Server. |

Besides `/mcp` and `/webhooks/github`, the server answers `/healthz` (the process runs), `/readyz` (a repository is indexed), `/status` (each repository's branch, commit, index time, counts and last error, as JSON) and `/metrics` (Prometheus); the last two need a token. Every repository is served as in a [workspace](#several-repositories): results are labelled with their repository, every tool takes a `repo` argument, and `get_service_links` links the services across repositories. Code from the repositories is only parsed, never built or run.

**Where the indexes live.** By default, in SQLite files on the data volume: the server is their only user, so no database has to be run, and one replica serves any number of repositories. Set `CODEWEFT_DATABASE_URL` to a PostgreSQL database (version 13 or later; a managed one is fine) when you want several replicas behind a load balancer, or a database you already back up. The server creates what it needs: one schema per repository, named `cw_<repository>`, rebuilt by itself when an upgrade changes the index format. Replicas share the indexes and take turns: each fetches its own clone, one indexes a new commit while the others wait, and they then find it already indexed. A restarted server serves the index it finds at once, before its first fetch. With the Helm chart, set `database.url` (or `database.existingSecret`) and `replicaCount`.

Measured on the machine of the [Benchmarks](#benchmarks) (Go standard library, PostgreSQL 16 on the same host, median of three runs): a first full index took 19 s, against 8.5 s for SQLite in the same runs, since every row crosses a connection; re-indexing after a push cost the same on both (0.2 s with nothing to do, 0.5 s after a one-file edit).

### CLI

```
codeweft index  [--db FILE] [--include-tests] [--skip-generated] [--platform GOOS/GOARCH|all] [--exclude dir1,dir2] PATH
codeweft query  [--db FILE] [--kind KIND] [--limit N] [--json] TEXT...
codeweft symbol [--db FILE] [--json] NAME
codeweft reindex [--db FILE] [--include-tests] [--skip-generated] [--platform GOOS/GOARCH|all] [--exclude dir1,dir2] [PATH]
codeweft deps   [--db FILE] [--callers] [--depth N] [--external] NAME
codeweft impl   [--db FILE] [--up] [--depth N] [--json] NAME
codeweft tests  [--db FILE] [--depth N] [--json] NAME
codeweft file   [--db FILE] [--json] PATH
codeweft links  [--db FILE | --workspace FILE] [--kind KIND] [--json] [QUERY]
codeweft apis   [--db FILE] [--method M] [--json] [ROUTE]
codeweft overview [--db FILE] [--json]
codeweft watch  [--db FILE] [--interval 2s] [PATH]
codeweft serve  [--db FILE] [--root PATH] [--watch=false]
codeweft serve  --workspace FILE [--watch=false]
codeweft server [--addr :8080] [--data DIR]
```

`reindex`, `watch` and `serve` reuse the `--include-tests`, `--skip-generated`, `--platform` and `--exclude` settings the index was built with; flags passed to `reindex` override them.

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
Claude Code ◀── MCP over stdio ◀── codeweft serve
```

1. **Index:** walks the repo, skips `vendor/`, `testdata/` and hidden directories, and parses files in parallel: Go with the standard library `go/ast`, Java and Python with [tree-sitter](https://tree-sitter.github.io/). Every language produces the same model, so search and the call graph work identically across them. Generated files (`// Code generated ... DO NOT EDIT.`) are indexed and flagged: search ranks them after hand-written code, and tools label them; `--skip-generated` leaves them out. Go files are selected for one build configuration, as the `go` command and gopls do (see below).
2. **Store:** writes everything to SQLite in one transaction. A contentless FTS5 table indexes names, split name words, signatures and docs, ranked with BM25 and weighted towards names.
3. **Resolve:** turns raw call sites into a call graph (see below).
4. **Serve:** exposes the index through the official [MCP Go SDK](https://github.com/modelcontextprotocol/go-sdk) over stdio.

### Call resolution

Resolution is syntactic: nothing is built or type-checked, so it works on any checkout, even one that does not build or whose dependencies are not downloaded. The type of a call's receiver is still worked out from declarations: the parser records how it can be found (a stated type, how a local was assigned, a package-level variable, a function's or method's declared result, a field, an element of a slice or map), and the resolver follows that once every file is indexed. Each call site is resolved by the first rule that applies:

| Rule | Example | Result |
|---|---|---|
| Same package, or a method without a receiver in a class | `newOrder(id)`; Java `size()` inherited from a superclass, `check()` of an enclosing class | exact edge |
| Package import (inside the module, via `go.mod`) | `payments.NewPaymentService(gw)` | exact edge |
| Receiver whose type is stated or can be worked out | `s.gw.Charge(...)` with `gw Gateway`; `svc := payments.NewPaymentService(gw); svc.ProcessPayment(...)`; Java `base().size()`; Python `adapter = self.get_adapter(url); adapter.send(r)` with `-> BaseAdapter` | exact edge to the method, also promoted from an embedded type or inherited |
| Type parameter | `p.Add(q).Bytes()` with `P Point[P]` | exact edge to the constraint's method |
| Python name re-exported by a package | `requests.get(...)` with `from .api import get` in `requests/__init__.py` | exact edge to `requests.api.get` |
| Python module path rooted at an import | `pkg.models.Response()`, `os.path.join(...)` | exact edge to `pkg.models.Response`; external reference `os.path.join` |
| Call on the supertype | `super.save(o)`, `super(...)`, `Named.super.name()`; Python `super().get(sku)` | exact edge to the first supertype declaring it |
| Receiver of a type outside the repository | `sql.Open(...)`, `db.Query(...)` with `db *sql.DB` | external reference `database/sql.DB.Query` |
| Method on a value of unknown type | `servicelocator.Get("x").ProcessPayment(...)` | edge only if exactly one method has that name, marked *matched by method name*; never for names of built-in types' methods (`update`, `get`, `size`, ...) |

Go: scopes follow definition order (in `x := f(x)` the right-hand `x` is the earlier one), type switches give each case its type, anonymous struct and interface types and generics (constraints, instantiation arguments) are followed, and an interface with an unexported method only matches methods of its own package.

Java: names resolve as javac does: a simple type name is a member type of the class, its enclosing classes or their supertypes before a type of the package or `java.lang`; type variables resolve to their bound; overloads declared in different classes are told apart by the number of arguments; a constructor without `this(...)` or `super(...)` calls its superclass's; methods of `Object` are always there. Java overloads share one node in the graph.

A `super` call is looked up through the declared class hierarchy, depth first in declaration order. Java's `super.m()` starts from the superclass (and what it inherits, interfaces included), never from the class's own interfaces, which take `Interface.super.m()`. Python's `super().m()` and `super(Cls, self).m()` search the base classes left to right, which is Python's method resolution order except in diamond-shaped hierarchies. A method found only on a supertype outside the repository becomes an external reference to it.

Anything ambiguous is dropped rather than guessed: a receiver of a known type that has no such method, a value that came from an outside type (`list.get(0).size()`), `new Runnable() { ... }` of an interface. Raw call sites are stored, so the graph can be rebuilt without re-parsing files. How well this matches a type checker is measured under [Accuracy](#accuracy).

### TypeScript and JavaScript

`.ts`, `.tsx`, `.js`, `.jsx`, `.mjs` and `.cjs` files are parsed with tree-sitter (type declarations, `.d.ts`, and minified bundles are skipped). A file is a module named by its path (`src/orders/service`; an `index` file by its directory), and its symbols by the module: `src/orders/service.OrderService.place`. Functions (declared, or assigned as `const f = () => {}`, `exports.f = function`, `X.prototype.m = function`), classes with their methods, constructors and fields, interfaces with their method signatures, type aliases and enums are indexed with their JSDoc. ES imports and `require` map local names to modules (relative paths resolved, packages kept as named), so calls resolve to imported functions, `this.method()` (inherited too), `super`, fields typed in the class or by constructor parameters (`constructor(private repo: OrderRepository)`), variables created with `new` or annotated with a type, and static calls; other method calls fall back to the method's name, never across languages and never for the built-in names of JavaScript values (`map`, `then`, `push` ...). `extends` and `implements` give the type hierarchy. Routes come from Express-style registrations (`app.get("/orders/:id", handler)`, with `app.use("/prefix", router)`) and NestJS controllers (`@Controller` and `@Get(":id")`); HTTP calls from `fetch` and axios; `process.env.X` is a placeholder resolved from configuration; Kafka (kafkajs), RabbitMQ (amqplib), SQS (AWS SDK v3 commands) and Pub/Sub are linked as in the other languages. In test files (`*.test.ts`, `*.spec.js`, and `__tests__`, `test` and `tests` directories), each `it(...)` and `test(...)` is a test, so `find_tests` works. Not handled: `tsconfig` path aliases, types inferred without an annotation or `new`, and gRPC and Temporal in TypeScript. On the [NestJS RealWorld app](https://github.com/lujakob/nestjs-realworld-example-app), all 21 routes are found with their controller methods and every call resolved within the repository is exact; on [Express](https://github.com/expressjs/express) itself, written in older JavaScript, its 92 test files reach the library through 880 test edges.

### Build configurations (Go)

Go code often declares a name once per platform or build tag (`conn_linux.go`, `conn_windows.go`, a stub behind `//go:build omitfeature`). Indexing all of them at once would merge declarations that never coexist, so Go files are selected for one build configuration with `go/build`'s own matching: by default the GOOS/GOARCH codeweft runs on; `--platform windows/amd64` picks another; `--platform all` indexes every file. The choice is stored with the index, so re-indexing and watching keep it.

### Type hierarchy

Java `extends`/`implements` and Python base classes are recorded as declared. Go has no `implements` clause, so a Go type implements an interface when its method set has every method of the interface with the same parameter and result types:

- the method set includes methods promoted from embedded types, following Go's rules: the shallowest embedding wins, and two at the same depth cancel out;
- type names in signatures are resolved through each file's package and imports, so `payments.Card` and `Card` inside package `payments` match while `fs.File` and `http.File` do not; `byte`/`uint8`, `rune`/`int32` and `interface{}`/`any` are treated as the same type, and type aliases declared in the repository are followed;
- an interface that embeds an interface from outside the repository has a method set the index cannot see in full, so nothing is said to implement it.

- an unexported method name only matches a method declared in the interface's own package (a type elsewhere can still get it by embedding);
- interfaces may overlap (`interface{ Gateway; VoidingGateway }`): their method set is the union.

Measured against the Go type checker (`go/types`) on the Go 1.24.7 standard library (linux/amd64), for the 263 non-generic interfaces and 2,921 non-generic concrete types it type-checks: precision 100% (all 3,969 reported implementations are real), recall 99.97% (3,969 of 3,970). Generic types are not matched. See [Accuracy](#accuracy).

### Tests

Test files (`*_test.go`; `*Test.java`, `*Tests.java`, `*IT.java` and `src/test/`; `test_*.py`, `*_test.py` and `tests/`) are parsed too, but kept apart from the code: their functions are not searchable symbols, are not part of the call graph or the type hierarchy, and a test double's method can never make a call in the code ambiguous. What is kept is each test function and the calls it makes, resolved against the code and against helper functions in test files. `find_tests` then walks up from a symbol through its callers (3 calls by default) and through test helpers to the tests the runner calls: Go `Test`, `Benchmark`, `Fuzz` and `Example` functions, JUnit `@Test` methods (and JUnit 3 `test` methods), and Python `test_` functions and methods. Tests that reach code only through calls the index cannot resolve (an HTTP request through Spring's `MockMvc`, a function value) are not found.

`--include-tests` additionally indexes test code as ordinary code, so it shows up in search and the call graph.

Parsing tests costs index time; the [Benchmarks](#benchmarks) include it. For the 50 most-called functions, `find_tests` answers in 7.8 ms at the median and 110 ms at the 95th percentile on the Go standard library (`fmt.Sprintf` alone is reached by over 8,000 tests), and in 2.7 / 10 ms on Guava.

### Incremental re-index

`codeweft reindex` hashes every file, re-parses only those whose content changed and removes deleted ones. Most edits change only function bodies, and each file also records a *shape*: a hash of what call resolution reads from it (its declarations and their signatures, imports, fields, variables and supertypes), not its bodies, docs or line numbers. When every changed file keeps its shape and no file was added or removed, no call elsewhere can resolve differently. The changed files are then updated in place, keeping their symbols' ids, so the edges pointing at them stay valid, and only their own calls are resolved again. Any other edit rebuilds the call graph, the test edges and the type hierarchy from all stored call sites, so edges *into* a changed file stay correct even though the files calling it were not re-parsed; only the edges that differ are written. A test checks, edit by edit, that both paths give exactly the index a full build gives.

`codeweft watch` (and `serve`, unless started with `--watch=false`) first runs one incremental re-index to catch up on edits made while nothing was watching, then polls file sizes and modification times every couple of seconds and runs the same incremental re-index when something changes. The baseline for polling is taken before the catch-up run, so a file saved during it is not missed. Polling needs no platform-specific file events and stays cheap on large trees.

Search input is always tokenised and quoted before it reaches FTS5, so user text can never inject query syntax. A query first requires every word to match, then falls back to matching any word.

### Service links

Routes come from the same detection as `list_apis`. Client calls are found in Go `net/http` (`http.Get`, `Post`, `NewRequest`, `NewRequestWithContext` and the methods of an `http.Client`), Java Spring `RestTemplate`, `WebClient` and `RestClient`, and `@FeignClient` interfaces, and Python `requests` and `httpx` (module functions, `Session` and `Client`). The URL is worked out as far as the source shows it: literals, concatenation, string constants, `fmt.Sprintf`, `String.format`, f-strings, `%` and `.format`, `path.Join` and `url.JoinPath`; any part the index cannot see (a base URL from configuration, an id from a variable) becomes a wildcard. Scheme and host are dropped, since services usually find each other through configuration, and path parameters (`{id}`, `:id`, `<int:id>`) become wildcards too. A call then links to the routes whose path matches segment by segment with a compatible method, keeping the most specific ones. A link is marked `exact` when the whole URL was written out, and `pattern` otherwise.

Temporal workflows and activities are found for all three SDKs. Providers: Go functions taking a `workflow.Context` and worker registrations (`RegisterWorkflow`, `RegisterActivity` of a function or of a struct, whose exported methods are then activities, and `RegisterOptions.Name`); Java `@WorkflowInterface` / `@WorkflowMethod` and `@ActivityInterface` / `@ActivityMethod` (with `namePrefix`), reported at the implementing class, and classes registered with a worker; Python `@workflow.defn` and `@activity.defn` (with `name=`). Uses: `ExecuteWorkflow`, `SignalWithStartWorkflow`, `ExecuteChildWorkflow`, `ExecuteActivity` and `ExecuteLocalActivity` in Go; workflow stubs and calls on activity stubs, typed or untyped, in Java; `execute_workflow`, `start_workflow`, `execute_child_workflow` and the `execute_activity` family in Python. Each side is named the way Temporal names it (a Java activity method `chargeCard` is `ChargeCard`), so a Java workflow can start a Go one by name and a Go workflow can run a Python activity. The task queue is read from the worker (`worker.New`, `newWorker`) and from the start options; a start and a worker on different queues are not linked. When the call passes the code itself (a workflow function, a stub of an interface, `OrderWorkflow.run`), it links to that code rather than to everything sharing its name.

On Temporal's own sample repositories ([Go](https://github.com/temporalio/samples-go), [Java](https://github.com/temporalio/samples-java), [Python](https://github.com/temporalio/samples-python)), dozens of separate sample apps with the same workflow names in one repository:

| Repository | Workflow and activity starts | Linked | To exactly one provider | Not linked |
|---|---|---|---|---|
| samples-go | 240 | 231 (96.2%) | 222 | 9: names built at run time (dynamic workflows, a name in a variable) |
| samples-java | 275 | 270 (98.2%) | 249 | 5: dynamic workflows, and stubs of a signal listener interface |
| samples-python | 300 | 297 (99.0%) | 290 | 3: a workflow or activity held in a variable |

A start linked to several providers is reached by several: an activity interface with several implementations, two versions of a workflow, or one function registered on several task queues.

gRPC services are found from the code generated by `protoc` for each language. Servers: a Go struct registered with `RegisterXServer` or embedding `UnimplementedXServer`, a Java class extending `XGrpc.XImplBase`, a Python class subclassing `XServicer`, including methods written in their subclasses. Clients: calls on a Go client from `NewXClient` (or a variable, parameter or struct field of type `XClient`), on a Java stub from `XGrpc.newBlockingStub`, `newStub` or `newFutureStub` (or of a stub type), and on a Python `XStub(channel)` held in a variable or a `self` attribute. Methods are matched as `Service/Method`. `.proto` files are indexed too: their services, RPC methods and messages are searchable, they give each link the service's full name (`shop.orders.v1.OrderService`), and only the methods a service declares count as RPC methods of its server. On Google's [microservices-demo](https://github.com/GoogleCloudPlatform/microservices-demo), all 13 gRPC calls into its Go, Java and Python services link to their servers (the email service's two implementations both count); the 9 calls into its C# and Node.js services are listed as not provided by anything indexed.

Kafka topics link each producer to every consumer of the topic, one per consumer group. Consumers: Spring `@KafkaListener` (on a method, or a class with `@KafkaHandler` methods) and `KafkaConsumer.subscribe` in Java; `kafka.ReaderConfig` (segmentio/kafka-go), sarama `ConsumePartition` and consumer groups (reported at the handler's `ConsumeClaim`), confluent-kafka-go `Subscribe`/`SubscribeTopics` and franz-go `ConsumeTopics` in Go; `KafkaConsumer`/`AIOKafkaConsumer` and `subscribe` in Python. Producers: `KafkaTemplate.send` (also inside `executeInTransaction`) and `sendDefault`, and `Producer.send(new ProducerRecord(...))` in Java; `kafka.Writer`/`kafka.Message`, `sarama.ProducerMessage`, confluent `kafka.Message` and `kgo.Record` in Go; `send`, `send_and_wait` and `produce` on kafka-python, aiokafka and confluent-kafka producers in Python. Topics are evaluated through constants and local variables. A topic read from configuration (`"${orders.topic}"`, directly or through an `@Value` field) is kept as the property's name, so a producer and a consumer reading the same property link, marked as a pattern. On the samples of [spring-kafka](https://github.com/spring-projects/spring-kafka) all 10 producers found link to their consumers (19 links); of the consumers left without a producer, one is fed from Kotlin and one is a dead-letter topic written by the framework. On the examples of [confluent-kafka-go](https://github.com/confluentinc/confluent-kafka-go) all 5 producers link. The examples of kafka-go and confluent-kafka-python take every topic from an environment variable or a command-line argument, so nothing is linked there rather than guessed.

RabbitMQ, Amazon SQS and Google Cloud Pub/Sub are linked the same way, in all three languages. For RabbitMQ, a publish names an exchange and a routing key, and a consumer reads a queue: the link follows the bindings declared in code (`QueueBind` in Go, `queue_bind` in pika, `@QueueBinding` on a Spring `@RabbitListener`), matching topic patterns (`order.*`, `#`), and says which binding it went through; a publish to the default exchange goes to the queue its key names. Producers: `Publish`/`PublishWithContext` (amqp091-go, streadway/amqp), pika's `basic_publish`, Spring's `RabbitTemplate.convertAndSend`; consumers: `Consume`, `basic_consume`, `@RabbitListener`. For SQS, producers and consumers meet on the queue name, taken from a queue URL or ARN when that is what the code has (also from configuration): `SendMessageInput`/`ReceiveMessageInput` (AWS SDK for Go), boto3's `send_message`/`receive_message` and resource queues, the AWS SDK for Java's request builders, Spring Cloud AWS's `@SqsListener` and `SqsTemplate`. For Pub/Sub, publishers name a topic and subscribers a subscription; the link follows the subscriptions created in code (`CreateSubscription`, `create_subscription`, `createSubscription`). Subscriptions created only in infrastructure code (Terraform, the console) are not seen, and their consumers are listed without a producer.

Names often live in configuration rather than code: a topic in `application.yml`, a base URL in an environment variable. codeweft reads Spring's `application*.yml`/`.yaml`/`.properties` and `bootstrap*` files (with their profiles) and `.env` files, and treats `os.Getenv("X")` (Go), `os.getenv`/`os.environ` (Python) and `System.getenv` (Java) as the placeholder `${X}`, like Spring's `${...}`. Placeholders in topics, URLs, routes, queues and workflow names are then resolved from the configuration file nearest to the code (so a monorepo's modules keep their own values), a base file before a profile's, falling back to the placeholder's default; a key also matches its environment-variable form (`orders.topic` and `ORDERS_TOPIC`), as Spring binds them. A Java producer sending to `${inventory.topic}` thus links to a Python consumer of `os.environ["RESTOCK_TOPIC"]` when both resolve to the same topic. Keys that look like secrets (passwords, tokens, keys, credentials) are never read. A placeholder left unresolved still links to the same placeholder elsewhere, as a pattern.

Name matching between a call and a method never crosses languages: a Go call on a value of unknown type is not linked to a Python method of the same name.

### Output size

Every tool result lands in the agent's context window, so results are capped and always say what was left out and how to get it: `get_symbol` shows the first 200 lines of source and the file range of the rest, and at most 15 callers and callees; an ambiguous name lists up to 30 candidates with the total count; `get_file_summary` lists 100 declarations at a time (an `offset` argument gives the next ones) and the 20 most-connected related files in each direction; `get_dependency_graph` lists at most 20 callers or callees under a node and 120 nodes in all, and says under each node how many it left out. Every tool is annotated as read-only.

On the Go standard library, `get_symbol String` went from 12.1 KB of text (the full source of 20 of its matches) to 4.1 KB (a list of candidates), and the longest function (`cmd/internal/obj/arm64.ctxt7.asmout`, 2,457 lines) from 60 KB to 6.5 KB.

## Design decisions

| Decision | Choice | Why |
|---|---|---|
| Parser | `go/ast` for Go, tree-sitter for Java and Python | Exact for Go; one robust, error-tolerant parser for the rest, all behind the same symbol model. |
| Storage | SQLite + FTS5, or PostgreSQL | SQLite: zero config, a single portable file, fast ranked search. PostgreSQL (weighted `tsvector` search) only for servers with several replicas. |
| Search | Keyword + BM25, no embeddings | Deterministic, explainable results with no model or network dependency. |
| Call graph | Syntactic resolution with types worked out from declarations, no type checker | Works on any checkout without building it; measured against each language's type checker; precision is marked per edge. |
| Transport | stdio | What local MCP clients like Claude Code use. |

## Accuracy

The call graph is checked against a compiler or a static analyser for each language. The tools are in `scripts/accuracy`.

| Language | Reference | Precision | Recall |
|---|---|---|---|
| Go: Go 1.24.7 standard library, linux/amd64 | `go/types` (the type checker) | 99.92% (44,074 of 44,110 edges) | 99.37% (44,373 of 44,654 calls); 99.56% where caller and callee are indexed |
| Go: implementations, same tree | `go/types` | 100% (3,969 of 3,969) | 99.97% (3,969 of 3,970) |
| Java: Guava `guava/src` (`fce80fd`) | `javac` | 99.45% (14,140 of 14,218 edges) | 97.30% (14,140 of 14,533 calls inside Guava) |
| Python: requests `src` (`3c7baaa`) | jedi 0.19.2 | 99.17% (239 of 241 edges) agree with jedi | 99.17% (239 of 241) of jedi's edges found |

- Precision counts edges between two symbols of the repository; recall counts the distinct caller-to-callee pairs the reference finds. Edges matched only by method name are part of both: in Go they are 81 of 44,110 (85% right), in Java 61 of 14,218 (66% right).
- Calls that leave the repository are external references: in Guava 82% of the calls into the JDK and other libraries are found as such (`java.util.List.get`). In the standard library, 299 of the 375 calls into `vendor/` (dependencies, not indexed) are.
- What remains in Go: calls through values the syntax cannot follow (function values, a variable narrowed by an earlier `if`), and code in the 2 files over the size limit. In Java: receivers that come out of generic library types (`map.get(k).foo()`), lambda parameters without stated types, and overloads with the same number of arguments in different classes. Python has no type checker that decides every call, so jedi is a reference to compare with, not ground truth; the four disagreements on requests are listed by `compare.py python` (two calls through attributes set at runtime on another class's objects, which jedi follows, and two where codeweft's answer is the more specific one).

To reproduce (Go):

```bash
make build && ./bin/codeweft index --db /tmp/std.db "$(go env GOROOT)/src"
cd scripts/accuracy && mkdir -p /tmp/truth
go run ./calls /tmp/truth && go run ./implements /tmp/truth
python3 compare.py calls /tmp/std.db /tmp/truth
python3 compare.py implements /tmp/std.db /tmp/truth
```

Java (needs a JDK) and Python (needs jedi):

```bash
./bin/codeweft index --db /tmp/guava.db guava/guava/src
javac -d /tmp/ct scripts/accuracy/java/CallTruth.java && java -cp /tmp/ct CallTruth guava/guava/src > /tmp/java.txt
python3 scripts/accuracy/compare.py java /tmp/guava.db /tmp/java.txt

./bin/codeweft index --db /tmp/requests.db requests
python3 -m venv /tmp/jedi && /tmp/jedi/bin/pip install jedi==0.19.2
/tmp/jedi/bin/python scripts/accuracy/python/truth.py requests > /tmp/py.txt
python3 scripts/accuracy/compare.py python /tmp/requests.db /tmp/py.txt
```

## Benchmarks

Reproducible with `make bench`: [`scripts/bench`](scripts/bench) clones each repository of [`corpus.json`](scripts/bench/corpus.json) at its pinned tag or commit, and measures:
- indexing as a separate process, from scratch each run, with CPU time and peak memory;
- incremental re-indexing, with nothing changed and with the largest file edited;
- every MCP tool through the real MCP server, with queries generated from the index: the 50 most-called functions, the types with the most implementations, the largest files, and words from symbol names;
- the time from starting `codeweft serve` to its first tool answer.

Test files are parsed and kept apart (see [Tests](#tests)); Go files are those of linux/amd64. "Files" counts files other than tests; "Call edges" counts calls resolved to the repository's own code.

Machine: linux/amd64, 4 vCPUs (Intel(R) Xeon(R) Processor @ 2.10GHz), 16 GB RAM, go1.24.7. Full index: median of 3 runs, each from scratch (fastest and slowest in brackets).

| Repository | Files | Lines | Symbols | Call edges | Full index | CPU | Peak memory | Index size | Re-index, no change | Re-index, one file | Server start |
|---|---|---|---|---|---|---|---|---|---|---|---|
| MCP Go SDK `v1.1.0` | 56 | 14,043 | 939 | 740 | 0.16 s (0.13-0.17) | 0.2 s | 27 MB | 2 MB | 0.01 s | 0.03 s | 17 ms |
| Go standard library `go1.24.7` | 2,503 | 1,107,573 | 38,642 | 210,683 | 7.15 s (6.85-7.17) | 11.1 s | 354 MB | 118 MB | 0.17 s | 0.47 s | 235 ms |
| Kubernetes `v1.31.0` | 8,188 | 1,696,593 | 91,865 | 85,601 | 13.08 s (13.04-19.91) | 20.2 s | 586 MB | 231 MB | 0.42 s | 1.33 s | 259 ms |
| Guava `v33.4.0` | 2,221 | 497,267 | 36,928 | 54,696 | 8.06 s (7.83-9.11) | 18.0 s | 365 MB | 122 MB | 0.24 s | 0.53 s | 134 ms |
| Spring PetClinic `500158f` | 33 | 1,942 | 118 | 92 | 0.07 s (0.07-0.09) | 0.1 s | 49 MB | 1 MB | 0.01 s | 0.01 s | 14 ms |
| requests `3c7baaa` | 22 | 6,874 | 306 | 273 | 0.22 s (0.22-0.26) | 0.4 s | 49 MB | 1 MB | 0.01 s | 0.03 s | 13 ms |
| Django `5.1.4` | 907 | 159,320 | 10,606 | 10,972 | 6.88 s (6.33-8.23) | 20.0 s | 225 MB | 46 MB | 0.17 s | 0.27 s | 36 ms |
| microservices-demo `v0.10.2` | 53 | 22,678 | 2,366 | 462 | 0.19 s (0.17-0.26) | 0.3 s | 49 MB | 3 MB | 0.01 s | 0.03 s | 15 ms |

Tool latency through the MCP protocol, p50 / p95 in ms:

| Tool | MCP Go SDK | Go standard library | Kubernetes | Guava | Spring PetClinic | requests | Django | microservices-demo |
|---|---|---|---|---|---|---|---|---|
| `search_code` | 0.7 / 1.2 | 2.2 / 11 | 4.4 / 24 | 1.8 / 5.2 | 0.7 / 1.2 | 0.6 / 1.4 | 1.4 / 3.0 | 0.9 / 1.9 |
| `get_symbol` | 1.3 / 2.6 | 20 / 88 | 9.4 / 61 | 39 / 171 | 1.5 / 2.9 | 1.1 / 2.6 | 8.2 / 14 | 1.2 / 1.9 |
| `find_tests` | 0.7 / 1.8 | 5.8 / 71 | 2.3 / 4.6 | 3.0 / 18 | 0.5 / 1.7 | 0.5 / 1.5 | 2.0 / 4.7 | 0.4 / 0.7 |
| `get_dependency_graph` | 0.6 / 1.7 | 1.1 / 14 | 0.6 / 5.5 | 1.4 / 12 | 0.5 / 1.2 | 0.4 / 1.0 | 1.1 / 2.7 | 0.5 / 1.5 |
| `get_implementations` | 0.5 / 1.1 | 8.3 / 11 | 22 / 28 | 12 / 18 | 0.7 / 0.9 | 0.4 / 0.8 | 3.5 / 9.3 | 0.5 / 0.9 |
| `get_file_summary` | 0.9 / 2.8 | 4.7 / 7.5 | 4.5 / 7.7 | 6.6 / 9.9 | 0.8 / 1.5 | 0.9 / 4.9 | 3.5 / 5.1 | 1.1 / 3.9 |
| `get_architecture_overview` | 3.3 / 3.9 | 199 / 201 | 247 / 332 | 145 / 145 | 2.0 / 2.7 | 2.1 / 2.2 | 25 / 26 | 5.1 / 5.7 |
| `list_apis` | 0.4 / 0.5 | 1.0 / 1.4 | 4.2 / 5.0 | 0.4 / 0.4 | 1.4 / 1.4 | 0.3 / 0.3 | 0.2 / 0.3 | 0.3 / 0.4 |
| `get_service_links` | 0.5 / 0.5 | 1.6 / 1.7 | 12 / 19 | 1.4 / 1.6 | 1.1 / 1.1 | 0.2 / 0.3 | 0.5 / 0.7 | 4.7 / 5.2 |

Response size, p50 / largest in KB (about 250 tokens per KB):

| Tool | MCP Go SDK | Go standard library | Kubernetes | Guava | Spring PetClinic | requests | Django | microservices-demo |
|---|---|---|---|---|---|---|---|---|
| `search_code` | 1.4 / 2.3 | 1.3 / 2.1 | 2.0 / 3.5 | 2.0 / 3.4 | 1.2 / 3.0 | 1.4 / 2.9 | 1.4 / 2.5 | 1.3 / 2.1 |
| `get_symbol` | 1.8 / 4.4 | 2.8 / 9.3 | 4.2 / 6.8 | 6.6 / 23.3 | 1.5 / 7.0 | 2.2 / 8.9 | 3.9 / 9.3 | 1.0 / 3.3 |
| `find_tests` | 0.9 / 3.4 | 2.9 / 4.8 | 4.5 / 6.7 | 4.8 / 8.0 | 0.3 / 3.7 | 0.6 / 4.0 | 4.0 / 6.0 | 0.2 / 0.6 |
| `get_dependency_graph` | 0.4 / 2.5 | 1.0 / 8.4 | 0.5 / 11.4 | 1.7 / 25.1 | 0.4 / 3.3 | 0.3 / 1.9 | 0.7 / 7.5 | 0.3 / 2.8 |
| `get_implementations` | 0.3 / 1.7 | 3.8 / 4.9 | 6.1 / 10.5 | 3.4 / 9.6 | 0.5 / 1.2 | 0.2 / 1.7 | 2.4 / 4.9 | 0.3 / 0.4 |
| `get_file_summary` | 1.9 / 11.0 | 7.1 / 13.6 | 6.9 / 21.6 | 13.8 / 22.3 | 1.1 / 3.1 | 1.5 / 8.9 | 8.8 / 15.3 | 2.5 / 11.5 |
| `get_architecture_overview` | 5.5 / 5.5 | 6.4 / 6.4 | 7.8 / 7.8 | 9.6 / 9.6 | 2.9 / 2.9 | 1.4 / 1.4 | 6.0 / 6.0 | 4.2 / 4.2 |
| `list_apis` | 0.7 / 0.7 | 1.9 / 1.9 | 10.1 / 10.1 | 0.0 / 0.0 | 3.5 / 3.5 | 0.0 / 0.0 | 0.0 / 0.0 | 0.0 / 0.0 |
| `get_service_links` | 0.1 / 0.1 | 0.1 / 0.1 | 24.7 / 24.7 | 0.1 / 0.1 | 0.1 / 0.1 | 0.1 / 0.1 | 0.1 / 0.1 | 5.4 / 5.4 |

What the numbers say:
- **Indexing** takes about 1 s per 150k lines of Go and 60k lines of Java on this machine. Django's Python is the slowest per line (6.9 s for 159k lines) because tree-sitter does all of the parsing. "Files" includes configuration files and, where a repository has them, TypeScript and JavaScript.
- **Memory** peaks while indexing (586 MB for Kubernetes). A running server only holds the SQLite index open.
- **Re-indexing** after an edit takes under half a second on the Go standard library (0.47 s, and 0.17 s when nothing changed) and 1.3 s on Kubernetes: only the changed files are parsed, and an edit that keeps a file's declarations only resolves that file's calls again (see [Incremental re-index](#incremental-re-index)).
- **Tools** answer in single-digit milliseconds at the median, even on Kubernetes. The overview is the slowest (200 to 250 ms on the largest repositories); everything else stays under 40 ms at the median.
- **Response sizes** stay at a few KB at the median, and the largest answer of any tool on any of these repositories is 25 KB, thanks to the caps described in [Output size](#output-size).

## Contributing

Contributions are welcome: see [CONTRIBUTING.md](CONTRIBUTING.md), and [SECURITY.md](SECURITY.md) to report a vulnerability. Changes are listed in [CHANGELOG.md](CHANGELOG.md).

## Development

```bash
make test   # unit, golden-file and MCP protocol tests
make vet
```

Releases are built by `.github/workflows/release.yml` when a `v*` tag is pushed: one job per platform (Linux and Windows cross-compiled on Ubuntu, macOS on a macOS runner), a smoke test on the native ones, and a GitHub release with the archives and their checksums.

`scripts/accuracy` is a separate Go module, so its tools are not part of the build; see [Accuracy](#accuracy).

`scripts/eval/run.py` asks Claude Code the same questions about the Go standard library with and without codeweft (which functions call X, which types implement I), generated from the `go/types` ground truth, and scores each answer's precision and recall along with the tokens, turns and time it took. Running it spends Claude usage; `--dry-run` shows the questions without calling Claude. No results are published here yet.

- `internal/indexer`: golden-file tests over a fixture repo in `testdata/sample`, for the extracted symbols, the full call graph and the type hierarchy. After an intended change, run `go test -tags sqlite_fts5 ./internal/indexer -update` and review the diff.
- `internal/server`: drives the real MCP protocol through an in-memory client session.

## Roadmap

- [x] **v0.1:** Go indexer, SQLite + FTS5 store, `index` / `query` / `symbol` CLI
- [x] **v0.2:** MCP server over stdio with `search_code` and `get_symbol`
- [x] **v0.3:** call graph: `get_dependency_graph`, callers and callees in `get_symbol`
- [x] **v0.4:** `get_file_summary`, incremental re-index using file hashes
- [x] **v0.5:** Java and Python via tree-sitter
- [x] **v0.6:** `get_architecture_overview`, `list_apis`, watch mode
- [x] **v0.7:** workspaces of several repositories; `get_service_links` for HTTP routes, gRPC methods, Kafka, RabbitMQ, SQS and Pub/Sub, and Temporal workflows and activities, with values from configuration files; `.proto` files; TypeScript and JavaScript; `codeweft server` (self-hosted, kept current by GitHub webhooks) with a container image, Helm chart, npm and Homebrew packages; capped answers and incremental call graph updates
- [x] **v0.8:** PostgreSQL backend for the server, with several replicas
- [ ] Next: per-user sign-in (GitHub OAuth) with per-repository access
- [ ] Later: Kotlin, C# and Rust; gRPC and Temporal in TypeScript; bindings declared in infrastructure code (Terraform); optional semantic search with local embeddings

## License

[Apache License 2.0](LICENSE). Commits before the switch were published under the MIT License.
