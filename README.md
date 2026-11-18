# codewiki

**A local MCP server that gives AI coding agents structured, deterministic context about a codebase.**

When an agent like Claude Code needs to know *where rate limiting is implemented*, *what `ProcessPayment` looks like* or *what breaks if this function changes*, it usually greps, opens a dozen files and guesses. `codewiki` parses the code once into an index of functions, methods, types and the calls between them, then answers those questions directly over the [Model Context Protocol](https://modelcontextprotocol.io/).

- **Deterministic:** answers come from the parsed source, not from a model's guess.
- **Local:** one binary and one SQLite file. No cloud service, no API keys, no embeddings.
- **Agent-native:** exposed as MCP tools, so any MCP client can use it.

> Status: v0.6. Supports **Go, Java and Python**, and Protocol Buffers service definitions; see the [roadmap](#roadmap).

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
| `get_service_links` | How services call each other: each HTTP route with the client calls that reach it, each gRPC method with the stubs calling it, each Kafka topic's producers with its consumers, and each Temporal workflow and activity with the code starting it and its task queue, across languages and across repositories in a [workspace](#several-repositories). Also lists calls whose provider is not indexed. |
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

### Several repositories

Services that call each other usually live in separate repositories. A workspace file lists them, with paths relative to the file:

```json
{"repos": [{"name": "orders", "path": "../orders-service"}, {"path": "../web"}]}
```

```bash
claude mcp add codewiki -- /abs/path/to/codewiki/bin/codewiki serve --workspace /abs/path/to/workspace.json
```

Each repository keeps its own index at `PATH/.codewiki/index.db`, built and watched as with one repository. Every tool then answers across all of them: results are labelled with their repository (`[orders] main.getOrder`), a name found in two repositories lists both, and every tool takes an optional `repo` argument to look in one. `get_service_links` links a client call in one repository to the route serving it in another. A repository's name defaults to its directory name. `codewiki links --workspace FILE` prints the same links from the terminal.

### CLI

```
codewiki index  [--db FILE] [--include-tests] [--skip-generated] [--platform GOOS/GOARCH|all] [--exclude dir1,dir2] PATH
codewiki query  [--db FILE] [--kind KIND] [--limit N] [--json] TEXT...
codewiki symbol [--db FILE] [--json] NAME
codewiki reindex [--db FILE] [--include-tests] [--skip-generated] [--platform GOOS/GOARCH|all] [--exclude dir1,dir2] [PATH]
codewiki deps   [--db FILE] [--callers] [--depth N] [--external] NAME
codewiki impl   [--db FILE] [--up] [--depth N] [--json] NAME
codewiki tests  [--db FILE] [--depth N] [--json] NAME
codewiki file   [--db FILE] [--json] PATH
codewiki links  [--db FILE | --workspace FILE] [--kind KIND] [--json] [QUERY]
codewiki apis   [--db FILE] [--method M] [--json] [ROUTE]
codewiki overview [--db FILE] [--json]
codewiki watch  [--db FILE] [--interval 2s] [PATH]
codewiki serve  [--db FILE] [--root PATH] [--watch=false]
codewiki serve  --workspace FILE [--watch=false]
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
Claude Code ◀── MCP over stdio ◀── codewiki serve
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

### Build configurations (Go)

Go code often declares a name once per platform or build tag (`conn_linux.go`, `conn_windows.go`, a stub behind `//go:build omitfeature`). Indexing all of them at once would merge declarations that never coexist, so Go files are selected for one build configuration with `go/build`'s own matching: by default the GOOS/GOARCH codewiki runs on; `--platform windows/amd64` picks another; `--platform all` indexes every file. The choice is stored with the index, so re-indexing and watching keep it.

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

`codewiki reindex` hashes every file, re-parses only those whose content changed, removes deleted ones, and then rebuilds the call graph from the stored call sites. Edges *into* a changed file stay correct even though the files calling it were not re-parsed.

`codewiki watch` (and `serve`, unless started with `--watch=false`) first runs one incremental re-index to catch up on edits made while nothing was watching, then polls file sizes and modification times every couple of seconds and runs the same incremental re-index when something changes. The baseline for polling is taken before the catch-up run, so a file saved during it is not missed. Polling needs no platform-specific file events and stays cheap on large trees.

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

Name matching between a call and a method never crosses languages: a Go call on a value of unknown type is not linked to a Python method of the same name.

### Output size

Every tool result lands in the agent's context window, so results are capped and always say what was left out and how to get it: `get_symbol` shows the first 200 lines of source and the file range of the rest, and at most 15 callers and callees; an ambiguous name lists up to 30 candidates with the total count; `get_file_summary` lists the 20 most-connected related files in each direction. Every tool is annotated as read-only.

On the Go standard library, `get_symbol String` went from 12.1 KB of text (the full source of 20 of its matches) to 4.1 KB (a list of candidates), and the longest function (`cmd/internal/obj/arm64.ctxt7.asmout`, 2,457 lines) from 60 KB to 6.5 KB.

## Design decisions

| Decision | Choice | Why |
|---|---|---|
| Parser | `go/ast` for Go, tree-sitter for Java and Python | Exact for Go; one robust, error-tolerant parser for the rest, all behind the same symbol model. |
| Storage | SQLite + FTS5 | Zero config, a single portable file, fast ranked search. |
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
- What remains in Go: calls through values the syntax cannot follow (function values, a variable narrowed by an earlier `if`), and code in the 2 files over the size limit. In Java: receivers that come out of generic library types (`map.get(k).foo()`), lambda parameters without stated types, and overloads with the same number of arguments in different classes. Python has no type checker that decides every call, so jedi is a reference to compare with, not ground truth; the four disagreements on requests are listed by `compare.py python` (two calls through attributes set at runtime on another class's objects, which jedi follows, and two where codewiki's answer is the more specific one).

To reproduce (Go):

```bash
make build && ./bin/codewiki index --db /tmp/std.db "$(go env GOROOT)/src"
cd scripts/accuracy && mkdir -p /tmp/truth
go run ./calls /tmp/truth && go run ./implements /tmp/truth
python3 compare.py calls /tmp/std.db /tmp/truth
python3 compare.py implements /tmp/std.db /tmp/truth
```

Java (needs a JDK) and Python (needs jedi):

```bash
./bin/codewiki index --db /tmp/guava.db guava/guava/src
javac -d /tmp/ct scripts/accuracy/java/CallTruth.java && java -cp /tmp/ct CallTruth guava/guava/src > /tmp/java.txt
python3 scripts/accuracy/compare.py java /tmp/guava.db /tmp/java.txt

./bin/codewiki index --db /tmp/requests.db requests
python3 -m venv /tmp/jedi && /tmp/jedi/bin/pip install jedi==0.19.2
/tmp/jedi/bin/python scripts/accuracy/python/truth.py requests > /tmp/py.txt
python3 scripts/accuracy/compare.py python /tmp/requests.db /tmp/py.txt
```

## Benchmarks

Reproducible with `make bench`: [`scripts/bench`](scripts/bench) clones each repository of [`corpus.json`](scripts/bench/corpus.json) at its pinned tag or commit, and measures:
- indexing as a separate process, from scratch each run, with CPU time and peak memory;
- incremental re-indexing, with nothing changed and with the largest file edited;
- every MCP tool through the real MCP server, with queries generated from the index: the 50 most-called functions, the types with the most implementations, the largest files, and words from symbol names;
- the time from starting `codewiki serve` to its first tool answer.

Test files are parsed and kept apart (see [Tests](#tests)); Go files are those of linux/amd64. "Files" counts files other than tests; "Call edges" counts calls resolved to the repository's own code.

Machine: linux/amd64, 4 vCPUs (Intel(R) Xeon(R) Processor @ 2.30GHz), 16 GB RAM, go1.24.7. Full index: median of 3 runs, each from scratch (fastest and slowest in brackets).

| Repository | Files | Lines | Symbols | Call edges | Full index | CPU | Peak memory | Index size | Re-index, no change | Re-index, one file | Server start |
|---|---|---|---|---|---|---|---|---|---|---|---|
| MCP Go SDK `v1.1.0` | 56 | 14,043 | 939 | 740 | 0.10 s (0.10-0.12) | 0.2 s | 23 MB | 2 MB | 0.04 s | 0.04 s | 12 ms |
| Go standard library `go1.24.7` | 2,503 | 1,107,573 | 38,642 | 210,683 | 5.40 s (5.29-5.61) | 8.3 s | 362 MB | 118 MB | 2.87 s | 2.85 s | 182 ms |
| Kubernetes `v1.31.0` | 8,188 | 1,696,593 | 91,865 | 85,601 | 9.34 s (9.30-9.85) | 14.6 s | 659 MB | 230 MB | 4.46 s | 4.48 s | 193 ms |
| Guava `v33.4.0` | 2,219 | 497,238 | 36,924 | 54,696 | 5.76 s (5.70-5.84) | 12.9 s | 338 MB | 122 MB | 1.64 s | 1.77 s | 92 ms |
| Spring PetClinic `500158f` | 30 | 1,899 | 118 | 92 | 0.06 s (0.05-0.11) | 0.1 s | 59 MB | 1 MB | 0.01 s | 0.01 s | 8 ms |
| requests `3c7baaa` | 22 | 6,874 | 306 | 273 | 0.18 s (0.17-0.19) | 0.3 s | 59 MB | 1 MB | 0.02 s | 0.03 s | 9 ms |
| Django `5.1.4` | 883 | 156,354 | 10,591 | 10,960 | 4.82 s (4.72-4.97) | 14.2 s | 232 MB | 46 MB | 0.68 s | 0.79 s | 36 ms |
| microservices-demo `v0.10.2` | 47 | 22,123 | 2,342 | 451 | 0.13 s (0.13-0.14) | 0.2 s | 59 MB | 3 MB | 0.04 s | 0.04 s | 11 ms |

Tool latency through the MCP protocol, p50 / p95 in ms:

| Tool | MCP Go SDK | Go standard library | Kubernetes | Guava | Spring PetClinic | requests | Django | microservices-demo |
|---|---|---|---|---|---|---|---|---|
| `search_code` | 0.5 / 0.8 | 2.2 / 9.4 | 3.1 / 17 | 1.6 / 4.6 | 0.4 / 0.6 | 0.5 / 0.9 | 0.8 / 1.6 | 0.5 / 1.0 |
| `get_symbol` | 1.2 / 1.9 | 20 / 132 | 7.6 / 37 | 34 / 148 | 1.0 / 2.3 | 1.0 / 2.1 | 5.6 / 9.8 | 1.0 / 1.1 |
| `find_tests` | 0.7 / 1.2 | 7.8 / 110 | 1.9 / 4.0 | 2.7 / 10 | 0.5 / 1.3 | 0.5 / 1.3 | 1.6 / 3.2 | 0.4 / 0.6 |
| `get_dependency_graph` | 0.5 / 1.1 | 1.4 / 18 | 1.0 / 9.6 | 1.2 / 13 | 0.2 / 0.8 | 0.3 / 0.7 | 0.6 / 5.9 | 0.3 / 1.4 |
| `get_implementations` | 0.7 / 1.0 | 8.1 / 11 | 18 / 26 | 7.7 / 11 | 0.4 / 0.5 | 0.3 / 0.6 | 2.5 / 4.0 | 0.4 / 0.6 |
| `get_file_summary` | 1.0 / 3.3 | 6.1 / 19 | 9.0 / 16 | 5.2 / 14 | 0.4 / 0.8 | 0.5 / 2.8 | 2.6 / 5.5 | 0.7 / 5.0 |
| `get_architecture_overview` | 4.2 / 4.3 | 170 / 174 | 178 / 188 | 82 / 82 | 1.0 / 1.6 | 1.1 / 1.4 | 20 / 22 | 3.0 / 3.9 |
| `list_apis` | 0.6 / 0.7 | 0.9 / 1.0 | 3.0 / 4.1 | 0.3 / 0.6 | 0.7 / 0.8 | 0.2 / 0.5 | 0.2 / 0.3 | 0.2 / 0.3 |
| `get_service_links` | 0.6 / 0.7 | 1.4 / 1.5 | 246 / 258 | 0.8 / 0.9 | 0.5 / 0.7 | 0.1 / 0.2 | 0.4 / 0.5 | 5.3 / 5.9 |

Response size, p50 / largest in KB (about 250 tokens per KB):

| Tool | MCP Go SDK | Go standard library | Kubernetes | Guava | Spring PetClinic | requests | Django | microservices-demo |
|---|---|---|---|---|---|---|---|---|
| `search_code` | 1.4 / 2.3 | 1.3 / 2.1 | 2.0 / 3.5 | 2.0 / 3.4 | 1.2 / 3.0 | 1.4 / 2.9 | 1.4 / 2.5 | 1.4 / 2.2 |
| `get_symbol` | 1.8 / 4.4 | 2.8 / 9.3 | 4.2 / 6.8 | 6.6 / 23.3 | 1.5 / 7.0 | 2.2 / 8.9 | 3.9 / 9.3 | 1.0 / 3.3 |
| `find_tests` | 0.9 / 3.4 | 2.9 / 4.8 | 4.5 / 6.7 | 4.8 / 8.0 | 0.3 / 3.7 | 0.6 / 4.0 | 4.0 / 6.0 | 0.2 / 0.6 |
| `get_dependency_graph` | 0.4 / 2.5 | 1.0 / 33.2 | 0.5 / 44.5 | 1.7 / 59.9 | 0.4 / 3.3 | 0.3 / 1.9 | 0.7 / 24.2 | 0.3 / 4.3 |
| `get_implementations` | 0.3 / 1.7 | 3.8 / 4.9 | 6.1 / 10.5 | 3.4 / 9.6 | 0.5 / 1.2 | 0.2 / 1.7 | 2.4 / 4.9 | 0.3 / 0.4 |
| `get_file_summary` | 1.9 / 11.8 | 17.9 / 63.0 | 27.0 / 184.0 | 21.0 / 57.2 | 1.1 / 3.1 | 1.5 / 8.9 | 9.9 / 20.5 | 2.5 / 19.2 |
| `get_architecture_overview` | 5.5 / 5.5 | 6.4 / 6.4 | 7.8 / 7.8 | 9.5 / 9.5 | 2.9 / 2.9 | 1.4 / 1.4 | 5.9 / 5.9 | 4.0 / 4.0 |
| `list_apis` | 0.7 / 0.7 | 1.9 / 1.9 | 10.0 / 10.0 | 0.0 / 0.0 | 3.5 / 3.5 | 0.0 / 0.0 | 0.0 / 0.0 | 0.0 / 0.0 |
| `get_service_links` | 0.1 / 0.1 | 0.1 / 0.1 | 24.7 / 24.7 | 0.1 / 0.1 | 0.1 / 0.1 | 0.1 / 0.1 | 0.1 / 0.1 | 5.4 / 5.4 |

What the numbers say:
- **Indexing** takes about 1 s per 180k lines of Go and 100k lines of Java. Django's Python is the slowest per line (4.8 s for 156k lines) because tree-sitter does all of the parsing.
- **Memory** peaks while indexing (659 MB for Kubernetes). A running server only holds the SQLite index open.
- **Tools** answer in single-digit milliseconds at the median, even on Kubernetes. The overview is the slowest (about 180 ms on the largest repositories). `get_service_links` takes 250 ms on Kubernetes, which has many HTTP client calls to match.
- **Re-indexing** one changed file costs about the same as a run with no change (2.9 s on the standard library, 4.5 s on Kubernetes). Only the changed file is parsed, but the call graph, type hierarchy and test edges are rebuilt for the whole repository.
- **Response sizes** stay at a few KB at the median. The largest are `get_file_summary` of a file with hundreds of declarations (184 KB for Kubernetes' largest file) and `get_dependency_graph` of the most-called functions (up to 60 KB). Both are on the [roadmap](#roadmap) to be capped like the other tools.

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
- [x] **v0.7 (in progress):** workspaces of several repositories, `get_service_links` for HTTP routes, gRPC methods, Kafka topics, and Temporal workflows and activities; `.proto` files
- [ ] Next: cap `get_file_summary` and `get_dependency_graph` responses on very large files and hubs; rebuild the call graph incrementally after an edit
- [ ] Later: TypeScript and Rust, optional semantic search with local embeddings

## License

MIT
