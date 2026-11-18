package server_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/anjanikumarjangid/codeweft/internal/indexer"
	"github.com/anjanikumarjangid/codeweft/internal/server"
	"github.com/anjanikumarjangid/codeweft/internal/store"
)

// Two services: web calls the orders service over HTTP. Both have a main
// function and a client type of the same name, as separate services do.
var workspaceRepos = map[string]map[string]string{
	"orders": {
		"go.mod": "module example.com/orders\n\ngo 1.24\n",
		"main.go": `package main

import "net/http"

// main starts the orders service.
func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /orders/{id}", getOrder)
	mux.HandleFunc("DELETE /orders/{id}", cancelOrder)
	http.ListenAndServe(":8080", mux)
}

// getOrder returns one order as JSON.
func getOrder(w http.ResponseWriter, r *http.Request) {}

// cancelOrder cancels an order nobody calls.
func cancelOrder(w http.ResponseWriter, r *http.Request) {}

// Store keeps the orders.
type Store struct{}

// Lookup finds an order by id.
func (s *Store) Lookup(id string) string { return id }
`,
	},
	"web": {
		"go.mod": "module example.com/web\n\ngo 1.24\n",
		"main.go": `package main

import "net/http"

const ordersURL = "http://orders:8080"

// main starts the web frontend.
func main() { showOrder("42") }

// showOrder fetches an order from the orders service.
func showOrder(id string) (*http.Response, error) {
	return http.Get(ordersURL + "/orders/" + id)
}

// Store caches pages.
type Store struct{}
`,
	},
}

func connectWorkspace(t *testing.T) *mcp.ClientSession {
	t.Helper()
	var repos []server.Repo
	for _, name := range []string{"orders", "web"} {
		dir := t.TempDir()
		for path, src := range workspaceRepos[name] {
			if err := os.WriteFile(filepath.Join(dir, path), []byte(src), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		st, err := store.Open(filepath.Join(t.TempDir(), "index.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { st.Close() })
		if _, err := indexer.Index(dir, st, indexer.Options{}); err != nil {
			t.Fatal(err)
		}
		repos = append(repos, server.Repo{Name: name, Store: st})
	}
	ctx := context.Background()
	serverT, clientT := mcp.NewInMemoryTransports()
	ss, err := server.NewWorkspace(repos, server.Options{}).Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v0"}, nil).Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func wantAll(t *testing.T, what, got string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("%s: missing %q in:\n%s", what, w, got)
		}
	}
}

func TestWorkspace(t *testing.T) {
	cs := connectWorkspace(t)

	// The call from web reaches the route of orders.
	got := callText(t, cs, "get_service_links", nil)
	wantAll(t, "get_service_links", got,
		"GET /orders/* (http), provided by [orders] main.getOrder at main.go:",
		"  <- [web] main.showOrder at main.go:",
		"matched as a pattern: http://orders:8080/orders/*")
	if strings.Contains(got, "cancelOrder") {
		t.Errorf("a DELETE route is not reached by a GET:\n%s", got)
	}
	if got := callText(t, cs, "get_service_links", map[string]any{"repo": "web"}); !strings.Contains(got, "[orders] main.getOrder") {
		t.Errorf("repo keeps links with an end in it:\n%s", got)
	}

	// A name in both repositories lists candidates with their repository.
	got = callText(t, cs, "get_symbol", map[string]any{"name": "main"})
	wantAll(t, "get_symbol main", got, `"main" matches 2 symbols`, "and its repo", "[orders] main.main", "[web] main.main")
	got = callText(t, cs, "get_symbol", map[string]any{"name": "main", "repo": "web"})
	wantAll(t, "get_symbol main in web", got, "[web] main.main (func)", "main.showOrder")

	// A name in one repository is answered from it.
	got = callText(t, cs, "get_symbol", map[string]any{"name": "showOrder"})
	wantAll(t, "get_symbol showOrder", got, "[web] main.showOrder (func)", "Called by:\n- main.main")
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "get_symbol", Arguments: map[string]any{"name": "Lookup"}})
	if err != nil {
		t.Fatal(err)
	}
	var out server.SymbolOutput
	if b, _ := json.Marshal(res.StructuredContent); json.Unmarshal(b, &out) != nil || len(out.Matches) != 1 || out.Matches[0].Repo != "orders" {
		t.Errorf("structured get_symbol Lookup: %+v", res.StructuredContent)
	}

	got = callText(t, cs, "search_code", map[string]any{"query": "Store"})
	wantAll(t, "search_code", got, "[orders] main.Store (struct)", "[web] main.Store (struct)")
	if got := callText(t, cs, "search_code", map[string]any{"query": "Store", "repo": "orders"}); strings.Contains(got, "[web]") {
		t.Errorf("search_code in orders:\n%s", got)
	}

	got = callText(t, cs, "get_dependency_graph", map[string]any{"symbol": "showOrder", "include_external": true})
	wantAll(t, "get_dependency_graph", got, "[web] main.showOrder calls", "net/http.Get")
	got = callText(t, cs, "get_dependency_graph", map[string]any{"symbol": "main"})
	wantAll(t, "get_dependency_graph main", got, "several repositories", "[orders] main.main", "[web] main.main")
	got = callText(t, cs, "get_dependency_graph", map[string]any{"symbol": "net/http"})
	wantAll(t, "external callers", got, "[orders] main.main", "[web] main.showOrder")

	got = callText(t, cs, "list_apis", nil)
	wantAll(t, "list_apis", got, "GET     [orders] /orders/{id}", "DELETE  [orders] /orders/{id}")

	got = callText(t, cs, "get_file_summary", map[string]any{"path": "main.go"})
	wantAll(t, "get_file_summary", got, `"main.go" matches 2 files`, "- [orders] main.go", "- [web] main.go")
	got = callText(t, cs, "get_file_summary", map[string]any{"path": "main.go", "repo": "orders"})
	wantAll(t, "get_file_summary in orders", got, "[orders] main.go (go, package main)", "getOrder")

	got = callText(t, cs, "get_architecture_overview", nil)
	wantAll(t, "overview", got, "Repository: workspace: orders, web", "Indexed: 2 files,", "- [orders] main.main at", "- [web] main.main at",
		"- net/http (")

	res, err = cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "search_code", Arguments: map[string]any{"query": "x", "repo": "billing"}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Content[0].(*mcp.TextContent).Text, "it has: orders, web") {
		t.Errorf("unknown repo: %+v", res.Content)
	}
}
