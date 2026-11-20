# codeweft

MCP server that gives AI coding agents structured, deterministic context about your code and the services calling each other: symbols, call graphs, type hierarchies, tests, HTTP routes, gRPC, Kafka, RabbitMQ, SQS, Pub/Sub and Temporal links, for Go, Java, Python, TypeScript and JavaScript.

```bash
npx -y codeweft serve            # from inside a repository: index it and serve over stdio
claude mcp add codeweft -- npx -y codeweft serve
```

This package runs the prebuilt binary for your platform (Linux x64/arm64, macOS arm64/x64, Windows x64), installed from the matching `@codeweft/<os>-<cpu>` package. Documentation: https://github.com/anjanikumarjangid/codeweft
