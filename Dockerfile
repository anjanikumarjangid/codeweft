# syntax=docker/dockerfile:1

# codeweft server image. Build:  docker build -t codeweft .
# Run:    docker run -p 8080:8080 -v codeweft-data:/data \
#           -e CODEWEFT_REPOS="acme/orders acme/payments" \
#           -e CODEWEFT_API_TOKENS=... -e GITHUB_TOKEN=... codeweft

# Built on the build machine's platform and cross-compiled for the
# target: cgo (SQLite, tree-sitter) under emulation would be very slow.
FROM --platform=$BUILDPLATFORM golang:1.24-bookworm AS build
ARG TARGETARCH
RUN if [ "$TARGETARCH" = "arm64" ] && [ "$(dpkg --print-architecture)" != "arm64" ]; then \
      apt-get update && apt-get install -y --no-install-recommends gcc-aarch64-linux-gnu libc6-dev-arm64-cross && rm -rf /var/lib/apt/lists/*; \
    fi
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN set -e; \
    if [ "$TARGETARCH" = "arm64" ] && [ "$(dpkg --print-architecture)" != "arm64" ]; then export CC=aarch64-linux-gnu-gcc; fi; \
    CGO_ENABLED=1 GOOS=linux GOARCH="$TARGETARCH" go build -trimpath \
      -tags "sqlite_fts5 sqlite_omit_load_extension netgo osusergo" \
      -ldflags "-s -w -X github.com/anjanikumarjangid/codeweft/internal/server.Version=${VERSION} -linkmode external -extldflags -static" \
      -o /out/codeweft ./cmd/codeweft

FROM alpine:3.20
# git clones and fetches the repositories; tini reaps git's processes and
# forwards signals, so the server shuts down cleanly.
RUN apk add --no-cache git ca-certificates tini \
 && adduser -D -H -u 10001 codeweft \
 && mkdir /data && chown codeweft /data
COPY --from=build /out/codeweft /usr/local/bin/codeweft
USER codeweft
ENV CODEWEFT_DATA_DIR=/data CODEWEFT_ADDR=:8080
VOLUME /data
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=3s --start-period=10s CMD wget -qO- http://127.0.0.1:8080/healthz >/dev/null || exit 1
ENTRYPOINT ["/sbin/tini", "--", "codeweft"]
CMD ["server"]
