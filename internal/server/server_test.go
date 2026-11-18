package server_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/anjanikumarjangid/codewiki/internal/indexer"
	"github.com/anjanikumarjangid/codewiki/internal/server"
	"github.com/anjanikumarjangid/codewiki/internal/store"
)

// connect indexes the sample repo and returns an MCP client session talking
// to a codewiki server over an in-memory transport, the same protocol path a
// real client like Claude Code uses over stdio.
func connect(t *testing.T) *mcp.ClientSession {
	t.Helper()
	return connectWith(t, server.Options{})
}

func connectWith(t *testing.T, opts server.Options) *mcp.ClientSession {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if _, err := indexer.Index("../../testdata/sample", st, indexer.Options{}); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	serverT, clientT := mcp.NewInMemoryTransports()
	ss, err := server.New(st, opts).Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ss.Close() })

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v0"}, nil)
	cs, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func callText(t *testing.T, cs *mcp.ClientSession, tool string, args map[string]any) string {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	if res.IsError {
		t.Fatalf("%s returned a tool error: %+v", tool, res.Content)
	}
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

func TestListTools(t *testing.T) {
	cs := connect(t)
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, tool := range res.Tools {
		got[tool.Name] = true
		if tool.Description == "" || tool.InputSchema == nil {
			t.Errorf("tool %s is missing a description or input schema", tool.Name)
		}
		if a := tool.Annotations; a == nil || !a.ReadOnlyHint || a.OpenWorldHint == nil || *a.OpenWorldHint {
			t.Errorf("tool %s should be annotated read-only and closed-world: %+v", tool.Name, a)
		}
	}
	for _, want := range []string{"search_code", "get_symbol", "get_dependency_graph", "get_file_summary", "get_architecture_overview", "list_apis", "get_implementations", "find_tests", "get_service_links"} {
		if !got[want] {
			t.Errorf("tool %s not registered", want)
		}
	}
}

func TestSearchCode(t *testing.T) {
	cs := connect(t)
	out := callText(t, cs, "search_code", map[string]any{"query": "process payment"})
	for _, want := range []string{"payments.PaymentService.ProcessPayment", "payments/service.go:26"} {
		if !strings.Contains(out, want) {
			t.Errorf("search_code output missing %q:\n%s", want, out)
		}
	}
	none := callText(t, cs, "search_code", map[string]any{"query": "kubernetes"})
	if !strings.Contains(none, "No symbols match") {
		t.Errorf("expected a no-match message, got:\n%s", none)
	}
}

func TestGetSymbol(t *testing.T) {
	cs := connect(t)
	out := callText(t, cs, "get_symbol", map[string]any{"name": "TokenBucket.Allow"})
	for _, want := range []string{
		"ratelimit.TokenBucket.Allow", "ratelimit/limiter.go:21-26", "b.mu.Lock()",
		"Calls:\n- ratelimit.TokenBucket.refillLocked", "External calls: sync.Mutex.Lock, sync.Mutex.Unlock",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("get_symbol output missing %q:\n%s", want, out)
		}
	}
}

func TestDependencyGraph(t *testing.T) {
	cs := connect(t)
	out := callText(t, cs, "get_dependency_graph", map[string]any{"symbol": "checkout.Checkout", "depth": 2})
	for _, want := range []string{
		"- payments.PaymentService.ProcessPayment",
		"    - payments.Gateway.Charge",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("callees graph missing %q:\n%s", want, out)
		}
	}
	out = callText(t, cs, "get_dependency_graph", map[string]any{"symbol": "checkout.audit"})
	for _, want := range []string{
		"- payments.PaymentService.ProcessPayment",
		"(matched by method name)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("callees graph missing %q:\n%s", want, out)
		}
	}
	up := callText(t, cs, "get_dependency_graph", map[string]any{"symbol": "Gateway.Charge", "direction": "callers", "depth": 3})
	if !strings.Contains(up, "    - checkout.Checkout") {
		t.Errorf("callers graph should reach checkout.Checkout two levels up:\n%s", up)
	}
	ext := callText(t, cs, "get_dependency_graph", map[string]any{"symbol": "sync"})
	if !strings.Contains(ext, "ratelimit.TokenBucket.Allow calls sync.Mutex.Lock") {
		t.Errorf("external usage listing wrong:\n%s", ext)
	}
}

