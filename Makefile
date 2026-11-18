# SQLite FTS5 is compiled into go-sqlite3 only with this build tag.
TAGS := sqlite_fts5
export CGO_ENABLED := 1

.PHONY: build test vet bench clean

build:
	go build -tags $(TAGS) -o bin/codewiki ./cmd/codewiki

test:
	go test -tags $(TAGS) ./...

vet:
	go vet -tags $(TAGS) ./...

# Benchmarks on the pinned corpus in scripts/bench/corpus.json (clones the
# repositories into $(BENCH_CACHE) once; takes a while).
BENCH_CACHE ?= $(or $(TMPDIR),/tmp)/codewiki-bench
bench:
	go run -tags $(TAGS) ./scripts/bench -cache $(BENCH_CACHE) -out bench-results.json -md bench-results.md

clean:
	rm -rf bin .codewiki
