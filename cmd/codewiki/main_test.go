package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/anjanikumarjangid/codewiki/internal/store"
)

func writeFile(t *testing.T, path, src string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
}

// indexed reports whether a symbol with this qualified name is in the index.
func indexed(t *testing.T, db, qualified string) bool {
	t.Helper()
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	syms, err := st.Lookup(qualified)
	if err != nil {
		t.Fatal(err)
	}
	return len(syms) > 0
}

// TestReindexKeepsIndexOptions is the regression test for incremental runs
// forgetting the options the index was built with: `reindex` without flags
// used to drop test files from an index built with --include-tests, and
// re-add directories excluded with --exclude.
func TestReindexKeepsIndexOptions(t *testing.T) {
	repo := t.TempDir()
	writeFile(t, filepath.Join(repo, "go.mod"), "module example.com/m\n\ngo 1.24\n")
	writeFile(t, filepath.Join(repo, "a.go"), "package m\n\nfunc A() {}\n")
	writeFile(t, filepath.Join(repo, "a_test.go"), "package m\n\nfunc helperForTests() {}\n")
	writeFile(t, filepath.Join(repo, "gen", "g.go"), "package gen\n\nfunc G() {}\n")
	db := filepath.Join(t.TempDir(), "index.db")

	if err := runIndex([]string{"--include-tests", "--exclude", "gen", "--db", db, repo}); err != nil {
		t.Fatal(err)
	}
	if !indexed(t, db, "helperForTests") || indexed(t, db, "gen.G") {
		t.Fatal("index did not apply --include-tests and --exclude")
	}

	// No flags: the stored options apply.
	writeFile(t, filepath.Join(repo, "a.go"), "package m\n\nfunc A() {}\n\nfunc B() {}\n")
	if err := runReindex([]string{"--db", db}); err != nil {
		t.Fatal(err)
	}
	if !indexed(t, db, "m.B") {
		t.Fatal("reindex did not pick up the change")
	}
	if !indexed(t, db, "helperForTests") {
		t.Error("reindex without flags dropped test files")
	}
	if indexed(t, db, "gen.G") {
		t.Error("reindex without flags indexed an excluded directory")
	}

	// An explicit flag overrides the stored value, and is remembered.
	if err := runReindex([]string{"--db", db, "--include-tests=false"}); err != nil {
		t.Fatal(err)
	}
	if indexed(t, db, "helperForTests") {
		t.Error("--include-tests=false did not remove test files")
	}
	if err := runReindex([]string{"--db", db}); err != nil {
		t.Fatal(err)
	}
	if indexed(t, db, "helperForTests") || indexed(t, db, "gen.G") {
		t.Error("overridden options were not remembered by the next reindex")
	}
}