func TestFileSummary(t *testing.T) {
	cs := connect(t)
	out := callText(t, cs, "get_file_summary", map[string]any{"path": "service.go"})
	for _, want := range []string{
		"payments/service.go (go, package payments)",
		"Package payments processes customer payments.",
		"Imports: errors",
		"func (s *PaymentService) ProcessPayment(customerID string, cents int64) (string, error)",
		"Called from:\n- checkout/checkout.go (3 call site(s))",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("file summary missing %q:\n%s", want, out)
		}
	}
	miss := callText(t, cs, "get_file_summary", map[string]any{"path": "nope.go"})
	if !strings.Contains(miss, "No indexed file matches") {
		t.Errorf("unexpected output for unknown file:\n%s", miss)
	}
}

func TestJavaAndPython(t *testing.T) {
	cs := connect(t)
	out := callText(t, cs, "search_code", map[string]any{"query": "charge customer", "lang": "java"})
	if !strings.Contains(out, "1. com.shop.billing.Billing.charge (method)") {
		t.Errorf("java search:\n%s", out)
	}
	sym := callText(t, cs, "get_symbol", map[string]any{"name": "StockService.reserve"})
	for _, want := range []string{
		"def reserve(self, sku: str, qty: int) -> bool",
		"Reserve qty units of sku, if enough are in stock.",
		"- py.inventory.models.ItemRepository.get",
		"External calls", // _log is internal; reserve itself has none
	} {
		if want == "External calls" {
			if strings.Contains(sym, want) {
				t.Errorf("reserve should have no external calls:\n%s", sym)
			}
			continue
		}
		if !strings.Contains(sym, want) {
			t.Errorf("python get_symbol missing %q:\n%s", want, sym)
		}
	}
	up := callText(t, cs, "get_dependency_graph", map[string]any{"symbol": "Billing.charge", "direction": "callers"})
	if !strings.Contains(up, "- com.shop.orders.OrderService.place") {
		t.Errorf("java callers:\n%s", up)
	}
}

func TestOverviewAndAPIs(t *testing.T) {
	cs := connect(t)
	o := callText(t, cs, "get_architecture_overview", map[string]any{})
	for _, want := range []string{
		"Languages: go (10 files), java (8 files), python (7 files), proto (2 files)",
		"HTTP frameworks:",
		"- checkout/cmd/server/main.main at checkout/cmd/server/main.go:11",
		"- payments [go, 1 file(s), 28 symbols] - Package payments processes customer payments.",
		"Most-used external packages",
	} {
		if !strings.Contains(o, want) {
			t.Errorf("overview missing %q:\n%s", want, o)
		}
	}
	apis := callText(t, cs, "list_apis", map[string]any{"method": "post"})
	for _, want := range []string{"3 endpoint(s) with method POST", "POST    /orders", "-> checkout.handleCheckout (checkout/http.go:23) [net/http]"} {
		if !strings.Contains(apis, want) {
			t.Errorf("list_apis missing %q:\n%s", want, apis)
		}
	}
}

// TestNotReady checks that while the index is not ready (the first build is
// still running) every tool answers with a tool error that says why, and
// works normally once it is ready.
func TestNotReady(t *testing.T) {
	var notReady atomic.Bool
	notReady.Store(true)
	cs := connectWith(t, server.Options{Ready: func() error {
		if notReady.Load() {
			return errors.New("the index is still being built; try again in a few seconds")
		}
		return nil
	}})
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "search_code", Arguments: map[string]any{"query": "payment"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || len(res.Content) == 0 || !strings.Contains(res.Content[0].(*mcp.TextContent).Text, "still being built") {
		t.Fatalf("want a tool error explaining the index is not ready, got %+v", res)
	}
	notReady.Store(false)
	if got := callText(t, cs, "search_code", map[string]any{"query": "payment"}); !strings.Contains(got, "ProcessPayment") {
		t.Errorf("after ready, search_code = %q", got)
	}
}

func callTool(t *testing.T, cs *mcp.ClientSession, tool string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil || res.IsError {
		t.Fatalf("%s: %v %+v", tool, err, res)
	}
	return res
}

// structured decodes a tool's structured result into v.
func structured(t *testing.T, res *mcp.CallToolResult, v any) {
	t.Helper()
	data, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatal(err)
	}
}

