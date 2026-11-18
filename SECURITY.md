# Security policy

## Reporting a vulnerability

Please report vulnerabilities privately through GitHub: on the repository's **Security** tab, choose **Report a vulnerability**. Do not open a public issue.

Include what is affected (the CLI, `codeweft server`, the container image, the npm package), the version, and how to reproduce it. You will get a first answer within a week. Once a fix is released, the advisory is published with credit to you, unless you prefer otherwise.

## Supported versions

Fixes go into the latest release. Until 1.0, there are no backports to older versions.

## What codeweft does with your code

- It only **parses** source files. It never builds, runs or imports code from the repositories it indexes.
- The local server (`codeweft serve`) talks to its MCP client over stdio and opens no network port.
- The self-hosted server (`codeweft server`) requires bearer tokens on `/mcp`, `/status` and `/metrics` and refuses to start without them, unless `CODEWEFT_ALLOW_UNAUTHENTICATED=true` is set explicitly. GitHub webhook deliveries are checked against the webhook secret (HMAC-SHA256). The GitHub token is handed to Git through the environment as an HTTP header: it is never written into a URL, a clone's configuration or a log. Serve it over TLS (a reverse proxy or ingress) when it is reachable beyond localhost.
- Anyone holding an API token can read all the indexed code of that server. Use separate servers for repositories with different audiences.
