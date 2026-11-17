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
	// 6 Go (one of them generated), 5 Java and 4 Python files; test files
	// are kept apart and not counted.
	if res.Files != 15 || res.Generated != 1 || res.Skipped != 0 {
		t.Fatalf("got %d files (%d generated), %d skipped; want 15 (1), 0", res.Files, res.Generated, res.Skipped)
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
	if files != 15 || syms != 90 {
		t.Fatalf("after re-index: %d files, %d symbols; want 15 and 90", files, syms)
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

// TestTypeHierarchyGolden compares every direct supertype relation in the
// fixture against a checked-in expectation.
func TestTypeHierarchyGolden(t *testing.T) {
	st := indexSample(t)
	syms, err := st.All()
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, s := range syms {
		sups, err := st.Supertypes(s.ID, 1)
		if err != nil {
			t.Fatal(err)
		}
		for _, u := range sups {
			ext := ""
			if u.External {
				ext = " (external)"
			}
			lines = append(lines, fmt.Sprintf("%s -> %s [%s]%s", s.Qualified, u.Qualified, u.How, ext))
		}
	}
	got := strings.Join(lines, "\n") + "\n"
	golden := "../../testdata/sample.types.golden"
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
		t.Errorf("type hierarchy differs from %s:\n%s", golden, got)
	}
}

// TestImplementations spells out the cases the type hierarchy must get
// right, independently of the golden file.
func TestImplementations(t *testing.T) {
	st := indexSample(t)
	one := func(name string) store.Symbol {
		t.Helper()
		syms, err := st.Lookup(name)
		if err != nil || len(syms) != 1 {
			t.Fatalf("lookup %s: %v %v", name, syms, err)
		}
		return syms[0]
	}
	names := func(links []store.TypeLink, err error) string {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, l := range links {
			out = append(out, fmt.Sprintf("%s[%s,%d]", l.Qualified, l.How, l.Depth))
		}
		return strings.Join(out, " ")
	}
	for _, c := range []struct{ name, got, want string }{
		// Go: by method set. LoggingGateway gets its methods from the embedded
		// *StripeGateway; FakeGateway.Refund() has the wrong parameters; the
		// interface that embeds Gateway extends it.
		{"Gateway implementations", names(st.Subtypes(one("payments.Gateway").ID, 5)),
			"checkout.wrappedGateway[method set,1] payments.AuditedGateway[extends,1] payments.DoubleGateway[method set,1] payments.LoggingGateway[method set,1] payments.StripeGateway[method set,1] payments.VoidingGateway[extends,1]"},
		// DoubleGateway's Void is ambiguous (two embeddings at depth 1).
		{"VoidingGateway implementations", names(st.Subtypes(one("payments.VoidingGateway").ID, 5)),
			"checkout.wrappedGateway[method set,1] payments.AuditedGateway[extends,1] payments.LoggingGateway[method set,1] payments.StripeGateway[method set,1]"},
		// Overlapping embedded interfaces: the method set is their union.
		{"AuditedGateway implementations", names(st.Subtypes(one("payments.AuditedGateway").ID, 5)),
			"checkout.wrappedGateway[method set,1] payments.LoggingGateway[method set,1] payments.StripeGateway[method set,1]"},
		{"embedding is not subtyping", names(st.Subtypes(one("payments.StripeGateway").ID, 5)), ""},
		{"LoggingGateway supertypes", names(st.Supertypes(one("payments.LoggingGateway").ID, 1)),
			"payments.AuditedGateway[method set,1] payments.Gateway[method set,1] payments.StripeGateway[embeds,1] payments.VoidingGateway[method set,1] payments.settler[method set,1]"},
		// An unexported method is only the interface's own when declared in
		// its package: checkout.localSettler's settle is not, while
		// checkout.wrappedGateway gets payments' settle by embedding.
		{"settler implementations", names(st.Subtypes(one("payments.settler").ID, 5)),
			"checkout.wrappedGateway[method set,1] payments.DoubleGateway[method set,1] payments.LoggingGateway[method set,1] payments.StripeGateway[method set,1]"},
		// The predeclared error interface contributes Error() string.
		{"CodedError implementations", names(st.Subtypes(one("payments.CodedError").ID, 5)),
			"payments.DeclineError[method set,1]"},
		// Java: declared, followed through the class hierarchy.
		{"OrderRepository implementations", names(st.Subtypes(one("com.shop.orders.OrderRepository").ID, 5)),
			"com.shop.orders.InMemoryOrderRepository[implements,1] com.shop.orders.CountingOrderRepository[extends,2]"},
		{"CountingOrderRepository supertypes", names(st.Supertypes(one("com.shop.orders.CountingOrderRepository").ID, 5)),
			"com.shop.orders.InMemoryOrderRepository[extends,1] com.shop.orders.OrderRepository[implements,2] java.io.Serializable[implements,1]"},
		{"implementers of an external interface", names(st.ExternalSubtypes("Serializable")),
			"com.shop.orders.CountingOrderRepository[implements,1]"},
		// Python: base classes.
		{"ItemRepository subclasses", names(st.Subtypes(one("py.inventory.models.ItemRepository").ID, 5)),
			"py.inventory.models.CachedItemRepository[extends,1]"},
		// Methods: implementations and overrides, and the reverse.
		{"Gateway.Charge implementations", names(st.MethodImplementations(one("payments.Gateway.Charge"), 5)),
			"payments.StripeGateway.Charge[implements,1]"},
		{"OrderRepository.save implementations", names(st.MethodImplementations(one("com.shop.orders.OrderRepository.save"), 5)),
			"com.shop.orders.InMemoryOrderRepository.save[implements,1] com.shop.orders.CountingOrderRepository.save[implements,2]"},
		{"ItemRepository.get overrides", names(st.MethodImplementations(one("py.inventory.models.ItemRepository.get"), 5)),
			"py.inventory.models.CachedItemRepository.get[overrides,1]"},
		{"CountingOrderRepository.save supers", names(st.MethodSupers(one("com.shop.orders.CountingOrderRepository.save"), 5)),
			"com.shop.orders.InMemoryOrderRepository.save[overrides,1] com.shop.orders.OrderRepository.save[implements,2]"},
	} {
		if c.got != c.want {
			t.Errorf("%s:\n got: %s\nwant: %s", c.name, c.got, c.want)
		}
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
	// Checkout's call is exact (svc's type comes from NewPaymentService's
	// result); audit's goes through a function value, matched by name.
	if len(callers) != 2 || callers[0].Qualified != "checkout.Checkout" || callers[0].Confidence != "exact" ||
		callers[1].Qualified != "checkout.audit" || callers[1].Confidence != "name" {
		t.Fatalf("callers of ProcessPayment = %+v, want checkout.Checkout (exact) and checkout.audit (name)", callers)
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
	if res.Files != 0 || res.Unchanged != 18 || res.Removed != 0 {
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
	if res.Files != 2 || res.Unchanged != 16 || res.Removed != 1 {
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

// TestStoredOptionsKeepFileSet guards against incremental runs silently
// changing which files are indexed: an index built with --include-tests and
// --exclude must keep that file set when reindex or watch run later with
// the stored options.
func TestStoredOptionsKeepFileSet(t *testing.T) {
	repo := copyTree(t, "../../testdata/sample")
	st, err := store.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	if opts, err := indexer.StoredOptions(st); err != nil || opts.IncludeTests || len(opts.Exclude) != 0 {
		t.Fatalf("empty index: options %+v, %v; want zero options", opts, err)
	}
	want := indexer.Options{IncludeTests: true, Exclude: []string{"py"}}
	res, err := indexer.Index(repo, st, want)
	if err != nil {
		t.Fatal(err)
	}
	// 15 default files + 2 Go and Java test files - 4 Python files under
	// py/ (the Python test file is under py/ too).
	if res.Files != 13 {
		t.Fatalf("indexed %d files, want 13", res.Files)
	}
	got, err := indexer.StoredOptions(st)
	if err != nil {
		t.Fatal(err)
	}
	if !got.IncludeTests || len(got.Exclude) != 1 || got.Exclude[0] != "py" || got.MaxFileBytes != 0 {
		t.Fatalf("stored options = %+v, want %+v", got, want)
	}

	// Touch a file and re-index the way watch does: nothing may be removed
	// or added besides the changed file.
	svc := filepath.Join(repo, "payments", "service.go")
	data, _ := os.ReadFile(svc)
	if err := os.WriteFile(svc, append(data, "\n// edited\n"...), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err = indexer.Reindex(repo, st, got)
	if err != nil {
		t.Fatal(err)
	}
	if res.Files != 1 || res.Unchanged != 12 || res.Removed != 0 {
		t.Fatalf("reindex with stored options: %+v; want 1 changed, 12 unchanged, 0 removed", res)
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

// TestWatchCatchesUpOnStart checks that edits made while nothing was
// watching are indexed as soon as Watch starts, without waiting for another
// edit.
func TestWatchCatchesUpOnStart(t *testing.T) {
	repo := copyTree(t, "../../testdata/sample")
	st, err := store.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := indexer.Index(repo, st, indexer.Options{}); err != nil {
		t.Fatal(err)
	}
	src := "package payments\n\n// Audit records a charge.\nfunc Audit(id string) {}\n"
	if err := os.WriteFile(filepath.Join(repo, "payments", "audit.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan indexer.Result, 1)
	// A long interval: only the catch-up run can index the file in time.
	go indexer.Watch(ctx, repo, st, indexer.Options{}, time.Hour, func(r indexer.Result, err error) {
		if err == nil {
			done <- r
		}
	})
	select {
	case r := <-done:
		if r.Files != 1 {
			t.Errorf("catch-up re-indexed %d files, want 1", r.Files)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("watch did not catch up on an edit made before it started")
	}
	if syms, _ := st.Lookup("payments.Audit"); len(syms) != 1 {
		t.Error("file edited before watch started is not indexed")
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

// TestReindexUpdatesTypeHierarchy checks that the type hierarchy follows
// an incremental re-index: a changed signature removes an implementation,
// and relations declared in unchanged files stay.
func TestReindexUpdatesTypeHierarchy(t *testing.T) {
	repo := copyTree(t, "../../testdata/sample")
	st, err := store.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := indexer.Index(repo, st, indexer.Options{}); err != nil {
		t.Fatal(err)
	}
	implements := func(sub, super string) bool {
		t.Helper()
		syms, err := st.Lookup(super)
		if err != nil || len(syms) != 1 {
			t.Fatalf("lookup %s: %v %v", super, syms, err)
		}
		subs, err := st.Subtypes(syms[0].ID, 5)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range subs {
			if s.Qualified == sub {
				return true
			}
		}
		return false
	}
	if !implements("payments.StripeGateway", "payments.Gateway") {
		t.Fatal("StripeGateway should implement Gateway before the change")
	}

	svc := filepath.Join(repo, "payments", "service.go")
	data, _ := os.ReadFile(svc)
	changed := strings.Replace(string(data), "func (g *StripeGateway) Refund(chargeID string) error", "func (g *StripeGateway) Refund(chargeID int) error", 1)
	if changed == string(data) {
		t.Fatal("fixture changed: StripeGateway.Refund not found")
	}
	if err := os.WriteFile(svc, []byte(changed), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := indexer.Reindex(repo, st, indexer.Options{})
	if err != nil || res.Files != 1 {
		t.Fatalf("reindex: %+v, %v", res, err)
	}
	if implements("payments.StripeGateway", "payments.Gateway") {
		t.Error("StripeGateway.Refund(int) no longer matches Gateway, but the implementation is still listed")
	}
	if !implements("com.shop.orders.CountingOrderRepository", "com.shop.orders.OrderRepository") {
		t.Error("a relation declared in an unchanged file was lost")
	}
}

// TestTestsFor checks which tests are found to exercise a symbol: directly,
// through the repository's own callers, and through helpers in test files.
func TestTestsFor(t *testing.T) {
	st := indexSample(t)
	tests := func(name string) string {
		t.Helper()
		syms, err := st.Lookup(name)
		if err != nil || len(syms) == 0 {
			t.Fatalf("lookup %s: %v %v", name, syms, err)
		}
		refs, total, err := st.TestsFor(syms[0].ID, 3, 3, 50)
		if err != nil {
			t.Fatal(err)
		}
		if total != len(refs) {
			t.Errorf("%s: total %d, listed %d", name, total, len(refs))
		}
		var out []string
		for _, r := range refs {
			out = append(out, fmt.Sprintf("%s[%d via %s]", r.Qualified, r.Distance, r.Via))
		}
		return strings.Join(out, " ")
	}
	for _, c := range []struct{ name, want string }{
		// Direct, and through the test helper allowTwice.
		{"ratelimit.TokenBucket.Allow", "ratelimit.TestAllow[1 via ratelimit.TokenBucket.Allow] ratelimit.TestBurst[2 via ratelimit.TokenBucket.Allow]"},
		// Direct, and through Allow -> refillLocked -> refill in the code
		// under test. Testable is not a test.
		{"ratelimit.refill", "ratelimit.TestRefill[1 via ratelimit.refill] ratelimit.TestAllow[3 via ratelimit.TokenBucket.Allow] ratelimit.TestBurst[4 via ratelimit.TokenBucket.Allow]"},
		// Java: through OrderService.place, and through the newService helper
		// for the constructor.
		{"com.shop.billing.Billing.charge", "com.shop.orders.OrderServiceTest.placeCreatesAnOrder[2 via com.shop.orders.OrderService.place]"},
		{"com.shop.orders.OrderService.OrderService", "com.shop.orders.OrderServiceTest.placeCreatesAnOrder[2 via com.shop.orders.OrderService.OrderService]"},
		// Python: a module-level test and a test method in a class.
		{"py.inventory.stock.StockService.reserve", "py.inventory.test_stock.test_reserve_fails_without_stock[1 via py.inventory.stock.StockService.reserve]"},
		// reserve -> ItemRepository.get -> Item: the reserve test counts too.
		{"py.inventory.models.Item", "py.inventory.test_stock.TestItem.test_item_keeps_count[1 via py.inventory.models.Item] py.inventory.test_stock.test_reserve_fails_without_stock[3 via py.inventory.stock.StockService.reserve]"},
		{"payments.Gateway.Charge", ""},
	} {
		if got := tests(c.name); got != c.want {
			t.Errorf("tests for %s:\n got: %s\nwant: %s", c.name, got, c.want)
		}
	}
	// Test code stays out of search and lookup.
	if hits, _ := st.Search("allowTwice", store.SearchOptions{}); len(hits) != 0 {
		t.Errorf("test helper is searchable: %v", hits[0].Qualified)
	}
	if syms, _ := st.Lookup("TestAllow"); len(syms) != 0 {
		t.Error("test function is a symbol")
	}
}

// TestIncludeTestsSwitch checks that re-indexing with --include-tests makes
// unchanged test files searchable, and that switching back hides them.
func TestIncludeTestsSwitch(t *testing.T) {
	repo := copyTree(t, "../../testdata/sample")
	st, err := store.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	searchable := func() bool {
		hits, _ := st.Search("allowTwice", store.SearchOptions{})
		return len(hits) > 0
	}
	if _, err := indexer.Index(repo, st, indexer.Options{}); err != nil {
		t.Fatal(err)
	}
	if searchable() {
		t.Fatal("test helper searchable without --include-tests")
	}
	res, err := indexer.Reindex(repo, st, indexer.Options{IncludeTests: true})
	if err != nil || res.Files != 3 {
		t.Fatalf("switching to --include-tests should re-index the 3 test files: %+v %v", res, err)
	}
	if !searchable() {
		t.Error("test helper not searchable with --include-tests")
	}
	syms, _ := st.Lookup("ratelimit.TokenBucket.Allow")
	if refs, _, _ := st.TestsFor(syms[0].ID, 3, 3, 50); len(refs) != 2 {
		t.Errorf("tests for Allow with --include-tests: %+v", refs)
	}
	if _, err := indexer.Reindex(repo, st, indexer.Options{}); err != nil {
		t.Fatal(err)
	}
	if searchable() {
		t.Error("test helper still searchable after switching back")
	}
}

// TestPythonReExports checks that a call through a package that re-exports
// a name (requests.get, defined as requests.api.get and imported into
// requests/__init__.py) resolves to the definition, from code and tests.
func TestPythonReExports(t *testing.T) {
	repo := t.TempDir()
	write := func(rel, src string) {
		t.Helper()
		p := filepath.Join(repo, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("pkg/__init__.py", "from .api import get\n")
	write("pkg/api.py", "def get(url):\n    return request('GET', url)\n\n\ndef request(method, url):\n    return None\n")
	write("app.py", "import pkg\n\n\ndef main():\n    pkg.get('x')\n")
	write("tests/test_api.py", "import pkg\n\n\ndef test_get():\n    pkg.get('x')\n")
	st, err := store.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := indexer.Index(repo, st, indexer.Options{}); err != nil {
		t.Fatal(err)
	}
	main, _ := st.Lookup("app.main")
	callees, err := st.Callees(main[0].ID, false)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range callees {
		got = append(got, c.Qualified+"["+c.Confidence+"]")
	}
	if strings.Join(got, " ") != "pkg.api.get[exact]" {
		t.Errorf("app.main callees = %v; want the exact pkg.api.get", got)
	}
	req, _ := st.Lookup("pkg.api.request")
	tests, _, err := st.TestsFor(req[0].ID, 3, 3, 10)
	if err != nil || len(tests) != 1 || tests[0].Qualified != "tests.test_api.test_get" || tests[0].Distance != 2 {
		t.Errorf("tests for pkg.api.request = %+v, %v; want tests.test_api.test_get at distance 2", tests, err)
	}
}

// TestBuildOutputDirs checks that directories named like build output are
// skipped, unless they are Go packages (as Go's own go/build is), and that
// --exclude still skips them.
func TestBuildOutputDirs(t *testing.T) {
	repo := t.TempDir()
	write := func(rel, src string) {
		t.Helper()
		p := filepath.Join(repo, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module example.com/m\n\ngo 1.24\n")
	write("go/build/build.go", "package build\n\n// Import finds a package.\nfunc Import() {}\n")
	write("app/build/generated/Gen.java", "package gen;\n\nclass Gen {}\n")
	write("lib/dist/bundle.py", "def bundled():\n    pass\n")
	write("target/classes/A.java", "package a;\n\nclass A {}\n")

	index := func(opts indexer.Options) map[string]bool {
		t.Helper()
		st, err := store.Open(filepath.Join(t.TempDir(), "index.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		if _, err := indexer.Index(repo, st, opts); err != nil {
			t.Fatal(err)
		}
		all, err := st.All()
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]bool{}
		for _, s := range all {
			got[s.File] = true
		}
		return got
	}
	got := index(indexer.Options{})
	if !got["go/build/build.go"] {
		t.Error("a Go package in a directory named build was skipped")
	}
	for _, f := range []string{"app/build/generated/Gen.java", "lib/dist/bundle.py", "target/classes/A.java"} {
		if got[f] {
			t.Errorf("build output %s was indexed", f)
		}
	}
	if got := index(indexer.Options{Exclude: []string{"build"}}); got["go/build/build.go"] {
		t.Error("--exclude build should skip go/build too")
	}
}

// TestSuperAndModulePathCalls covers calls the resolver used to drop or
// guess: super calls in Java and Python, and Python calls through a
// dotted module path.
func TestSuperAndModulePathCalls(t *testing.T) {
	repo := t.TempDir()
	write := func(rel, src string) {
		t.Helper()
		p := filepath.Join(repo, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("j/Base.java", `package j;
class Base {
    Base(int x) {}
    void close() {}
}
interface Named {
    default String name() { return "n"; }
}
class Middle extends Base {
    Middle() { super(1); }
}
class Leaf extends Middle implements Named {
    Leaf() { this(2); }
    Leaf(int y) { super(); }
    void close() { super.close(); }
    public String name() { return Named.super.name(); }
    public String toString() { return super.toString(); }
}
class Repo extends org.springframework.data.Repository {
    void save() { super.save(); }
}
class Outer {
    static class Inner {
        void open() {}
    }
}
class Sub extends Outer.Inner {
    void open() { super.open(); }
}
`)
	write("p/base.py", "class Base:\n    def get(self):\n        return 1\n\n\nclass Child(Base):\n    def get(self):\n        return super(Child, self).get()\n")
	write("pkg/__init__.py", "from .api import get\n")
	write("pkg/api.py", "def get():\n    return None\n")
	write("pkg/models.py", "class Response:\n    pass\n")
	write("app.py", "import os\nimport pkg\nimport pkg.models\n\n\nclass Paths:\n    def join(self):\n        pass\n\n\ndef main():\n    pkg.models.Response()\n    os.path.join('a', 'b')\n")
	st, err := store.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := indexer.Index(repo, st, indexer.Options{}); err != nil {
		t.Fatal(err)
	}
	callees := func(name string) string {
		t.Helper()
		syms, err := st.Lookup(name)
		if err != nil || len(syms) == 0 {
			t.Fatalf("lookup %s: %v %v", name, syms, err)
		}
		var out []string
		for _, s := range syms { // overloaded constructors share a name
			refs, err := st.Callees(s.ID, true)
			if err != nil {
				t.Fatal(err)
			}
			for _, r := range refs {
				out = append(out, r.Qualified+"["+r.Confidence+"]")
			}
		}
		return strings.Join(out, " ")
	}
	for _, c := range []struct{ name, want string }{
		// super.close() on Leaf finds Base.close two levels up.
		{"j.Leaf.close", "j.Base.close[exact]"},
		// Named.super.name() calls the interface's default method.
		{"j.Leaf.name", "j.Named.name[exact]"},
		// super.toString() looks in the superclass chain only, never in
		// the class's own interfaces (Named has no toString anyway), and
		// that chain ends at Object, which is not indexed: no edge.
		{"j.Leaf.toString", ""},
		// super(1) calls Base's constructor; Leaf's constructors call
		// this(2), and super() into Middle's constructor.
		{"j.Middle.Middle", "j.Base.Base[exact]"},
		{"j.Leaf.Leaf", "j.Leaf.Leaf[exact] j.Middle.Middle[exact]"},
		// A nested superclass written as Outer.Inner.
		{"j.Sub.open", "j.Outer.Inner.open[exact]"},
		// A supertype outside the repository: an external reference.
		{"j.Repo.save", "org.springframework.data.Repository.save[external]"},
		// Python's super(Cls, self) form.
		{"p.base.Child.get", "p.base.Base.get[exact]"},
		// A module path rooted at an import resolves, and os.path.join is
		// external instead of matching the repository's only join method.
		{"app.main", "pkg.models.Response[exact] os.path.join[external]"},
	} {
		if got := callees(c.name); got != c.want {
			t.Errorf("callees of %s:\n got: %s\nwant: %s", c.name, got, c.want)
		}
	}
}

// TestGeneratedCode checks that generated files are indexed, flagged and
// ranked after hand-written code, and left out with SkipGenerated.
func TestGeneratedCode(t *testing.T) {
	st := indexSample(t)
	hits, err := st.Search("helper", store.SearchOptions{})
	if err != nil || len(hits) == 0 {
		t.Fatalf("search: %v %v", hits, err)
	}
	last := hits[len(hits)-1]
	if last.Qualified != "ratelimit.GeneratedHelper" || !last.Generated {
		t.Errorf("generated code should come last and be flagged: %+v", hits)
	}
	for _, h := range hits[:len(hits)-1] {
		if h.Generated {
			t.Errorf("%s flagged as generated", h.Qualified)
		}
	}

	st2, err := store.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	res, err := indexer.Index("../../testdata/sample", st2, indexer.Options{SkipGenerated: true})
	if err != nil || res.Files != 14 || res.Skipped != 1 {
		t.Fatalf("SkipGenerated: %+v %v; want 14 files, 1 skipped", res, err)
	}
	if syms, _ := st2.Lookup("ratelimit.GeneratedHelper"); len(syms) != 0 {
		t.Error("generated symbol indexed with SkipGenerated")
	}
}

// TestPlatform checks that Go files are indexed for one build
// configuration, so declarations of the same name for different platforms
// are not merged.
func TestPlatform(t *testing.T) {
	repo := t.TempDir()
	write := func(rel, src string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, rel), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module example.com/m\n\ngo 1.24\n")
	write("conn.go", "package m\n\n// Conn is a connection.\ntype Conn interface{ Close() error }\n\nfunc use(c Conn) { c.Close() }\n")
	write("conn_linux.go", "package m\n\nfunc open() string { return linuxName() }\n\nfunc linuxName() string { return \"l\" }\n")
	write("conn_windows.go", "package m\n\nfunc open() string { return windowsName() }\n\nfunc windowsName() string { return \"w\" }\n")
	write("stub.go", "//go:build nethttpomit\n\npackage m\n\n// Conn is a stub with the interface's name.\ntype Conn struct{}\n")
	callees := func(platform string) string {
		t.Helper()
		st, err := store.Open(filepath.Join(t.TempDir(), "index.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		if _, err := indexer.Index(repo, st, indexer.Options{Platform: platform}); err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, name := range []string{"m.open", "m.use"} {
			syms, _ := st.Lookup(name)
			for _, s := range syms {
				refs, _ := st.Callees(s.ID, false)
				for _, r := range refs {
					out = append(out, s.Qualified+"->"+r.Qualified)
				}
			}
		}
		conn, _ := st.Lookup("m.Conn")
		for _, c := range conn {
			out = append(out, "Conn:"+c.Kind)
		}
		return strings.Join(out, " ")
	}
	// The stub's build tag is never set, so Conn stays an interface.
	if got := callees("linux/amd64"); got != "m.open->m.linuxName m.use->m.Conn.Close Conn:interface" {
		t.Errorf("linux/amd64: %s", got)
	}
	if got := callees("windows/amd64"); got != "m.open->m.windowsName m.use->m.Conn.Close Conn:interface" {
		t.Errorf("windows/amd64: %s", got)
	}
	// "all" merges both platforms' open, and the stub.
	if got := callees("all"); !strings.Contains(got, "m.open->m.linuxName") || !strings.Contains(got, "m.open->m.windowsName") || !strings.Contains(got, "Conn:struct") {
		t.Errorf("all: %s", got)
	}
	if _, err := indexer.Index(repo, nil, indexer.Options{Platform: "linux"}); err == nil || !strings.Contains(err.Error(), "GOOS/GOARCH") {
		t.Errorf("invalid platform: %v", err)
	}
}

// TestGoExpressionTypes covers receivers whose type the index works out
// from declarations: anonymous struct and interface types, generic type
// parameters, and locals that shadow a package.
func TestGoExpressionTypes(t *testing.T) {
	repo := t.TempDir()
	write := func(rel, src string) {
		t.Helper()
		p := filepath.Join(repo, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module example.com/m\n\ngo 1.24\n")
	write("lock/lock.go", "package lock\n\n// Mutex locks.\ntype Mutex struct{}\n\nfunc (m *Mutex) Lock() {}\n\n// Counter counts.\ntype Counter struct{}\n\nfunc (c *Counter) Add(n int) int { return n }\n")
	write("app/app.go", `package app

import (
	"example.com/m/lock"
)

// Point is implemented by curve points.
type Point[P any] interface {
	Add(p P) P
	Bytes() []byte
}

// Curve works on points of type P.
type Curve[P Point[P]] struct{ base P }

func (c *Curve[P]) Double(p P) []byte {
	return p.Add(p).Bytes()
}

func Sum[P Point[P]](a, b P) []byte {
	return a.Add(b).Bytes()
}

var registry struct {
	lock.Mutex
	stats struct{ calls lock.Counter }
}

func Register() {
	registry.Lock()
	registry.stats.calls.Add(1)
	var r interface{ lock.Mutex }
	_ = r
}

func Shadow() {
	lock := lock.Counter{}
	lock.Add(2)
}
`)
	st, err := store.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := indexer.Index(repo, st, indexer.Options{Platform: "linux/amd64"}); err != nil {
		t.Fatal(err)
	}
	callees := func(name string) string {
		t.Helper()
		syms, err := st.Lookup(name)
		if err != nil || len(syms) != 1 {
			t.Fatalf("lookup %s: %v %v", name, syms, err)
		}
		refs, err := st.Callees(syms[0].ID, true)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, r := range refs {
			out = append(out, r.Qualified+"["+r.Confidence+"]")
		}
		return strings.Join(out, " ")
	}
	for _, c := range []struct{ name, want string }{
		// A type parameter has its constraint's methods; Add returns P, so
		// .Bytes() is looked up on the constraint again, never as an
		// external "app.P.Bytes".
		{"app.Curve.Double", "app.Point.Bytes[exact] app.Point.Add[exact]"},
		{"app.Sum", "app.Point.Bytes[exact] app.Point.Add[exact]"},
		// An anonymous struct: a method of its embedded type, and a field
		// chain through a nested anonymous struct.
		{"app.Register", "lock.Mutex.Lock[exact] lock.Counter.Add[exact]"},
		// The local named lock is defined by this statement: on its right,
		// lock is still the package.
		{"app.Shadow", "lock.Counter.Add[exact]"},
	} {
		if got := callees(c.name); got != c.want {
			t.Errorf("callees of %s:\n got: %s\nwant: %s", c.name, got, c.want)
		}
	}
}