// TestGetSymbolAmbiguous checks that a short name matching several symbols
// returns a list of candidates without their source, instead of the full
// source of every match.
func TestGetSymbolAmbiguous(t *testing.T) {
	cs := connect(t)
	res := callTool(t, cs, "get_symbol", map[string]any{"name": "place"})
	var out server.SymbolOutput
	structured(t, res, &out)
	if len(out.Matches) != 0 || out.Total != 2 || len(out.Candidates) != 2 {
		t.Fatalf("got %d matches, %d candidates, total %d; want 0, 2, 2", len(out.Matches), len(out.Candidates), out.Total)
	}
	txt := res.Content[0].(*mcp.TextContent).Text
	for _, want := range []string{
		`"place" matches 2 symbols`,
		"- com.shop.orders.OrderController.place (method) at java/com/shop/orders/OrderController.java:",
		"- com.shop.orders.OrderService.place (method) at java/com/shop/orders/OrderService.java:",
	} {
		if !strings.Contains(txt, want) {
			t.Errorf("candidate list missing %q:\n%s", want, txt)
		}
	}
	if strings.Contains(txt, "```") {
		t.Errorf("candidate list should not include source:\n%s", txt)
	}

	// Java overloads share one qualified name and stay one symbol.
	if got := callText(t, cs, "get_symbol", map[string]any{"name": "Billing.charge"}); strings.Contains(got, "matches") || !strings.Contains(got, "```java") {
		t.Errorf("overloads should be shown as one symbol with source:\n%s", got)
	}

	// More candidates than the limit: say how many were left out.
	defer server.SetLimits(1, 200, 20)()
	if got := callText(t, cs, "get_symbol", map[string]any{"name": "place"}); !strings.Contains(got, "- ... and 1 more") {
		t.Errorf("truncated candidate list should say how many are missing:\n%s", got)
	}
}

func TestOutputLimits(t *testing.T) {
	defer server.SetLimits(30, 2, 1)()
	cs := connect(t)

	// TokenBucket.Allow spans lines 21-26: 2 lines shown, 4 left out.
	res := callTool(t, cs, "get_symbol", map[string]any{"name": "TokenBucket.Allow"})
	var sym server.SymbolOutput
	structured(t, res, &sym)
	if len(sym.Matches) != 1 || sym.Matches[0].BodyOmittedLines != 4 || strings.Count(sym.Matches[0].Body, "\n") != 1 {
		t.Fatalf("body not cut to 2 lines: %+v", sym.Matches)
	}
	txt := res.Content[0].(*mcp.TextContent).Text
	if !strings.Contains(txt, "(4 more line(s) not shown: read ratelimit/limiter.go lines 23-26)") {
		t.Errorf("truncated body should say where the rest is:\n%s", txt)
	}

	// OrderService.java calls into 3 other files: 1 shown, 2 left out.
	res = callTool(t, cs, "get_file_summary", map[string]any{"path": "OrderService.java"})
	var fs server.FileOutput
	structured(t, res, &fs)
	if len(fs.Summary.DependsOn) != 1 || fs.MoreDependsOn != 2 {
		t.Fatalf("depends_on not cut to 1: %d shown, %d more", len(fs.Summary.DependsOn), fs.MoreDependsOn)
	}
	if txt := res.Content[0].(*mcp.TextContent).Text; !strings.Contains(txt, "- ... and 2 more file(s) with fewer call sites") {
		t.Errorf("truncated file links should say how many are missing:\n%s", txt)
	}
}

func TestImplementations(t *testing.T) {
	cs := connect(t)
	for _, c := range []struct {
		args map[string]any
		want []string
	}{
		{map[string]any{"symbol": "Gateway"}, []string{
			"What payments.Gateway (interface) is extended, implemented or overridden by:",
			"- payments.StripeGateway (method set) at payments/service.go:",
			"- payments.LoggingGateway (method set) at payments/service.go:",
			"- payments.VoidingGateway (extends) at payments/service.go:",
		}},
		// Where an interface call actually goes.
		{map[string]any{"symbol": "Gateway.Charge"}, []string{
			"- payments.StripeGateway.Charge (implements) at payments/service.go:",
		}},
		{map[string]any{"symbol": "OrderRepository"}, []string{
			"- com.shop.orders.InMemoryOrderRepository (implements)",
			"  - com.shop.orders.CountingOrderRepository (extends)",
		}},
		{map[string]any{"symbol": "CountingOrderRepository", "direction": "up"}, []string{
			"What com.shop.orders.CountingOrderRepository (class) extends, implements or overrides:",
			"- com.shop.orders.InMemoryOrderRepository (extends)",
			"  - com.shop.orders.OrderRepository (implements)",
			"- java.io.Serializable (implements, outside the repository)",
		}},
		{map[string]any{"symbol": "java.io.Serializable"}, []string{
			"1 repository type(s) extend or implement java.io.Serializable (outside the repository):",
			"- com.shop.orders.CountingOrderRepository (implements)",
		}},
		{map[string]any{"symbol": "save"}, []string{`"save" matches 3 symbols; call get_implementations again`}},
		{map[string]any{"symbol": "checkout.Checkout"}, []string{"checkout.Checkout is a func; implementations apply to"}},
		{map[string]any{"symbol": "FakeGateway"}, []string{"nothing found in the index"}},
	} {
		got := callText(t, cs, "get_implementations", c.args)
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("get_implementations %v: missing %q in:\n%s", c.args, w, got)
			}
		}
	}

	// Limit and path filter.
	restore := server.SetMaxImpls(1)
	got := callText(t, cs, "get_implementations", map[string]any{"symbol": "Gateway"})
	restore()
	if !strings.Contains(got, "- ... and 5 more; pass path to narrow") {
		t.Errorf("limited list should say how many were left out:\n%s", got)
	}
	if got := callText(t, cs, "get_implementations", map[string]any{"symbol": "ItemRepository", "path": "java"}); !strings.Contains(got, "nothing found") {
		t.Errorf("path filter should drop results outside java/:\n%s", got)
	}

	// get_symbol shows the hierarchy inline.
	got = callText(t, cs, "get_symbol", map[string]any{"name": "payments.Gateway"})
	if !strings.Contains(got, "Extended / implemented by:\n- checkout.wrappedGateway (method set)") {
		t.Errorf("get_symbol on an interface should list implementations:\n%s", got)
	}
	got = callText(t, cs, "get_symbol", map[string]any{"name": "py.inventory.models.CachedItemRepository.get"})
	if !strings.Contains(got, "Implements / overrides:\n- py.inventory.models.ItemRepository.get (overrides)") {
		t.Errorf("get_symbol on an overriding method should name what it overrides:\n%s", got)
	}
}

