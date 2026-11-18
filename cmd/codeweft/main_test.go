package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anjanikumarjangid/codeweft/internal/store"
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

func newRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(repo, "go.mod"), "module example.com/m\n\ngo 1.24\n")
	writeFile(t, filepath.Join(repo, "a.go"), "package m\n\nfunc A() {}\n")
	writeFile(t, filepath.Join(repo, "sub", "deep", "b.go"), "package deep\n\nfunc B() {}\n")
	return repo
}

func TestResolveServeFindsRepositoryRoot(t *testing.T) {
	repo := newRepo(t)
	cfg, err := resolveServe("", filepath.Join(repo, "sub", "deep"))
	if err != nil {
		t.Fatal(err)
	}
	// An explicit --root is used as given, not walked up from.
	if cfg.root != filepath.Join(repo, "sub", "deep") {
		t.Errorf("explicit --root: got %s", cfg.root)
	}

	t.Chdir(filepath.Join(repo, "sub", "deep"))
	cfg, err = resolveServe("", "")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.root != repo || cfg.db != filepath.Join(repo, ".codeweft", "index.db") || cfg.exists {
		t.Errorf("from a subdirectory: got %+v; want root %s with no index yet", cfg, repo)
	}

	// --db without an index needs to know what to build.
	if _, err := resolveServe(filepath.Join(t.TempDir(), "x.db"), ""); err == nil {
		t.Error("--db pointing at no index and no --root: want an error")
	}
}

// waitFor polls cond for up to five seconds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestServeBuildsIndexOnFirstStart(t *testing.T) {
	repo := newRepo(t)
	cfg, err := resolveServe("", repo)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ready := startIndexing(ctx, st, cfg, false, func(string) {})
	if err := ready(); err == nil || !strings.Contains(err.Error(), "building the index") {
		t.Errorf("right after start: ready() = %v; want a 'building the index' error", err)
	}
	waitFor(t, "the first index", func() bool { return ready() == nil })
	if !indexed(t, cfg.db, "sub/deep.B") {
		t.Error("first build did not index the repository")
	}
	data, err := os.ReadFile(filepath.Join(repo, ".codeweft", ".gitignore"))
	if err != nil || !strings.Contains(string(data), "*") {
		t.Errorf(".codeweft/.gitignore = %q, %v; want it to ignore everything", data, err)
	}
}

func TestServeCatchesUpAndWatches(t *testing.T) {
	repo := newRepo(t)
	db := filepath.Join(repo, defaultDB)
	if err := runIndex([]string{repo}); err != nil {
		t.Fatal(err)
	}
	// Edited while no server was running.
	writeFile(t, filepath.Join(repo, "c.go"), "package m\n\nfunc C() {}\n")

	cfg, err := resolveServe("", repo)
	if err != nil || !cfg.exists {
		t.Fatalf("resolveServe: %+v, %v", cfg, err)
	}
	st, err := store.Open(cfg.db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := startIndexing(ctx, st, cfg, true, func(string) {})
	if err := ready(); err != nil {
		t.Errorf("existing index: ready() = %v; want it served right away", err)
	}
	waitFor(t, "the catch-up re-index", func() bool { return indexed(t, db, "m.C") })

	// And edits after start are picked up by the watcher.
	writeFile(t, filepath.Join(repo, "d.go"), "package m\n\nfunc D() {}\n")
	waitFor(t, "the watcher", func() bool { return indexed(t, db, "m.D") })
}

// TestLinksWorkspace links a client call in one repository to the route of
// another, through a workspace file with paths relative to it.
func TestLinksWorkspace(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "orders", "go.mod"), "module example.com/orders\n\ngo 1.24\n")
	writeFile(t, filepath.Join(dir, "orders", "main.go"), `package main

import "net/http"

func main() { http.HandleFunc("POST /orders", createOrder) }

func createOrder(w http.ResponseWriter, r *http.Request) {}
`)
	writeFile(t, filepath.Join(dir, "web", "go.mod"), "module example.com/web\n\ngo 1.24\n")
	writeFile(t, filepath.Join(dir, "web", "main.go"), `package main

import "net/http"

func main() { http.Post("http://orders/orders", "application/json", nil) }
`)
	for _, r := range []string{"orders", "web"} {
		if err := runIndex([]string{filepath.Join(dir, r)}); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(dir, "ws", "workspace.json"), `{"repos": [{"path": "../orders"}, {"name": "frontend", "path": "../web"}]}`)

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = w
	err = runLinks([]string{"--workspace", filepath.Join(dir, "ws", "workspace.json")})
	os.Stdout = stdout
	w.Close()
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(r)
	if want := "[orders] main.createOrder  <-  [frontend] main.main (exact)"; !strings.Contains(string(out), want) {
		t.Errorf("links --workspace: missing %q in:\n%s", want, out)
	}
}
