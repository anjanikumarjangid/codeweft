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
	"github.com/anjanikumarjangid/codewiki/internal/links"
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
	// 10 Go (one of them generated), 8 Java, 7 Python and 2 proto files;
	// test files are kept apart and not counted.
	if res.Files != 27 || res.Generated != 1 || res.Skipped != 0 {
		t.Fatalf("got %d files (%d generated), %d skipped; want 27 (1), 0", res.Files, res.Generated, res.Skipped)
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
	if files != 27 || syms != 176 {
		t.Fatalf("after re-index: %d files, %d symbols; want 27 and 176", files, syms)
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
		{"refun", "interface method", "payments.Gateway.Refund"},
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
	if res.Files != 0 || res.Unchanged != 30 || res.Removed != 0 {
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
	if res.Files != 2 || res.Unchanged != 28 || res.Removed != 1 {
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
	// 27 default files + 2 Go and Java test files - 7 Python files under
	// py/ (the Python test file is under py/ too).
	if res.Files != 22 {
		t.Fatalf("indexed %d files, want 22", res.Files)
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
	if res.Files != 1 || res.Unchanged != 21 || res.Removed != 0 {
		t.Fatalf("reindex with stored options: %+v; want 1 changed, 21 unchanged, 0 removed", res)
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
	if len(o.Languages) != 4 || o.Endpoints != 6 {
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
	if err != nil || res.Files != 26 || res.Skipped != 1 {
		t.Fatalf("SkipGenerated: %+v %v; want 26 files, 1 skipped", res, err)
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

func (c *Curve[P]) newPoint() P { return c.base }

func (c *Curve[P]) Fresh() []byte {
	return c.newPoint().Bytes()
}

// Node is a syntax tree node.
type Node interface{ Pos() int }

// Ident is a name.
type Ident struct{}

func (Ident) Pos() int    { return 1 }
func (Ident) Name() string { return "x" }

// Lit is a literal.
type Lit struct{}

func (Lit) Pos() int { return 2 }

func Visit(n Node) {
	switch x := n.(type) {
	case Ident:
		x.Name()
	case Lit, nil:
		x.Pos()
	}
}

// List visits its items.
type List []Ident

func (l List) Names() {
	for _, l := range l {
		l.Name()
	}
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
		// A generic receiver's method returning P: P's constraint.
		{"app.Curve.Fresh", "app.Point.Bytes[exact] app.Curve.newPoint[exact]"},
		// A type switch: x is Ident in its case, and n's type (Node) in a
		// case listing several types.
		{"app.Visit", "app.Ident.Name[exact] app.Node.Pos[exact]"},
		// The loop variable l shadows the receiver l inside the loop.
		{"app.List.Names", "app.Ident.Name[exact]"},
	} {
		if got := callees(c.name); got != c.want {
			t.Errorf("callees of %s:\n got: %s\nwant: %s", c.name, got, c.want)
		}
	}
}

// TestStdModuleQualifiers checks that, in a tree declaring module std
// (GOROOT/src), an import of a package whose name differs from its
// directory (math/rand/v2 holds package rand) resolves.
func TestStdModuleQualifiers(t *testing.T) {
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
	write("go.mod", "module std\n\ngo 1.24\n")
	write("math/rand/v2/rand.go", "package rand\n\n// Uint64 returns a number.\nfunc Uint64() uint64 { return 4 }\n")
	write("app/app.go", "package app\n\nimport \"math/rand/v2\"\n\nfunc Roll() uint64 { return rand.Uint64() }\n")
	st, err := store.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := indexer.Index(repo, st, indexer.Options{Platform: "linux/amd64"}); err != nil {
		t.Fatal(err)
	}
	roll, _ := st.Lookup("app.Roll")
	refs, err := st.Callees(roll[0].ID, true)
	if err != nil || len(refs) != 1 || refs[0].Qualified != "math/rand/v2/rand.Uint64" || refs[0].External {
		t.Errorf("app.Roll callees = %+v, %v; want the internal math/rand/v2/rand.Uint64", refs, err)
	}
}

// TestJavaScopes covers how Java resolves names the way javac does:
// inherited and enclosing-class methods called without a receiver, nested
// types named without their outer class, member types inherited from a
// supertype, type variables, java.lang, and implicit super() calls.
func TestJavaScopes(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, "j"), 0o755); err != nil {
		t.Fatal(err)
	}
	src := `package j;
class Base {
    Base() {}
    int size() { return 0; }
    static class Entry { void touch() {} }
}
class Outer {
    static void check() {}
    class Inner extends Base {
        Inner() { }
        int count() { check(); return size(); }
        void entry() { new Entry().touch(); }
    }
    static class Form { Form() {} }
    Object writeReplace() { return new Form(); }
}
class Box<T extends Base> {
    T item;
    int total(T t) { return t.size(); }
    String name() { return String.valueOf(1); }
}
class Hasher {
    Hasher putBytes(byte[] b) { return this; }
}
class Murmur extends Hasher {
    Hasher putBytes(byte[] b, int off, int len) { return this; }
    Hasher putString(String s) { return putBytes(s.getBytes()); }
}
class Uses {
    Base base() { return new Base(); }
    int chain() { return base().size(); }
    void fail(java.util.List<Base> items) {
        try { chain(); } catch (IllegalStateException e) { e.getMessage(); }
        for (Base b : items) { b.size(); }
        items.get(0).size();
        getClass().getSimpleName();
        Runnable r = new Runnable() { public void run() {} };
    }
}
`
	if err := os.WriteFile(filepath.Join(repo, "j", "All.java"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
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
		for _, s := range syms {
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
		// check() is the enclosing class's; size() is inherited from Base.
		{"j.Outer.Inner.count", "j.Outer.check[exact] j.Base.size[exact]"},
		// Entry is a member type Inner inherits from Base; the method on
		// the new object is found from the object's type.
		{"j.Outer.Inner.entry", "j.Base.Entry.touch[exact] j.Base.Entry[exact]"},
		// Inner's constructor calls Base's implicitly.
		{"j.Outer.Inner.Inner", "j.Base.Base[exact]"},
		// Form is Outer's nested class, named without Outer.
		{"j.Outer.writeReplace", "j.Outer.Form.Form[exact]"},
		// T is bounded by Base; String is java.lang.String.
		{"j.Box.total", "j.Base.size[exact]"},
		{"j.Box.name", "java.lang.String.valueOf[external]"},
		// putBytes(x) with one argument is the superclass's overload.
		{"j.Murmur.putString", "j.Hasher.putBytes[exact] java.lang.String.getBytes[external]"},
		// base() returns Base, so .size() is Base.size.
		{"j.Uses.chain", "j.Base.size[exact] j.Uses.base[exact]"},
		// A catch parameter and a for-each variable have their declared
		// types; items.get(0) is a value of an outside type, so its size()
		// is not guessed; getClass() is Object's; new Runnable() { ... }
		// calls nothing of the interface.
		{"j.Uses.fail", "j.Uses.chain[exact] j.Base.size[exact] java.lang.IllegalStateException.getMessage[external] " +
			"java.util.List.get[external] java.lang.Class.getSimpleName[external] java.lang.Object.getClass[external]"},
	} {
		if got := callees(c.name); got != c.want {
			t.Errorf("callees of %s:\n got: %s\nwant: %s", c.name, got, c.want)
		}
	}
}

// TestPythonTypes covers Python receivers typed from return annotations,
// assignments, module functions and class names.
func TestPythonTypes(t *testing.T) {
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
	write("app/adapters.py", "class BaseAdapter:\n    def __init__(self):\n        pass\n\n    def send(self, req):\n        pass\n\n\nclass HTTPAdapter(BaseAdapter):\n    def __init__(self):\n        BaseAdapter.__init__(self)\n")
	write("app/sessions.py", `from .adapters import BaseAdapter, HTTPAdapter


def default_adapter() -> BaseAdapter:
    return HTTPAdapter()


class Session:
    def get_adapter(self, url) -> BaseAdapter:
        return HTTPAdapter()

    def send(self, req):
        adapter = self.get_adapter(req)
        adapter.send(req)
        data = {}
        data.update(req)

    def send_default(self, req):
        default_adapter().send(req)
`)
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
		if err != nil || len(syms) != 1 {
			t.Fatalf("lookup %s: %v %v", name, syms, err)
		}
		refs, err := st.Callees(syms[0].ID, false)
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
		// get_adapter() and default_adapter() return BaseAdapter; a dict's
		// update is not guessed to be anything of the repository.
		{"app.sessions.Session.send", "app.sessions.Session.get_adapter[exact] app.adapters.BaseAdapter.send[exact]"},
		{"app.sessions.Session.send_default", "app.adapters.BaseAdapter.send[exact] app.sessions.default_adapter[exact]"},
		// BaseAdapter.__init__(self) through the class name.
		{"app.adapters.HTTPAdapter.__init__", "app.adapters.BaseAdapter.__init__[exact]"},
	} {
		if got := callees(c.name); got != c.want {
			t.Errorf("callees of %s:\n got: %s\nwant: %s", c.name, got, c.want)
		}
	}
}

// TestHTTPLinks checks the HTTP contracts of the fixture: routes in Go,
// Java and Python, and client calls between them (net/http, Feign,
// RestTemplate, requests), linked by path pattern and method.
func TestHTTPLinks(t *testing.T) {
	st := indexSample(t)
	rows, err := st.Contracts(store.ContractHTTP)
	if err != nil {
		t.Fatal(err)
	}
	ends := make([]links.End, len(rows))
	for i, r := range rows {
		ends[i] = links.End{ContractRow: r}
	}
	res := links.Match(ends)
	var got []string
	for _, l := range res.Links {
		got = append(got, fmt.Sprintf("%s %s: %s -> %s [%s]", l.To.Method, l.To.Name, l.From.Symbol, l.To.Symbol, l.Confidence))
	}
	want := []string{
		"POST /api/v1/checkout: com.shop.orders.CheckoutClient.checkout -> checkout.handleCheckout [exact]",
		"GET /api/v1/orders/*: com.shop.orders.CheckoutOrders.find -> checkout.Handler.getOrder [pattern]",
		"GET /inventory/stock/*: checkout.stockLevel -> py.inventory.api.get_stock [pattern]",
		"POST /inventory/stock/*/reserve: checkout.reserveRequest -> py.inventory.api.reserve [pattern]",
		"POST /orders: py.inventory.api.place_order -> com.shop.orders.OrderController.place [pattern]",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("links:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if len(res.Unmatched) != 0 {
		t.Errorf("unmatched uses: %+v", res.Unmatched)
	}
	// GET /orders/{id} in Java is not called by anything in the fixture.
	if len(res.Unreferring) != 1 || res.Unreferring[0].Symbol != "com.shop.orders.OrderController.get" {
		t.Errorf("unused routes: %+v", res.Unreferring)
	}
}

// TestTemporalLinks links the workflows and activities of the three SDKs
// in the fixture, across languages: a Java workflow starts the Go one by
// name, the Go workflow runs a Python activity, a Python workflow starts
// the Java one by its annotated name.
func TestTemporalLinks(t *testing.T) {
	st := indexSample(t)
	var ends []links.End
	for _, kind := range []string{store.ContractTemporalWorkflow, store.ContractTemporalActivity} {
		rows, err := st.Contracts(kind)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rows {
			ends = append(ends, links.End{ContractRow: r})
		}
	}
	res := links.Match(ends)
	var got []string
	for _, l := range res.Links {
		got = append(got, fmt.Sprintf("%s %s [%s]: %s -> %s at %s:%d", l.To.Kind, l.To.Name, l.To.Detail,
			l.From.Symbol, l.To.Symbol, l.To.File, l.To.Line))
	}
	want := []string{
		"temporal-activity ChargeCard [billing]: com.shop.orders.BillingWorkflowImpl.bill -> com.shop.orders.BillingActivitiesImpl.chargeCard at java/com/shop/orders/BillingWorkflow.java:46",
		"temporal-activity NotifyCustomer []: fulfillment.ShipOrder -> py.inventory.notify.notify_customer at py/inventory/notify.py:6",
		"temporal-activity Release [fulfillment]: fulfillment.ReturnOrder -> fulfillment.Activities.Release at fulfillment/workflow.go:35",
		"temporal-activity Reserve [fulfillment]: fulfillment.ShipOrder -> fulfillment.Activities.Reserve at fulfillment/workflow.go:32",
		"temporal-activity notify_customer []: py.inventory.notify.RestockWorkflow.run -> py.inventory.notify.notify_customer at py/inventory/notify.py:6",
		"temporal-workflow BillingWorkflow [billing]: com.shop.orders.BillingStarter.start -> com.shop.orders.BillingWorkflowImpl.bill at java/com/shop/orders/BillingWorkflow.java:37",
		"temporal-workflow RestockWorkflow []: py.inventory.notify.restock -> py.inventory.notify.RestockWorkflow.run at py/inventory/notify.py:15",
		"temporal-workflow ShipOrder [fulfillment]: com.shop.orders.BillingWorkflowImpl.bill -> fulfillment.ShipOrder at fulfillment/workflow.go:13",
		"temporal-workflow ShipOrder [fulfillment]: fulfillment.StartShipping -> fulfillment.ShipOrder at fulfillment/workflow.go:13",
		"temporal-workflow bill-order [billing]: py.inventory.notify.RestockWorkflow.run -> com.shop.orders.BillingWorkflowImpl.bill at java/com/shop/orders/BillingWorkflow.java:37",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("links:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if len(res.Unmatched) != 0 {
		t.Errorf("unmatched uses: %+v", res.Unmatched)
	}
	// Nothing starts the return workflow or the refund activity; the
	// unexported audit method is not an activity, and the interfaces'
	// copies are reported at their implementations, not twice.
	var unused []string
	for _, p := range res.Unreferring {
		unused = append(unused, p.Name+" "+p.Symbol)
	}
	wantUnused := "ReturnOrder fulfillment.ReturnOrder, process-return fulfillment.ReturnOrder, " +
		"RefundCard com.shop.orders.BillingActivitiesImpl.refundCard, refund com.shop.orders.BillingActivitiesImpl.refundCard"
	if strings.Join(unused, ", ") != wantUnused {
		t.Errorf("providers without uses: %s\nwant: %s", strings.Join(unused, ", "), wantUnused)
	}
}

// TestPythonLocalImport is the regression test for a module imported inside
// a block (`if __name__ == "__main__": import uvicorn`) taken for an object
// whose method could be matched by name: uvicorn.run(app) was linked to the
// repository's only method named run.
func TestPythonLocalImport(t *testing.T) {
	st := indexSample(t)
	syms, err := st.Lookup("py.inventory.api.__main__")
	if err != nil || len(syms) != 1 {
		t.Fatalf("lookup: %v %v", syms, err)
	}
	callees, err := st.Callees(syms[0].ID, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range callees {
		if c.Qualified == "py.inventory.notify.RestockWorkflow.run" {
			t.Errorf("uvicorn.run linked to %s", c.Qualified)
		}
	}
}

// TestGRPCLinks links gRPC clients to servers across the fixture's three
// languages, with the services' full names from the .proto files.
func TestGRPCLinks(t *testing.T) {
	st := indexSample(t)
	rows, err := st.Contracts(store.ContractGRPC)
	if err != nil {
		t.Fatal(err)
	}
	ends := make([]links.End, len(rows))
	for i, r := range rows {
		ends[i] = links.End{ContractRow: r}
	}
	res := links.Match(ends)
	var got []string
	for _, l := range res.Links {
		got = append(got, fmt.Sprintf("%s [%s]: %s -> %s at %s:%d", l.To.Name, l.To.Detail, l.From.Symbol, l.To.Symbol, l.To.File, l.To.Line))
	}
	want := []string{
		"InventoryService/GetStock [shop.inventory.v1.InventoryService]: checkout.available -> py.inventory.grpc_api.InventoryServicer.GetStock at py/inventory/grpc_api.py:10",
		"InventoryService/GetStock [shop.inventory.v1.InventoryService]: com.shop.orders.RefundServer.refund -> py.inventory.grpc_api.InventoryServicer.GetStock at py/inventory/grpc_api.py:10",
		"InventoryService/Reserve [shop.inventory.v1.InventoryService]: checkout.stockClient.hold -> py.inventory.grpc_api.InventoryServicer.Reserve at py/inventory/grpc_api.py:13",
		"InventoryService/Reserve [shop.inventory.v1.InventoryService]: com.shop.orders.RefundServer.restock -> py.inventory.grpc_api.InventoryServicer.Reserve at py/inventory/grpc_api.py:13",
		"OrderService/GetOrder [shop.orders.v1.OrderService]: py.inventory.grpc_api.OrdersClient.order -> checkout.orderServer.GetOrder at checkout/grpc.go:18",
		"RefundService/Refund [shop.orders.v1.RefundService]: py.inventory.grpc_api.refund -> com.shop.orders.RefundServer.refund at java/com/shop/orders/RefundServer.java:20",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("links:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if len(res.Unmatched) != 0 {
		t.Errorf("unmatched uses: %+v", res.Unmatched)
	}
	// Close is a method of the Go server but not an RPC, _audit is private,
	// and withDeadlineAfter configures a Java stub: none is a contract.
	if len(res.Unreferring) != 1 || res.Unreferring[0].Symbol != "checkout.orderServer.ListOrders" {
		t.Errorf("providers without uses: %+v", res.Unreferring)
	}

	// A Go call on a client of unknown type is never matched by name to a
	// method of another language (here the Python servicer's GetStock).
	syms, err := st.Lookup("checkout.available")
	if err != nil || len(syms) != 1 {
		t.Fatalf("lookup: %v %v", syms, err)
	}
	callees, err := st.Callees(syms[0].ID, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range callees {
		if strings.HasPrefix(c.Qualified, "py.") {
			t.Errorf("Go call linked to %s", c.Qualified)
		}
	}
}

func TestParseProto(t *testing.T) {
	src, err := os.ReadFile("../../testdata/sample/proto/orders.proto")
	if err != nil {
		t.Fatal(err)
	}
	fr, err := indexer.ParseProto("proto/orders.proto", src)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, s := range fr.Symbols {
		got = append(got, fmt.Sprintf("%s %s %q %q", s.Kind, s.Qualified, s.Signature, s.Doc))
	}
	want := []string{
		`service shop.orders.v1.OrderService "service OrderService" "OrderService reads orders; served by checkout (Go)."`,
		`rpc shop.orders.v1.OrderService.GetOrder "rpc GetOrder(GetOrderRequest) returns (Order)" ""`,
		`rpc shop.orders.v1.OrderService.ListOrders "rpc ListOrders(ListOrdersRequest) returns (stream Order)" ""`,
		`service shop.orders.v1.RefundService "service RefundService" "RefundService refunds orders; served by the Java orders service."`,
		`rpc shop.orders.v1.RefundService.Refund "rpc Refund(RefundRequest) returns (Order)" ""`,
		`message shop.orders.v1.GetOrderRequest "message GetOrderRequest" ""`,
		`message shop.orders.v1.ListOrdersRequest "message ListOrdersRequest" ""`,
		`message shop.orders.v1.RefundRequest "message RefundRequest" ""`,
		`message shop.orders.v1.Order "message Order" ""`,
		`message shop.orders.v1.Order.Line "message Line" "Line is one item of the order."`,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("symbols:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if fr.File.Package != "shop.orders.v1" || fr.File.Lang != "proto" {
		t.Errorf("file: %+v", fr.File)
	}
}

// TestKafkaLinks links Kafka producers to the consumers of their topics
// across the fixture's three languages and five client libraries.
func TestKafkaLinks(t *testing.T) {
	st := indexSample(t)
	rows, err := st.Contracts(store.ContractKafka)
	if err != nil {
		t.Fatal(err)
	}
	ends := make([]links.End, len(rows))
	for i, r := range rows {
		ends[i] = links.End{ContractRow: r}
	}
	res := links.Match(ends)
	var got []string
	for _, l := range res.Links {
		got = append(got, fmt.Sprintf("%s [%s]: %s -> %s at %s:%d (%s)", l.To.Name, l.To.Detail, l.From.Symbol, l.To.Symbol,
			l.To.File, l.To.Line, l.Confidence))
	}
	want := []string{
		// A topic read from configuration: the same property on both sides.
		"${inventory.topic} [audit]: com.shop.orders.OrderEvents.requestRestock -> com.shop.orders.OrderEvents.audit at java/com/shop/orders/OrderEvents.java:29 (pattern)",
		// kafka-go producer (package kafka from the path kafka-go) to a
		// Spring listener and a kafka-python consumer, one per group.
		"orders.placed [billing]: checkout.publishOrderPlaced -> com.shop.orders.OrderEvents.onOrderPlaced at java/com/shop/orders/OrderEvents.java:20 (exact)",
		"orders.placed [inventory]: checkout.publishOrderPlaced -> py.inventory.events.consume_orders at py/inventory/events.py:8 (exact)",
		// KafkaTemplate with a constant topic to a sarama consumer group,
		// reported at its handler's ConsumeClaim.
		"payments.settled []: com.shop.orders.OrderEvents.onOrderPlaced -> checkout.paymentHandler.ConsumeClaim at checkout/events.go:27 (exact)",
		// confluent-kafka (Python, a self attribute) to confluent-kafka-go.
		"stock.changed []: py.inventory.events.StockEvents.publish -> checkout.watchStock at checkout/events.go:37 (exact)",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("links:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if len(res.Unmatched) != 0 || len(res.Unreferring) != 0 {
		t.Errorf("unmatched uses %+v, providers %+v", res.Unmatched, res.Unreferring)
	}
}

// TestKafkaForms covers producer and consumer forms found in real code:
// a topic list held in a variable, a KafkaTemplate passed to a lambda by
// executeInTransaction, and sendDefault's configured topic.
func TestKafkaForms(t *testing.T) {
	contracts := func(fr *indexer.FileResult, err error) string {
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, c := range fr.Contracts {
			if c.Kind == store.ContractKafka {
				out = append(out, fmt.Sprintf("%s %s by %s exact=%v", c.Role, c.Name, c.Symbol, c.Exact))
			}
		}
		return strings.Join(out, "; ")
	}
	goSrc := `package events

import "github.com/confluentinc/confluent-kafka-go/v2/kafka"

func consume(c *kafka.Consumer) error {
	topic := "go-test-topic"
	topics := []string{topic, "audit"}
	return c.SubscribeTopics(topics, nil)
}

func produce(p *kafka.Producer) error {
	topic := "go-test-topic"
	return p.Produce(&kafka.Message{TopicPartition: kafka.TopicPartition{Topic: &topic}}, nil)
}
`
	want := "provides go-test-topic by events.consume exact=true; provides audit by events.consume exact=true; " +
		"uses go-test-topic by events.produce exact=true"
	if got := contracts(indexer.ParseGo("events/events.go", []byte(goSrc))); got != want {
		t.Errorf("Go:\n got %s\nwant %s", got, want)
	}
	javaSrc := `package com.example;

import org.springframework.kafka.core.KafkaTemplate;

public class Sender {
    private KafkaTemplate<Object, Object> template;

    public void sendAll(java.util.List<String> foos) {
        this.template.executeInTransaction(kafkaTemplate -> {
            foos.forEach(foo -> kafkaTemplate.send("topic2", foo));
            return null;
        });
        template.sendDefault("x");
    }
}
`
	want = "uses topic2 by com.example.Sender.sendAll exact=true; " +
		"uses ${spring.kafka.template.default-topic} by com.example.Sender.sendAll exact=false"
	if got := contracts(indexer.ParseJava("com/example/Sender.java", []byte(javaSrc))); got != want {
		t.Errorf("Java:\n got %s\nwant %s", got, want)
	}
}