func TestFindTests(t *testing.T) {
	cs := connect(t)
	got := callText(t, cs, "find_tests", map[string]any{"symbol": "refill"})
	for _, want := range []string{
		"3 test(s) exercise ratelimit.refill:",
		"- ratelimit.TestRefill at ratelimit/limiter_test.go:",
		"(calls it directly)",
		"- ratelimit.TestBurst at ratelimit/limiter_test.go:",
		"(4 calls away, through ratelimit.TokenBucket.Allow)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("find_tests refill: missing %q in:\n%s", want, got)
		}
	}
	if got := callText(t, cs, "find_tests", map[string]any{"symbol": "refill", "depth": 1}); strings.Contains(got, "TestBurst") {
		t.Errorf("depth 1 should not reach TestBurst through Allow:\n%s", got)
	}
	if got := callText(t, cs, "find_tests", map[string]any{"symbol": "Gateway.Charge"}); !strings.Contains(got, "No test reaches payments.Gateway.Charge") {
		t.Errorf("untested symbol:\n%s", got)
	}
	if got := callText(t, cs, "find_tests", map[string]any{"symbol": "place"}); !strings.Contains(got, "call find_tests again") {
		t.Errorf("ambiguous name should list candidates:\n%s", got)
	}
	// get_symbol lists the nearest tests inline.
	got = callText(t, cs, "get_symbol", map[string]any{"name": "StockService.reserve"})
	if !strings.Contains(got, "Tested by:\n- py.inventory.test_stock.test_reserve_fails_without_stock at py/inventory/test_stock.py:9 (calls it directly)") {
		t.Errorf("get_symbol should list tests:\n%s", got)
	}
	// Test code is not searchable.
	if got := callText(t, cs, "search_code", map[string]any{"query": "allowTwice"}); !strings.Contains(got, "No symbols match") {
		t.Errorf("test helper found by search_code:\n%s", got)
	}
}

func TestGeneratedLabel(t *testing.T) {
	cs := connect(t)
	if got := callText(t, cs, "get_symbol", map[string]any{"name": "GeneratedHelper"}); !strings.Contains(got, "ratelimit.GeneratedHelper (func, generated code)") {
		t.Errorf("get_symbol should label generated code:\n%s", got)
	}
	if got := callText(t, cs, "search_code", map[string]any{"query": "GeneratedHelper"}); !strings.Contains(got, "(func, generated code)") {
		t.Errorf("search_code should label generated code:\n%s", got)
	}
}

