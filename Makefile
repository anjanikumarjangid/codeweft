# SQLite FTS5 is compiled into go-sqlite3 only with this build tag.
TAGS := sqlite_fts5
export CGO_ENABLED := 1

.PHONY: build test vet clean

build:
	go build -tags $(TAGS) -o bin/codewiki ./cmd/codewiki

test:
	go test -tags $(TAGS) ./...

vet:
	go vet -tags $(TAGS) ./...

clean:
	rm -rf bin .codewiki
