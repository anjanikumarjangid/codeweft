# Changelog

All notable changes are listed here. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow [Semantic Versioning](https://semver.org/).

## [0.8.2] - 2026-11-20

### Fixed

- **npm**: the `codeweft` package of 0.8.1 was built without its launcher (`bin/codeweft.js`), which an ignore rule had kept out of the repository, so `npx codeweft` had nothing to run. The launcher is in the repository now, the publish script refuses a package missing a file it lists, and CI checks the launcher is packed.

## [0.8.1] - 2026-11-20

### Fixed

- **npm on Windows**: npm refused the name of the Windows package, so 0.8.0 was not published to npm for any platform. The per-platform packages are now published under the `@codeweft` scope (`@codeweft/linux-x64`, `@codeweft/win32-x64`, ...); `npx codeweft` is unchanged.
- Releases on npm retry a failed publish and skip packages already published, so a re-run finishes a partial release; the *npm publish* workflow republishes an existing release by hand.

## [0.8.0] - 2026-11-20

The first release. codeweft was developed as *codewiki* until this release.

### Added

- **Indexing** of Go (`go/ast`), Java, Python and Protocol Buffers (tree-sitter): symbols, imports, call sites, routes and service contracts, in a local SQLite + FTS5 index. Go files are those of one build configuration (`--platform`).
- **TypeScript and JavaScript**: modules, functions, classes, interfaces, imports and `require`, the call graph and type hierarchy, Express and NestJS routes, `fetch` and axios calls, kafkajs, amqplib, SQS and Pub/Sub links, and `it`/`test` tests.
- **Call graph** resolved from declarations without a type checker: expression typing for Go, Java and Python, method sets, overloads by arity; each edge marked `exact` or `name`. Measured against `go/types`, javac and jedi (see the README's Accuracy section).
- **Type hierarchy**: Go method sets, Java and Python declared supertypes, interface method implementations.
- **Tests**: which tests reach a function, through the code and test helpers.
- **MCP tools** over stdio: `search_code`, `get_symbol`, `get_dependency_graph`, `get_implementations`, `find_tests`, `get_file_summary`, `get_architecture_overview`, `list_apis`, `get_service_links`. Every answer is capped and says what it left out.
- **Service links** (`get_service_links`, `codeweft links`): HTTP routes and the client calls reaching them; gRPC methods and their callers, named from `.proto` files; Kafka topics, from producers to consumers; Temporal workflows and activities, with task queues. Across Go, Java and Python.
- **RabbitMQ, SQS and Pub/Sub links**: producers to consumers in Go, Java and Python, through RabbitMQ bindings (with topic patterns) and Pub/Sub subscriptions declared in code.
- **Configuration values**: topics, URLs, routes and queues set in Spring `application*.yml`/`.properties` files, `.env` files or environment variables (`os.Getenv`, `os.getenv`, `System.getenv`) are resolved, so services configured with the same value link. Secret-looking keys are never read.
- **Workspaces**: one server over several repositories (`serve --workspace`), with links across them.
- **Self-hosted server** (`codeweft server`): clones repositories (or a GitHub organization), keeps them indexed from GitHub push webhooks and periodic syncs, and serves them over MCP Streamable HTTP with bearer tokens; `/status`, `/metrics`, `/healthz`, `/readyz`.
- **PostgreSQL** as an optional index store for the server (`CODEWEFT_DATABASE_URL`): one schema per repository, created and migrated automatically; replicas share the indexes and take turns indexing. SQLite stays the default. The test suite runs against PostgreSQL with `CODEWEFT_TEST_POSTGRES`.
- A restarted server serves the indexes it already has right away, and skips re-indexing a commit it has already indexed.
- **Sign-in with GitHub** for the server (`CODEWEFT_GITHUB_CLIENT_ID`, `CODEWEFT_GITHUB_CLIENT_SECRET`, `CODEWEFT_PUBLIC_URL`): MCP clients sign people in through the MCP authorization flow (dynamic client registration, PKCE, a confirmation page), and each person sees only the repositories their GitHub account can read. GitHub tokens are also accepted as bearer tokens. No sessions are stored.
- **Incremental re-indexing**: only changed files are parsed; an edit that keeps a file's declarations only resolves that file's calls again (about half a second on the Go standard library, 1.3 s on Kubernetes).
- **Distribution**: release binaries for Linux, macOS and Windows; container image on ghcr.io; Docker Compose and Helm; `npx codeweft`; Homebrew.
- **Benchmarks** (`make bench`) on a pinned corpus, and accuracy tools in `scripts/accuracy`.

### Changed

- Licensed under the Apache License 2.0 (commits before the switch were MIT).

[0.8.2]: https://github.com/anjanikumarjangid/codeweft/releases/tag/v0.8.2
[0.8.1]: https://github.com/anjanikumarjangid/codeweft/releases/tag/v0.8.1
[0.8.0]: https://github.com/anjanikumarjangid/codeweft/releases/tag/v0.8.0