func TestServiceLinks(t *testing.T) {
	cs := connect(t)
	got := callText(t, cs, "get_service_links", map[string]any{"query": "checkout"})
	for _, want := range []string{
		"POST /api/v1/checkout (http), provided by checkout.handleCheckout at checkout/http.go:23",
		"  <- com.shop.orders.CheckoutClient.checkout at java/com/shop/orders/OrderController.java:",
		"GET /api/v1/orders/* (http), provided by checkout.Handler.getOrder at checkout/http.go:27",
		"matched as a pattern: http://checkout/api/v1/orders/*",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("get_service_links: missing %q in:\n%s", want, got)
		}
	}
	got = callText(t, cs, "get_service_links", map[string]any{"kind": "temporal-workflow", "query": "ShipOrder"})
	for _, want := range []string{
		"ShipOrder (temporal-workflow, task queue fulfillment), provided by fulfillment.ShipOrder at fulfillment/workflow.go:13",
		"  <- fulfillment.StartShipping at fulfillment/worker.go:25 (ShipOrder, task queue fulfillment)",
		"  <- com.shop.orders.BillingWorkflowImpl.bill at java/com/shop/orders/BillingWorkflow.java:39 (ShipOrder)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("get_service_links temporal: missing %q in:\n%s", want, got)
		}
	}
	got = callText(t, cs, "get_service_links", map[string]any{"kind": "grpc", "query": "Refund"})
	for _, want := range []string{
		"RefundService/Refund (grpc, service shop.orders.v1.RefundService), provided by com.shop.orders.RefundServer.refund at java/com/shop/orders/RefundServer.java:20",
		"  <- py.inventory.grpc_api.refund at py/inventory/grpc_api.py:33 (RefundService/Refund, service shop.orders.v1.RefundService)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("get_service_links grpc: missing %q in:\n%s", want, got)
		}
	}
	if got := callText(t, cs, "search_code", map[string]any{"query": "GetStock", "kind": "rpc"}); !strings.Contains(got, "shop.inventory.v1.InventoryService.GetStock (rpc) at proto/inventory.proto:11") {
		t.Errorf("search_code rpc:\n%s", got)
	}
	got = callText(t, cs, "get_service_links", map[string]any{"kind": "kafka", "query": "orders.placed"})
	for _, want := range []string{
		"orders.placed (kafka, consumer group billing), provided by com.shop.orders.OrderEvents.onOrderPlaced at java/com/shop/orders/OrderEvents.java:20",
		"  <- checkout.publishOrderPlaced at checkout/events.go:16 (orders.placed)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("get_service_links kafka: missing %q in:\n%s", want, got)
		}
	}
}

// TestLargeAnswersAreCapped lowers the limits to check that a file with
// many declarations is listed a page at a time, and that a graph lists a
// few callers or callees per node and says how many it left out.
func TestLargeAnswersAreCapped(t *testing.T) {
	defer server.SetPageLimits(3, 5, 2)()
	cs := connect(t)

	got := callText(t, cs, "get_file_summary", map[string]any{"path": "payments/service.go"})
	for _, want := range []string{"Declarations 1-3 of ", "call get_file_summary again with offset 3"} {
		if !strings.Contains(got, want) {
			t.Errorf("first page: missing %q in:\n%s", want, got)
		}
	}
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "get_file_summary",
		Arguments: map[string]any{"path": "payments/service.go", "offset": 3}})
	if err != nil {
		t.Fatal(err)
	}
	var page server.FileOutput
	b, _ := json.Marshal(res.StructuredContent)
	if err := json.Unmarshal(b, &page); err != nil {
		t.Fatal(err)
	}
	if page.Offset != 3 || len(page.Summary.Symbols) == 0 || len(page.Summary.Symbols) > 3 ||
		page.Offset+len(page.Summary.Symbols)+page.MoreSymbols != page.TotalSymbols {
		t.Errorf("second page: offset %d, %d listed, %d more, %d total", page.Offset, len(page.Summary.Symbols), page.MoreSymbols, page.TotalSymbols)
	}

	got = callText(t, cs, "get_dependency_graph", map[string]any{"symbol": "checkout.Checkout", "depth": 3})
	if !strings.Contains(got, "more") {
		t.Errorf("graph does not say what it left out:\n%s", got)
	}
	res, err = cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "get_dependency_graph",
		Arguments: map[string]any{"symbol": "checkout.Checkout", "depth": 3}})
	if err != nil {
		t.Fatal(err)
	}
	var g server.GraphOutput
	b, _ = json.Marshal(res.StructuredContent)
	if err := json.Unmarshal(b, &g); err != nil {
		t.Fatal(err)
	}
	if len(g.Edges) > 5 || g.Omitted == 0 {
		t.Errorf("graph: %d edges (limit 5), %d omitted", len(g.Edges), g.Omitted)
	}
	perNode := map[string]int{}
	for _, e := range g.Edges {
		perNode[e.From]++
		if perNode[e.From] > 2 {
			t.Errorf("more than 2 callees listed under %s", e.From)
		}
	}
}
