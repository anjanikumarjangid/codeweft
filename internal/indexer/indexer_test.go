package indexer_test

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	// 5 Go, 5 Java and 4 Python files; the generated Go file is skipped and
	// the _test.go file ignored.
	if res.Files != 14 || res.Skipped != 1 {
		t.Fatalf("got %d files, %d skipped; want 14 and 1", res.Files, res.Skipped)
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
	files, syms, _, err := st.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if files != 14 || syms != 55 {
		t.Fatalf("after re-index: %d files, %d symbols; want 14 and 55", files, syms)
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
		{"refun", "", "payments.Gateway.Refund"},
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

// TestCallGraphGolden checks every resolved and external edge in the sample
// repo against a hand-reviewed expectation.
func TestCallGraphGolden(t *testing.T) {
	st := indexSample(t)
	syms, err := st.All()
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, s := range syms {
		callees, err := st.Callees(s.ID, true)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range callees {
			lines = append(lines, fmt.Sprintf("%s -> %s [%s] line %d", s.Qualified, c.Qualified, c.Confidence, c.CallLine))
		}
	}
	got := strings.Join(lines, "\n") + "\n"
	golden := "../../testdata/sample.callgraph.golden"
	if *update {
		if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	if got != string(want) {
		t.Errorf("call graph differs from %s:\n%s", golden, got)
	}
}

func TestCallersAndGraph(t *testing.T) {
	st := indexSample(t)
	target, err := st.Lookup("payments.PaymentService.ProcessPayment")
	if err != nil || len(target) != 1 {
		t.Fatalf("lookup: %v %v", target, err)
	}
	callers, err := st.Callers(target[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(callers) != 1 || callers[0].Qualified != "checkout.Checkout" {
		t.Fatalf("callers of ProcessPayment = %+v, want checkout.Checkout", callers)
	}

	// Checkout -> ProcessPayment -> Gateway.Charge, two levels deep.
	root, err := st.Lookup("checkout.Checkout")
	if err != nil || len(root) != 1 {
		t.Fatal(err)
	}
	g, n, err := st.Graph(root[0], "callees", 2, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("empty graph")
	}
	found := false
	for _, c := range g.Children {
		if c.Qualified == "payments.PaymentService.ProcessPayment" {
			for _, cc := range c.Children {
				if cc.Qualified == "payments.Gateway.Charge" {
					found = true
				}
			}
		}
	}
	if !found {
		t.Errorf("depth-2 graph missing Checkout -> ProcessPayment -> Gateway.Charge")
	}

	ext, err := st.ExternalCallers("sync", 10)
	if err != nil || len(ext) == 0 || ext[0].Qualified != "ratelimit.TokenBucket.Allow" {
		t.Errorf("ExternalCallers(sync) = %+v, %v", ext, err)
	}
}

// copyTree copies the fixture repo so a test can modify it.
func copyTree(t *testing.T, src string) string {
	t.Helper()
	dst := t.TempDir()
	err := filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	return dst
}

func TestReindexIncremental(t *testing.T) {
	repo := copyTree(t, "../../testdata/sample")
	st, err := store.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := indexer.Index(repo, st, indexer.Options{}); err != nil {
		t.Fatal(err)
	}

	// No changes: nothing is re-parsed and the graph is identical.
	res, err := indexer.Reindex(repo, st, indexer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Files != 0 || res.Unchanged != 14 || res.Removed != 0 {
		t.Fatalf("no-op reindex: %+v", res)
	}

	// Change one file (rename a method), delete one, add one.
	svc := filepath.Join(repo, "payments", "service.go")
	data, _ := os.ReadFile(svc)
	data = []byte(strings.ReplaceAll(string(data), "ProcessPayment", "Pay"))
	if err := os.WriteFile(svc, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(repo, "ratelimit", "limiter.go")); err != nil {
		t.Fatal(err)
	}
	extra := "package payments\n\n// Refund reverses a charge.\nfunc (s *PaymentService) Refund(id string) error { return s.gw.Refund(id) }\n"
	if err := os.WriteFile(filepath.Join(repo, "payments", "refund.go"), []byte(extra), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err = indexer.Reindex(repo, st, indexer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Files != 2 || res.Unchanged != 12 || res.Removed != 1 {
		t.Fatalf("reindex after changes: %+v", res)
	}
	if syms, _ := st.Lookup("ratelimit.TokenBucket"); len(syms) != 0 {
		t.Error("symbols of a deleted file are still indexed")
	}
	if hits, _ := st.Search("ProcessPayment", store.SearchOptions{}); len(hits) != 0 {
		t.Errorf("renamed method still searchable: %v", hits[0].Qualified)
	}
	pay, err := st.Lookup("payments.PaymentService.Pay")
	if err != nil || len(pay) != 1 {
		t.Fatalf("lookup Pay: %v %v", pay, err)
	}
	// The whole graph is rebuilt from stored call sites, so edges stay
	// consistent: Gateway.Charge is now called from Pay, and checkout.go
	// (unchanged, still calling the old name) no longer links to anything.
	charge, _ := st.Lookup("payments.Gateway.Charge")
	callers, err := st.Callers(charge[0].ID)
	if err != nil || len(callers) != 1 || callers[0].Qualified != "payments.PaymentService.Pay" {
		t.Errorf("callers of Gateway.Charge = %+v, %v", callers, err)
	}
	if callers, _ := st.Callers(pay[0].ID); len(callers) != 0 {
		t.Errorf("stale call site resolved to renamed method: %+v", callers)
	}
	refund, err := st.Lookup("payments.PaymentService.Refund")
	if err != nil || len(refund) != 1 {
		t.Fatalf("new file not indexed: %v %v", refund, err)
	}
	callees, err := st.Callees(refund[0].ID, false)
	if err != nil || len(callees) != 1 || callees[0].Qualified != "payments.Gateway.Refund" {
		t.Errorf("Refund callees = %+v, %v", callees, err)
	}
}

func TestWatchPicksUpChanges(t *testing.T) {
	repo := copyTree(t, "../../testdata/sample")
	st, err := store.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := indexer.Index(repo, st, indexer.Options{}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan indexer.Result, 1)
	go indexer.Watch(ctx, repo, st, indexer.Options{}, 20*time.Millisecond, func(r indexer.Result, err error) {
		if err == nil {
			select {
			case done <- r:
			default:
			}
		}
	})
	time.Sleep(50 * time.Millisecond)
	src := "package payments\n\n// Audit records a charge.\nfunc Audit(id string) {}\n"
	if err := os.WriteFile(filepath.Join(repo, "payments", "audit.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-done:
		if r.Files != 1 {
			t.Errorf("watch re-indexed %d files, want 1", r.Files)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("watch did not re-index the new file")
	}
	if syms, _ := st.Lookup("payments.Audit"); len(syms) != 1 {
		t.Error("new symbol not searchable after watch re-index")
	}
}

func TestEndpointsAndOverview(t *testing.T) {
	st := indexSample(t)
	eps, err := st.ListEndpoints(store.EndpointFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range eps {
		got = append(got, fmt.Sprintf("%s %s -> %s [%s] %s", e.Method, e.Path, e.Handler, e.Framework, e.HandlerLocation))
	}
	want := []string{
		"POST /api/v1/checkout -> checkout.handleCheckout [net/http] checkout/http.go:23",
		"GET /api/v1/orders/{id} -> checkout.Handler.getOrder [net/http] checkout/http.go:27",
		"GET /inventory/stock/{sku} -> py.inventory.api.get_stock [fastapi] py/inventory/api.py:9",
		"POST /inventory/stock/{sku}/reserve -> py.inventory.api.reserve [fastapi] py/inventory/api.py:15",
		"POST /orders -> com.shop.orders.OrderController.place [spring] java/com/shop/orders/OrderController.java:19",
		"GET /orders/{id} -> com.shop.orders.OrderController.get [spring] java/com/shop/orders/OrderController.java:25",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("endpoints:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	o, err := st.BuildOverview(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(o.Languages) != 3 || o.Endpoints != 6 {
		t.Errorf("overview languages=%v endpoints=%d", o.Languages, o.Endpoints)
	}
	entries := map[string]bool{}
	for _, e := range o.EntryPoints {
		entries[e.Qualified] = true
	}
	if !entries["checkout/cmd/server/main.main"] || !entries["py.inventory.api.__main__"] {
		t.Errorf("entry points = %+v", o.EntryPoints)
	}
}
