// Package server exposes the code index to AI coding agents as MCP tools.
package server

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/anjanikumarjangid/codewiki/internal/store"
)

// Version is reported to MCP clients.
const Version = "0.2.0"

// SearchInput is the input of the search_code tool.
type SearchInput struct {
	Query string `json:"query" jsonschema:"words to search for in symbol names, signatures and doc comments, e.g. 'rate limiter' or 'ParseConfig'"`
	Kind  string `json:"kind,omitempty" jsonschema:"optional filter: func, method, struct, interface or type"`
	Limit int    `json:"limit,omitempty" jsonschema:"maximum results, default 10, max 50"`
}

// SymbolSummary is one search hit.
type SymbolSummary struct {
	Qualified string `json:"qualified"`
	Kind      string `json:"kind"`
	Signature string `json:"signature"`
	Doc       string `json:"doc,omitempty"`
	Location  string `json:"location"`
}

// SearchOutput is the structured result of search_code.
type SearchOutput struct {
	Results []SymbolSummary `json:"results"`
}

// SymbolInput is the input of the get_symbol tool.
type SymbolInput struct {
	Name string `json:"name" jsonschema:"qualified name like 'internal/store.Store.Search', or a short name like 'Search'"`
}

// SymbolOutput is the structured result of get_symbol.
type SymbolOutput struct {
	Matches []store.Symbol `json:"matches"`
}

// New builds an MCP server backed by st.
func New(st *store.Store) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "codewiki", Version: Version}, &mcp.ServerOptions{
		Instructions: "codewiki answers questions about the indexed codebase from a parsed index. " +
			"Use search_code to find where something is implemented, then get_symbol to read its exact signature, doc and source. " +
			"Prefer these tools over grepping or reading whole files.",
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "search_code",
		Description: "Ranked keyword search over the indexed codebase's functions, methods and types. " +
			"Matches names (including camelCase and snake_case parts), signatures and doc comments. " +
			"Returns qualified names and file:line locations.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in SearchInput) (*mcp.CallToolResult, SearchOutput, error) {
		limit := in.Limit
		if limit <= 0 {
			limit = 10
		}
		if limit > 50 {
			limit = 50
		}
		syms, err := st.Search(in.Query, store.SearchOptions{Limit: limit, Kind: in.Kind})
		if err != nil {
			return nil, SearchOutput{}, err
		}
		out := SearchOutput{Results: make([]SymbolSummary, 0, len(syms))}
		for _, sym := range syms {
			out.Results = append(out.Results, SymbolSummary{
				Qualified: sym.Qualified,
				Kind:      sym.Kind,
				Signature: sym.Signature,
				Doc:       firstLine(sym.Doc),
				Location:  fmt.Sprintf("%s:%d", sym.File, sym.StartLine),
			})
		}
		return text(formatSearch(in.Query, out)), out, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "get_symbol",
		Description: "Full details of a function, method or type: signature, doc comment, file and line range, and source code. " +
			"Accepts a qualified name from search_code, or a short name (which may return several matches).",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in SymbolInput) (*mcp.CallToolResult, SymbolOutput, error) {
		syms, err := st.Lookup(strings.TrimSpace(in.Name))
		if err != nil {
			return nil, SymbolOutput{}, err
		}
		if syms == nil {
			syms = []store.Symbol{}
		}
		out := SymbolOutput{Matches: syms}
		return text(formatSymbols(in.Name, syms)), out, nil
	})

	return s
}

func text(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func formatSearch(query string, out SearchOutput) string {
	if len(out.Results) == 0 {
		return fmt.Sprintf("No symbols match %q. Try fewer or different words.", query)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d result(s) for %q:\n", len(out.Results), query)
	for i, r := range out.Results {
		fmt.Fprintf(&b, "\n%d. %s (%s) at %s\n   %s\n", i+1, r.Qualified, r.Kind, r.Location, r.Signature)
		if r.Doc != "" {
			fmt.Fprintf(&b, "   %s\n", r.Doc)
		}
	}
	return b.String()
}

func formatSymbols(name string, syms []store.Symbol) string {
	if len(syms) == 0 {
		return fmt.Sprintf("No symbol named %q. Use search_code to find the qualified name.", name)
	}
	var b strings.Builder
	for i, s := range syms {
		if i > 0 {
			b.WriteString("\n---\n")
		}
		fmt.Fprintf(&b, "%s (%s)\n%s:%d-%d\n", s.Qualified, s.Kind, s.File, s.StartLine, s.EndLine)
		if s.Doc != "" {
			fmt.Fprintf(&b, "\n%s\n", s.Doc)
		}
		fmt.Fprintf(&b, "\n```go\n%s\n```\n", s.Body)
	}
	return b.String()
}
