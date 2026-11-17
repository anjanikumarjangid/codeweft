package server_test

import (
	"context"
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
	}
	for _, want := range []string{"search_code", "get_symbol", "get_dependency_graph", "get_file_summary", "get_architecture_overview", "list_apis"} {
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
		"Called from:\n- checkout/checkout.go (2 call site(s))",
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
		"Languages: go (5 files), java (5 files), python (4 files)",
		"HTTP frameworks:",
		"- checkout/cmd/server/main.main at checkout/cmd/server/main.go:11",
		"- payments [go, 1 file(s), 6 symbols] - Package payments processes customer payments.",
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
