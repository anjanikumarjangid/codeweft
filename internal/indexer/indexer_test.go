package indexer_test

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/anjanikumarjangid/codewiki/internal/indexer"
	"github.com/anjanikumarjangid/codewiki/internal/store"
)

var update = flag.Bool("update", false, "rewrite golden files")

func indexSample(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	res, err := indexer.Index("../../testdata/sample", st, indexer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	// 2 source files; the generated file is skipped, the _test.go file ignored.
	if res.Files != 2 || res.Skipped != 1 {
		t.Fatalf("got %d files, %d skipped; want 2 and 1", res.Files, res.Skipped)
	}
	return st
}

// TestIndexGolden compares every extracted symbol, without bodies, against a
// checked-in expectation. Run `go test ./... -update` after intended changes.
func TestIndexGolden(t *testing.T) {
	st := indexSample(t)
	syms, err := st.All()
	if err != nil {
		t.Fatal(err)
	}
	for i := range syms {
		syms[i].Body = ""
	}
	got, err := json.MarshalIndent(syms, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	golden := "../../testdata/sample.golden.json"
	if *update {
		if err := os.WriteFile(golden, append(got, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	if string(append(got, '\n')) != string(want) {
		t.Errorf("index differs from %s; run `go test ./internal/indexer -update` and review the diff.\ngot:\n%s", golden, got)
	}
}

func TestReindexReplacesData(t *testing.T) {
	st := indexSample(t)
	if _, err := indexer.Index("../../testdata/sample", st, indexer.Options{}); err != nil {
		t.Fatal(err)
	}
	files, syms, err := st.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if files != 2 || syms != 8 {
		t.Fatalf("after re-index: %d files, %d symbols; want 2 and 8", files, syms)
	}
	hits, err := st.Search("payment", store.SearchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, h := range hits {
		seen[h.Qualified]++
	}
	for q, n := range seen {
		if n > 1 {
			t.Errorf("%s returned %d times after re-index", q, n)
		}
	}
}

func TestSearch(t *testing.T) {
	st := indexSample(t)
	cases := []struct {
		query, kind, wantFirst string
	}{
		// camelCase words are searchable on their own
		{"payment", "", "payments.PaymentService"},
		{"process payment", "", "payments.PaymentService.ProcessPayment"},
		// prefix matching
		{"refun", "", "payments.Gateway"},
		// doc comments are searchable
		{"rate limiter", "", "ratelimit.TokenBucket"},
		// kind filter
		{"payment", "method", "payments.PaymentService.ProcessPayment"},
		// FTS5 syntax in user input must not break the query
		{`"payment*)(`, "", "payments.PaymentService"},
	}
	for _, c := range cases {
		hits, err := st.Search(c.query, store.SearchOptions{Kind: c.kind})
		if err != nil {
			t.Errorf("Search(%q): %v", c.query, err)
			continue
		}
		if len(hits) == 0 {
			t.Errorf("Search(%q): no results", c.query)
			continue
		}
		if hits[0].Qualified != c.wantFirst {
			t.Errorf("Search(%q, kind=%q) first = %s, want %s", c.query, c.kind, hits[0].Qualified, c.wantFirst)
		}
	}
	// Operator-looking words are treated as plain terms, never as syntax.
	if _, err := st.Search(`NEAR(payment OR "x" AND*`, store.SearchOptions{}); err != nil {
		t.Errorf("operator-looking query failed: %v", err)
	}
	if hits, _ := st.Search("   ", store.SearchOptions{}); len(hits) != 0 {
		t.Errorf("blank query returned %d hits", len(hits))
	}
}

func TestLookup(t *testing.T) {
	st := indexSample(t)
	for name, want := range map[string]string{
		"payments.PaymentService.ProcessPayment": "payments.PaymentService.ProcessPayment",
		"ProcessPayment":                         "payments.PaymentService.ProcessPayment",
		"TokenBucket.Allow":                      "ratelimit.TokenBucket.Allow",
	} {
		syms, err := st.Lookup(name)
		if err != nil || len(syms) != 1 || syms[0].Qualified != want {
			t.Errorf("Lookup(%q) = %v, %v; want %s", name, syms, err, want)
			continue
		}
		if syms[0].Body == "" || syms[0].StartLine == 0 {
			t.Errorf("Lookup(%q) missing body or position", name)
		}
	}
	if syms, _ := st.Lookup("DoesNotExist"); len(syms) != 0 {
		t.Errorf("unknown name matched %d symbols", len(syms))
	}
}
