package server_test

import (
	"context"
	"path/filepath"
	"strings"
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
	ss, err := server.New(st).Connect(ctx, serverT, nil)
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
	for _, want := range []string{"search_code", "get_symbol"} {
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
	for _, want := range []string{"ratelimit.TokenBucket.Allow", "ratelimit/limiter.go:21-25", "b.mu.Lock()"} {
		if !strings.Contains(out, want) {
			t.Errorf("get_symbol output missing %q:\n%s", want, out)
		}
	}
}
