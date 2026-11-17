// Package server exposes the code index to AI coding agents as MCP tools.
package server

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/anjanikumarjangid/codewiki/internal/store"
)

// Version is reported to MCP clients and by `codewiki version`. Release
// builds set it from the git tag:
// -ldflags "-X github.com/anjanikumarjangid/codewiki/internal/server.Version=1.2.3"
var Version = "0.6.0"

// Output limits. Every tool result lands in the agent's context window, so
// lists and source are capped, and the agent is told what was left out and
// how to get it.
var (
	maxRefs       = 15  // callers/callees listed inline by get_symbol
	maxCandidates = 30  // symbols listed when a name is ambiguous
	maxBodyLines  = 200 // source lines shown by get_symbol
	maxFileLinks  = 20  // related files listed by get_file_summary
	maxImpls      = 50  // types or methods listed by get_implementations
	maxTests      = 30  // tests listed by find_tests
	maxSymTests   = 5   // tests listed inline by get_symbol
)

// Tests are found up to testCallers calls away through the repository's
// own code, and testHelpers calls through helper functions in test files.
const (
	testCallers = 3
	testHelpers = 3
)

// Options configures New. The zero value serves st as it is.
type Options struct {
	// Ready, if set, is called before every tool call. A non-nil error is
	// returned to the agent as a tool error instead of running the tool,
	// e.g. while the first index of a repository is still being built and
	// every query would otherwise come back empty.
	Ready func() error
}

// addTool registers a tool whose handler first checks ready. Every codewiki
// tool only reads the local index, and says so, so clients can treat them
// as safe to call.
func addTool[In, Out any](s *mcp.Server, ready func() error, t *mcp.Tool, h mcp.ToolHandlerFor[In, Out]) {
	closedWorld := false
	t.Annotations = &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: &closedWorld}
	if ready != nil {
		inner := h
		h = func(ctx context.Context, req *mcp.CallToolRequest, in In) (*mcp.CallToolResult, Out, error) {
			if err := ready(); err != nil {
				var zero Out
				return nil, zero, err
			}
			return inner(ctx, req, in)
		}
	}
	mcp.AddTool(s, t, h)
}

// New builds an MCP server backed by st.
func New(st *store.Store, opts Options) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "codewiki", Version: Version}, &mcp.ServerOptions{
		Instructions: "codewiki answers questions about the indexed codebase from a parsed index. " +
			"Start with get_architecture_overview on an unfamiliar codebase, and list_apis for HTTP endpoints. " +
			"Use search_code to find where something is implemented, get_symbol to read its exact signature, " +
			"doc, source, callers and callees, and get_dependency_graph to trace what a function depends on " +
			"or what depends on it, get_implementations to find what implements an interface or overrides a " +
			"method (where an interface call actually goes), find_tests to see which tests exercise code " +
			"before changing it, and get_file_summary for an overview of a file " +
			"before reading it. " +
			"Prefer these tools over grepping or reading whole files.",
	})
	h := handlers{st}
	addTool(s, opts.Ready, &mcp.Tool{
		Name: "search_code",
		Description: "Ranked keyword search over the indexed codebase's functions, methods and types. " +
			"Matches names (including camelCase and snake_case parts), signatures and doc comments. " +
			"Returns qualified names and file:line locations.",
	}, h.searchCode)
	addTool(s, opts.Ready, &mcp.Tool{
		Name: "get_symbol",
		Description: "Full details of a function, method or type: signature, doc comment, file and line range, " +
			"source code (first 200 lines), and its direct callers and callees. Accepts a qualified name from " +
			"search_code, or a short name; a short name that matches several symbols returns the list of " +
			"candidates to choose from.",
	}, h.getSymbol)
	addTool(s, opts.Ready, &mcp.Tool{
		Name: "get_dependency_graph",
		Description: "Call graph around a symbol: what it calls (callees) or what calls it (callers), " +
			"several levels deep. Pass an external package path such as 'database/sql' instead of a symbol " +
			"to list every place in the codebase that calls into it.",
	}, h.dependencyGraph)
	addTool(s, opts.Ready, &mcp.Tool{
		Name: "get_implementations",
		Description: "Type hierarchy around an interface, class or method. By default lists what implements " +
			"or extends a type, or what implements or overrides a method, several levels down; with direction " +
			"'up', what a type extends and implements, or which interface methods a method implements. Go " +
			"implementations are found from method sets (names plus parameter and result types); Java and " +
			"Python from declared extends/implements and base classes. Pass a type from outside the " +
			"repository, such as 'java.io.Serializable', to list the repository types that implement it.",
	}, h.implementations)
	addTool(s, opts.Ready, &mcp.Tool{
		Name: "find_tests",
		Description: "Tests that exercise a function, method or type: tests that call it directly, call " +
			"something that calls it (up to `depth` calls away through the repository's code), or reach it " +
			"through helper functions in test files. Nearest first, with the call each test goes through. " +
			"Use it to know which tests to run, or which to update, before changing code.",
	}, h.findTests)
	addTool(s, opts.Ready, &mcp.Tool{
		Name: "get_file_summary",
		Description: "Overview of one source file without reading it: package, doc comment, imports, every " +
			"declaration with its signature, and which other files it calls into or is called from. " +
			"Accepts a repository-relative path or a unique suffix such as 'store/query.go'.",
	}, h.fileSummary)
	addTool(s, opts.Ready, &mcp.Tool{
		Name: "get_architecture_overview",
		Description: "Project-level map of the indexed codebase: languages, main packages or directories with " +
			"their purpose, entry points (main functions, Python __main__ blocks), HTTP frameworks, how " +
			"packages depend on each other, the most-called functions, and the most-used external libraries. " +
			"Use it first when working in an unfamiliar repository.",
	}, h.overview)
	addTool(s, opts.Ready, &mcp.Tool{
		Name: "list_apis",
		Description: "HTTP endpoints defined in the code, with method, route, handler function and location. " +
			"Detects Go net/http, gin, echo, chi and fiber routes, Spring and JAX-RS annotations, and " +
			"Flask and FastAPI decorators, including router and group prefixes.",
	}, h.listAPIs)
	return s
}

// OverviewInput is the input of get_architecture_overview.
type OverviewInput struct {
	Limit int `json:"limit,omitempty" jsonschema:"maximum entries per section, default 15"`
}

func (h handlers) overview(ctx context.Context, req *mcp.CallToolRequest, in OverviewInput) (*mcp.CallToolResult, *store.Overview, error) {
	o, err := h.st.BuildOverview(min(max(in.Limit, 0), 50))
	if err != nil {
		return nil, nil, err
	}
	return text(FormatOverview(o)), o, nil
}

// APIsInput is the input of list_apis.
type APIsInput struct {
	Path   string `json:"path,omitempty" jsonschema:"optional: only routes containing this text, e.g. '/orders'"`
	Method string `json:"method,omitempty" jsonschema:"optional: GET, POST, PUT, PATCH or DELETE"`
}

// APIsOutput is the structured result of list_apis.
type APIsOutput struct {
	Endpoints []store.EndpointInfo `json:"endpoints"`
}

func (h handlers) listAPIs(ctx context.Context, req *mcp.CallToolRequest, in APIsInput) (*mcp.CallToolResult, APIsOutput, error) {
	eps, err := h.st.ListEndpoints(store.EndpointFilter{Path: in.Path, Method: in.Method, Limit: 300})
	if err != nil {
		return nil, APIsOutput{}, err
	}
	if len(eps) == 0 {
		return text("No HTTP endpoints found" + filterNote(in) + "."), APIsOutput{Endpoints: eps}, nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d endpoint(s)%s:\n\n", len(eps), filterNote(in))
	for _, e := range eps {
		loc := e.HandlerLocation
		if loc == "" {
			loc = fmt.Sprintf("registered at %s:%d", e.File, e.Line)
		}
		fmt.Fprintf(&b, "%-7s %s\n        -> %s (%s) [%s]\n", e.Method, e.Path, e.Handler, loc, e.Framework)
	}
	return text(b.String()), APIsOutput{Endpoints: eps}, nil
}

func filterNote(in APIsInput) string {
	var parts []string
	if in.Method != "" {
		parts = append(parts, "method "+strings.ToUpper(in.Method))
	}
	if in.Path != "" {
		parts = append(parts, fmt.Sprintf("route containing %q", in.Path))
	}
	if len(parts) == 0 {
		return ""
	}
	return " with " + strings.Join(parts, " and ")
}

// FormatOverview renders an overview as plain text.
func FormatOverview(o *store.Overview) string {
	var b strings.Builder
	langs := make([]string, 0, len(o.Languages))
	for _, l := range o.Languages {
		langs = append(langs, fmt.Sprintf("%s (%d files)", l.Name, l.Count))
	}
	fmt.Fprintf(&b, "Repository: %s\nLanguages: %s\nIndexed: %d files, %d symbols, %d call edges, %d HTTP endpoints\n",
		o.Root, strings.Join(langs, ", "), o.Files, o.Symbols, o.Edges, o.Endpoints)
	if len(o.Frameworks) > 0 {
		var fw []string
		for _, f := range o.Frameworks {
			fw = append(fw, fmt.Sprintf("%s (%d routes)", f.Name, f.Count))
		}
		fmt.Fprintf(&b, "HTTP frameworks: %s\n", strings.Join(fw, ", "))
	}
	fmt.Fprintf(&b, "\nPackages and directories:\n")
	for _, p := range o.Packages {
		doc := ""
		if p.Doc != "" {
			doc = " - " + p.Doc
		}
		fmt.Fprintf(&b, "- %s [%s, %d file(s), %d symbols]%s\n", p.Dir, p.Lang, p.Files, p.Symbols, doc)
	}
	if len(o.EntryPoints) > 0 {
		fmt.Fprintf(&b, "\nEntry points:\n")
		for _, e := range o.EntryPoints {
			fmt.Fprintf(&b, "- %s at %s\n", e.Qualified, e.Location)
		}
	}
	if len(o.Dependencies) > 0 {
		fmt.Fprintf(&b, "\nPackage dependencies (calls from -> into):\n")
		for _, d := range o.Dependencies {
			fmt.Fprintf(&b, "- %s -> %s (%d)\n", d.From, d.To, d.Calls)
		}
	}
	if len(o.Hotspots) > 0 {
		fmt.Fprintf(&b, "\nMost-called functions (distinct callers):\n")
		for _, c := range o.Hotspots {
			fmt.Fprintf(&b, "- %s (%d)\n", c.Name, c.Count)
		}
	}
	if len(o.External) > 0 {
		fmt.Fprintf(&b, "\nMost-used external packages (call sites):\n")
		for _, c := range o.External {
			fmt.Fprintf(&b, "- %s (%d)\n", c.Name, c.Count)
		}
	}
	return b.String()
}

// FileInput is the input of get_file_summary.
type FileInput struct {
	Path string `json:"path" jsonschema:"repository-relative file path, or a unique suffix of one"`
}

// FileOutput is the structured result of get_file_summary. Related files
// are listed most-connected first; MoreDependsOn and MoreUsedBy count the
// ones left out.
type FileOutput struct {
	Summary       *store.FileSummary `json:"summary,omitempty"`
	MoreDependsOn int                `json:"more_depends_on,omitempty"`
	MoreUsedBy    int                `json:"more_used_by,omitempty"`
	Candidates    []string           `json:"candidates,omitempty"`
}

func (h handlers) fileSummary(ctx context.Context, req *mcp.CallToolRequest, in FileInput) (*mcp.CallToolResult, FileOutput, error) {
	paths, err := h.st.FindFiles(in.Path)
	if err != nil {
		return nil, FileOutput{}, err
	}
	switch len(paths) {
	case 0:
		return text(fmt.Sprintf("No indexed file matches %q. Use search_code to find where something lives.", in.Path)), FileOutput{}, nil
	case 1:
	default:
		return text(fmt.Sprintf("%q matches %d files; call again with one of:\n- %s", in.Path, len(paths), strings.Join(paths, "\n- "))),
			FileOutput{Candidates: paths}, nil
	}
	fs, err := h.st.FileSummary(paths[0])
	if err != nil {
		return nil, FileOutput{}, err
	}
	out := FileOutput{Summary: fs}
	if n := len(fs.DependsOn); n > maxFileLinks {
		fs.DependsOn, out.MoreDependsOn = fs.DependsOn[:maxFileLinks], n-maxFileLinks
	}
	if n := len(fs.UsedBy); n > maxFileLinks {
		fs.UsedBy, out.MoreUsedBy = fs.UsedBy[:maxFileLinks], n-maxFileLinks
	}
	return text(formatFile(out)), out, nil
}

func formatFile(out FileOutput) string {
	fs := out.Summary
	var b strings.Builder
	fmt.Fprintf(&b, "%s (%s, package %s)\n", fs.Path, fs.Lang, fs.Package)
	if fs.Doc != "" {
		fmt.Fprintf(&b, "\n%s\n", fs.Doc)
	}
	if len(fs.Imports) > 0 {
		fmt.Fprintf(&b, "\nImports: %s\n", strings.Join(fs.Imports, ", "))
	}
	fmt.Fprintf(&b, "\nDeclarations (%d):\n", len(fs.Symbols))
	for _, d := range fs.Symbols {
		fmt.Fprintf(&b, "- line %d: %s\n", d.Line, firstLine(d.Signature))
		if d.Doc != "" {
			fmt.Fprintf(&b, "    %s\n", d.Doc)
		}
	}
	link := func(title string, ls []store.FileLink, more int) {
		if len(ls) == 0 {
			return
		}
		fmt.Fprintf(&b, "\n%s:\n", title)
		for _, l := range ls {
			fmt.Fprintf(&b, "- %s (%d call site(s))\n", l.Path, l.Calls)
		}
		if more > 0 {
			fmt.Fprintf(&b, "- ... and %d more file(s) with fewer call sites\n", more)
		}
	}
	link("Calls into", fs.DependsOn, out.MoreDependsOn)
	link("Called from", fs.UsedBy, out.MoreUsedBy)
	return b.String()
}

type handlers struct{ st *store.Store }

// SearchInput is the input of search_code.
type SearchInput struct {
	Query string `json:"query" jsonschema:"words to search for in symbol names, signatures and doc comments, e.g. 'rate limiter' or 'ParseConfig'"`
	Kind  string `json:"kind,omitempty" jsonschema:"optional filter: func, method, struct, interface, type, class, record, enum, constructor"`
	Lang  string `json:"lang,omitempty" jsonschema:"optional filter: go, java or python"`
	Path  string `json:"path,omitempty" jsonschema:"optional: only files under this directory, e.g. 'internal/store'"`
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

func (h handlers) searchCode(ctx context.Context, req *mcp.CallToolRequest, in SearchInput) (*mcp.CallToolResult, SearchOutput, error) {
	limit := min(max(in.Limit, 0), 50)
	if limit == 0 {
		limit = 10
	}
	syms, err := h.st.Search(in.Query, store.SearchOptions{Limit: limit, Kind: in.Kind, Lang: in.Lang, Path: in.Path})
	if err != nil {
		return nil, SearchOutput{}, err
	}
	out := SearchOutput{Results: make([]SymbolSummary, 0, len(syms))}
	for _, sym := range syms {
		out.Results = append(out.Results, summarize(sym))
	}
	return text(formatSearch(in.Query, out)), out, nil
}

// SymbolInput is the input of get_symbol.
type SymbolInput struct {
	Name string `json:"name" jsonschema:"qualified name like 'internal/store.Store.Search', or a short name like 'Search'"`
}

// SymbolDetail is a symbol with its direct neighbours in the call graph.
type SymbolDetail struct {
	store.Symbol
	BodyOmittedLines int         `json:"body_omitted_lines,omitempty"`
	Callers          []store.Ref `json:"callers"`
	Callees          []store.Ref `json:"callees"`
	MoreCallers      int         `json:"more_callers,omitempty"`
	MoreCallees      int         `json:"more_callees,omitempty"`
	ExternalUses     []string    `json:"external_calls,omitempty"`
	// Type hierarchy: for a type, what it extends or implements and what
	// extends or implements it; for a method, what it implements or
	// overrides and what implements or overrides it.
	Supertypes []store.TypeLink `json:"supertypes,omitempty"`
	Subtypes   []store.TypeLink `json:"subtypes,omitempty"`
	MoreSubs   int              `json:"more_subtypes,omitempty"`
	// Tests that exercise the symbol, nearest first.
	Tests     []store.TestRef `json:"tests,omitempty"`
	MoreTests int             `json:"more_tests,omitempty"`
}

var typeKinds = map[string]bool{"struct": true, "interface": true, "type": true, "class": true, "enum": true, "record": true}

func isMethodKind(kind string) bool { return kind == "method" || kind == "interface method" }

// hierarchy returns what s extends or implements (up) or what extends or
// implements it (down), for a type or a method; ok is false for other
// symbols.
func (h handlers) hierarchy(s store.Symbol, up bool, depth int) (links []store.TypeLink, ok bool, err error) {
	switch {
	case typeKinds[s.Kind] && up:
		links, err = h.st.Supertypes(s.ID, depth)
	case typeKinds[s.Kind]:
		links, err = h.st.Subtypes(s.ID, depth)
	case isMethodKind(s.Kind) && up:
		links, err = h.st.MethodSupers(s, depth)
	case isMethodKind(s.Kind):
		links, err = h.st.MethodImplementations(s, depth)
	default:
		return nil, false, nil
	}
	return links, true, err
}

// SymbolOutput is the structured result of get_symbol. A name that matches
// one symbol (or one overloaded Java method) fills Matches; an ambiguous
// name fills Candidates instead, without source, and Total says how many
// symbols matched.
type SymbolOutput struct {
	Matches    []SymbolDetail  `json:"matches"`
	Candidates []SymbolSummary `json:"candidates,omitempty"`
	Total      int             `json:"total,omitempty"`
}

func summarize(sym store.Symbol) SymbolSummary {
	return SymbolSummary{
		Qualified: sym.Qualified,
		Kind:      sym.Kind,
		Signature: sym.Signature,
		Doc:       firstLine(sym.Doc),
		Location:  fmt.Sprintf("%s:%d", sym.File, sym.StartLine),
	}
}

// truncateLines keeps the first n lines of s and reports how many it dropped.
func truncateLines(s string, n int) (string, int) {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) <= n {
		return s, 0
	}
	return strings.Join(lines[:n], "\n"), len(lines) - n
}

func (h handlers) getSymbol(ctx context.Context, req *mcp.CallToolRequest, in SymbolInput) (*mcp.CallToolResult, SymbolOutput, error) {
	name := strings.TrimSpace(in.Name)
	syms, total, err := h.st.LookupAll(name, maxCandidates)
	if err != nil {
		return nil, SymbolOutput{}, err
	}
	out := SymbolOutput{Matches: []SymbolDetail{}}
	// Decide on the total, not on the rows fetched: with more matches than
	// the limit, the fetched rows alone could look unambiguous.
	if total > len(syms) || (len(syms) > 1 && !store.SameName(syms)) {
		// Ambiguous: list the candidates rather than sending the source of
		// every one. The agent picks one and asks again.
		out.Total = total
		for _, s := range syms {
			out.Candidates = append(out.Candidates, summarize(s))
		}
		return text(formatCandidates(name, out)), out, nil
	}
	for _, s := range syms {
		d := SymbolDetail{Symbol: s, Callers: []store.Ref{}, Callees: []store.Ref{}}
		d.Body, d.BodyOmittedLines = truncateLines(s.Body, maxBodyLines)
		callers, err := h.st.Callers(s.ID)
		if err != nil {
			return nil, out, err
		}
		callees, err := h.st.Callees(s.ID, true)
		if err != nil {
			return nil, out, err
		}
		for _, c := range callees {
			if c.External {
				d.ExternalUses = append(d.ExternalUses, c.Qualified)
			} else {
				d.Callees = append(d.Callees, c)
			}
		}
		if callers != nil {
			d.Callers = callers
		}
		if len(d.Callers) > maxRefs {
			d.MoreCallers, d.Callers = len(d.Callers)-maxRefs, d.Callers[:maxRefs]
		}
		if len(d.Callees) > maxRefs {
			d.MoreCallees, d.Callees = len(d.Callees)-maxRefs, d.Callees[:maxRefs]
		}
		if d.Supertypes, _, err = h.hierarchy(s, true, 1); err != nil {
			return nil, out, err
		}
		if d.Subtypes, _, err = h.hierarchy(s, false, 5); err != nil {
			return nil, out, err
		}
		if len(d.Subtypes) > maxRefs {
			d.MoreSubs, d.Subtypes = len(d.Subtypes)-maxRefs, d.Subtypes[:maxRefs]
		}
		tests, total, err := h.st.TestsFor(s.ID, testCallers, testHelpers, maxSymTests)
		if err != nil {
			return nil, out, err
		}
		if len(tests) > 0 {
			d.Tests, d.MoreTests = tests, total-len(tests)
		}
		out.Matches = append(out.Matches, d)
	}
	return text(formatSymbols(in.Name, out.Matches)), out, nil
}

// GraphInput is the input of get_dependency_graph.
type GraphInput struct {
	Symbol          string `json:"symbol" jsonschema:"qualified or short symbol name, or an external package path like 'database/sql'"`
	Direction       string `json:"direction,omitempty" jsonschema:"'callees' (what it depends on, default) or 'callers' (what depends on it)"`
	Depth           int    `json:"depth,omitempty" jsonschema:"levels to expand, default 2, max 5"`
	IncludeExternal bool   `json:"include_external,omitempty" jsonschema:"also list calls into packages outside the repository"`
}

// GraphEdge is one edge of the dependency graph, flattened breadth-first.
// (MCP output schemas cannot describe recursive types, so the tree is sent
// as a list; the text content shows it indented.)
type GraphEdge struct {
	From       string `json:"from"`
	To         string `json:"to"`
	Depth      int    `json:"depth"`
	Location   string `json:"location,omitempty"`
	External   bool   `json:"external,omitempty"`
	Repeated   bool   `json:"repeated,omitempty"`
	Confidence string `json:"confidence"`
}

// GraphOutput is the structured result of get_dependency_graph.
type GraphOutput struct {
	Root      string      `json:"root,omitempty"`
	Direction string      `json:"direction,omitempty"`
	Edges     []GraphEdge `json:"edges"`
	External  []store.Ref `json:"external_callers,omitempty"`
}

func flatten(root *store.GraphNode, dir string) []GraphEdge {
	edges := []GraphEdge{}
	var walk func(n *store.GraphNode, depth int)
	walk = func(n *store.GraphNode, depth int) {
		for _, c := range n.Children {
			e := GraphEdge{From: n.Qualified, To: c.Qualified, Depth: depth, Location: c.Location,
				External: c.External, Repeated: c.Repeated, Confidence: c.Confidence}
			if dir == "callers" {
				e.From, e.To = c.Qualified, n.Qualified
			}
			edges = append(edges, e)
			walk(c, depth+1)
		}
	}
	walk(root, 1)
	return edges
}

func (h handlers) dependencyGraph(ctx context.Context, req *mcp.CallToolRequest, in GraphInput) (*mcp.CallToolResult, GraphOutput, error) {
	name := strings.TrimSpace(in.Symbol)
	dir := in.Direction
	if dir != "callers" {
		dir = "callees"
	}
	depth := min(max(in.Depth, 0), 5)
	if depth == 0 {
		depth = 2
	}
	syms, err := h.st.Lookup(name)
	if err != nil {
		return nil, GraphOutput{}, err
	}
	if len(syms) == 0 {
		// Not a symbol in the repo: treat it as an external package or member.
		ext, err := h.st.ExternalCallers(name, 100)
		if err != nil {
			return nil, GraphOutput{}, err
		}
		out := GraphOutput{Edges: []GraphEdge{}, External: ext}
		return text(formatExternal(name, ext)), out, nil
	}
	if len(syms) > 1 && store.SameName(syms) {
		// Overloads: edges point at the first declaration of the name.
		r, err := h.st.Lookup(syms[0].Qualified)
		if err == nil && len(r) > 0 {
			syms = r[:1]
		}
	}
	if len(syms) > 1 {
		var b strings.Builder
		fmt.Fprintf(&b, "%q matches %d symbols; call again with one of these qualified names:\n", name, len(syms))
		for _, s := range syms {
			fmt.Fprintf(&b, "- %s (%s) at %s:%d\n", s.Qualified, s.Kind, s.File, s.StartLine)
		}
		return text(b.String()), GraphOutput{Edges: []GraphEdge{}}, nil
	}
	root, n, err := h.st.Graph(syms[0], dir, depth, in.IncludeExternal, 300)
	if err != nil {
		return nil, GraphOutput{}, err
	}
	out := GraphOutput{Root: root.Qualified, Direction: dir, Edges: flatten(root, dir)}
	return text(formatGraph(root, dir, depth, n)), out, nil
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
		fmt.Fprintf(&b, "\n%d. %s (%s) at %s\n   %s\n", i+1, r.Qualified, r.Kind, r.Location, firstLine(r.Signature))
		if r.Doc != "" {
			fmt.Fprintf(&b, "   %s\n", r.Doc)
		}
	}
	return b.String()
}

func formatRefs(b *strings.Builder, title string, refs []store.Ref, more int) {
	if len(refs) == 0 {
		fmt.Fprintf(b, "\n%s: none found\n", title)
		return
	}
	fmt.Fprintf(b, "\n%s:\n", title)
	for _, r := range refs {
		note := ""
		if r.Confidence == "name" {
			note = " (matched by method name)"
		}
		fmt.Fprintf(b, "- %s at %s, called at %s:%d%s\n", r.Qualified, r.Location, r.CallFile, r.CallLine, note)
	}
	if more > 0 {
		fmt.Fprintf(b, "- ... and %d more (use get_dependency_graph)\n", more)
	}
}

func formatSymbols(name string, syms []SymbolDetail) string {
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
		fmt.Fprintf(&b, "\n```%s\n%s\n```\n", s.Lang, s.Body)
		if s.BodyOmittedLines > 0 {
			fmt.Fprintf(&b, "(%d more line(s) not shown: read %s lines %d-%d)\n",
				s.BodyOmittedLines, s.File, s.EndLine-s.BodyOmittedLines+1, s.EndLine)
		}
		formatRefs(&b, "Called by", s.Callers, s.MoreCallers)
		formatRefs(&b, "Calls", s.Callees, s.MoreCallees)
		if len(s.ExternalUses) > 0 {
			fmt.Fprintf(&b, "\nExternal calls: %s\n", strings.Join(s.ExternalUses, ", "))
		}
		up, down := "Extends / implements", "Extended / implemented by"
		if isMethodKind(s.Kind) {
			up, down = "Implements / overrides", "Implemented / overridden by"
		}
		formatLinks(&b, up, s.Supertypes, 0)
		formatLinks(&b, down, s.Subtypes, s.MoreSubs)
		formatTests(&b, "Tested by", s.Tests, s.MoreTests, "use find_tests")
	}
	return b.String()
}

// formatLinks lists type hierarchy links under a title; nothing when empty.
func formatLinks(b *strings.Builder, title string, links []store.TypeLink, more int) {
	if len(links) == 0 {
		return
	}
	fmt.Fprintf(b, "\n%s:\n", title)
	for _, l := range links {
		indent := strings.Repeat("  ", max(l.Depth-1, 0))
		switch {
		case l.External:
			fmt.Fprintf(b, "%s- %s (%s, outside the repository)\n", indent, l.Qualified, l.How)
		default:
			fmt.Fprintf(b, "%s- %s (%s) at %s\n", indent, l.Qualified, l.How, l.Location)
		}
	}
	if more > 0 {
		fmt.Fprintf(b, "- ... and %d more (use get_implementations)\n", more)
	}
}

// formatTests lists tests under a title; nothing when there are none.
func formatTests(b *strings.Builder, title string, tests []store.TestRef, more int, hint string) {
	if len(tests) == 0 {
		return
	}
	fmt.Fprintf(b, "\n%s:\n", title)
	for _, t := range tests {
		how := "calls it directly"
		if t.Distance > 1 {
			how = fmt.Sprintf("%d calls away, through %s", t.Distance, t.Via)
		}
		fmt.Fprintf(b, "- %s at %s (%s)\n", t.Qualified, t.Location, how)
	}
	if more > 0 {
		fmt.Fprintf(b, "- ... and %d more (%s)\n", more, hint)
	}
}

// TestsInput is the input of find_tests.
type TestsInput struct {
	Symbol string `json:"symbol" jsonschema:"qualified or short name of a function, method or type"`
	Depth  int    `json:"depth,omitempty" jsonschema:"calls to follow up through the repository's code, default 3, max 6"`
}

// TestsOutput is the structured result of find_tests.
type TestsOutput struct {
	Root       string          `json:"root,omitempty"`
	Tests      []store.TestRef `json:"tests"`
	More       int             `json:"more,omitempty"`
	Candidates []SymbolSummary `json:"candidates,omitempty"`
	Total      int             `json:"total,omitempty"`
}

func (h handlers) findTests(ctx context.Context, req *mcp.CallToolRequest, in TestsInput) (*mcp.CallToolResult, TestsOutput, error) {
	name := strings.TrimSpace(in.Symbol)
	out := TestsOutput{Tests: []store.TestRef{}}
	depth := min(max(in.Depth, 0), 6)
	if depth == 0 {
		depth = testCallers
	}
	syms, total, err := h.st.LookupAll(name, maxCandidates)
	if err != nil {
		return nil, out, err
	}
	if len(syms) == 0 {
		return text(fmt.Sprintf("No symbol named %q. Use search_code to find the name.", name)), out, nil
	}
	if total > len(syms) || (len(syms) > 1 && !store.SameName(syms)) {
		out.Total = total
		for _, s := range syms {
			out.Candidates = append(out.Candidates, summarize(s))
		}
		so := SymbolOutput{Candidates: out.Candidates, Total: total}
		return text(strings.Replace(formatCandidates(name, so), "call get_symbol again", "call find_tests again", 1)), out, nil
	}
	s := syms[0]
	out.Root = s.Qualified
	tests, n, err := h.st.TestsFor(s.ID, depth, testHelpers, maxTests)
	if err != nil {
		return nil, out, err
	}
	out.Tests, out.More = tests, n-len(tests)
	if len(tests) == 0 {
		files, _, err := h.st.TestCount()
		if err != nil {
			return nil, out, err
		}
		if files == 0 {
			return text("No test files are indexed for this repository."), out, nil
		}
		return text(fmt.Sprintf("No test reaches %s within %d call(s) through the repository's code. "+
			"Calls the index cannot resolve (through function values, or ambiguous method names) are not followed.",
			s.Qualified, depth)), out, nil
	}
	var b strings.Builder
	formatTests(&b, fmt.Sprintf("%d test(s) exercise %s", n, s.Qualified), tests, out.More, "only the nearest are listed")
	return text(strings.TrimPrefix(b.String(), "\n")), out, nil
}

// ImplInput is the input of get_implementations.
type ImplInput struct {
	Symbol    string `json:"symbol" jsonschema:"qualified or short name of an interface, class, type or method, or a type outside the repository such as 'java.io.Serializable'"`
	Direction string `json:"direction,omitempty" jsonschema:"'down' (what implements, extends or overrides it; default) or 'up' (what it implements, extends or overrides)"`
	Depth     int    `json:"depth,omitempty" jsonschema:"levels to follow, default 5, max 10"`
	Path      string `json:"path,omitempty" jsonschema:"optional: only results in files under this directory, e.g. 'net/http'"`
}

// ImplOutput is the structured result of get_implementations.
type ImplOutput struct {
	Root       string           `json:"root,omitempty"`
	Kind       string           `json:"kind,omitempty"`
	Direction  string           `json:"direction"`
	Results    []store.TypeLink `json:"results"`
	More       int              `json:"more,omitempty"` // results left out by the limit
	Candidates []SymbolSummary  `json:"candidates,omitempty"`
	Total      int              `json:"total,omitempty"`
}

// limitLinks applies the optional path filter and the result limit.
func limitLinks(links []store.TypeLink, path string) ([]store.TypeLink, int) {
	if path = strings.Trim(strings.TrimSpace(path), "/"); path != "" {
		kept := []store.TypeLink{}
		for _, l := range links {
			if strings.HasPrefix(l.Location, path+"/") {
				kept = append(kept, l)
			}
		}
		links = kept
	}
	if len(links) > maxImpls {
		return links[:maxImpls], len(links) - maxImpls
	}
	return links, 0
}

func (h handlers) implementations(ctx context.Context, req *mcp.CallToolRequest, in ImplInput) (*mcp.CallToolResult, ImplOutput, error) {
	name := strings.TrimSpace(in.Symbol)
	up := in.Direction == "up"
	out := ImplOutput{Direction: "down", Results: []store.TypeLink{}}
	if up {
		out.Direction = "up"
	}
	depth := min(max(in.Depth, 0), 10)
	if depth == 0 {
		depth = 5
	}
	syms, total, err := h.st.LookupAll(name, maxCandidates)
	if err != nil {
		return nil, out, err
	}
	if len(syms) == 0 {
		if up {
			return text(fmt.Sprintf("No symbol named %q. Use search_code to find the name.", name)), out, nil
		}
		// Not in the repository: list the repository types implementing it.
		ext, err := h.st.ExternalSubtypes(name)
		if err != nil {
			return nil, out, err
		}
		out.Root = name
		out.Results, out.More = limitLinks(ext, in.Path)
		ext = out.Results
		if len(ext) == 0 {
			return text(fmt.Sprintf("No symbol named %q, and no repository type extends or implements a type of that name. Use search_code to find the name.", name)), out, nil
		}
		var b strings.Builder
		fmt.Fprintf(&b, "%d repository type(s) extend or implement %s (outside the repository):\n", len(ext)+out.More, name)
		for _, l := range ext {
			fmt.Fprintf(&b, "- %s (%s) at %s\n", l.Qualified, l.How, l.Location)
		}
		moreNote(&b, out.More)
		return text(b.String()), out, nil
	}
	if total > len(syms) || (len(syms) > 1 && !store.SameName(syms)) {
		out.Total = total
		for _, s := range syms {
			out.Candidates = append(out.Candidates, summarize(s))
		}
		so := SymbolOutput{Candidates: out.Candidates, Total: total}
		return text(strings.Replace(formatCandidates(name, so), "call get_symbol again", "call get_implementations again", 1)), out, nil
	}
	s := syms[0]
	out.Root, out.Kind = s.Qualified, s.Kind
	links, ok, err := h.hierarchy(s, up, depth)
	if err != nil {
		return nil, out, err
	}
	if !ok {
		return text(fmt.Sprintf("%s is a %s; implementations apply to interfaces, classes, types and methods.", s.Qualified, s.Kind)), out, nil
	}
	out.Results, out.More = limitLinks(links, in.Path)
	links = out.Results
	var b strings.Builder
	title := fmt.Sprintf("What %s (%s) is extended, implemented or overridden by", s.Qualified, s.Kind)
	if up {
		title = fmt.Sprintf("What %s (%s) extends, implements or overrides", s.Qualified, s.Kind)
	}
	if len(links) == 0 {
		fmt.Fprintf(&b, "%s: nothing found in the index.\n", title)
		if !up && (s.Kind == "interface" || s.Kind == "interface method") {
			b.WriteString("Implementations in generated files or outside the repository are not indexed.\n")
		}
		return text(b.String()), out, nil
	}
	formatLinks(&b, title, links, 0)
	moreNote(&b, out.More)
	return text(strings.TrimPrefix(b.String(), "\n")), out, nil
}

func moreNote(b *strings.Builder, more int) {
	if more > 0 {
		fmt.Fprintf(b, "- ... and %d more; pass path to narrow the list to one directory\n", more)
	}
}

func formatCandidates(name string, out SymbolOutput) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%q matches %d symbols; call get_symbol again with one of these qualified names:\n", name, out.Total)
	for _, c := range out.Candidates {
		fmt.Fprintf(&b, "- %s (%s) at %s\n    %s\n", c.Qualified, c.Kind, c.Location, firstLine(c.Signature))
	}
	if more := out.Total - len(out.Candidates); more > 0 {
		fmt.Fprintf(&b, "- ... and %d more; use search_code with kind or path filters to narrow it down\n", more)
	}
	return b.String()
}

func formatGraph(root *store.GraphNode, dir string, depth, n int) string {
	var b strings.Builder
	verb := "calls"
	if dir == "callers" {
		verb = "is called by"
	}
	fmt.Fprintf(&b, "%s %s (%d node(s), depth %d):\n\n%s (%s) at %s\n", root.Qualified, verb, n, depth, root.Qualified, root.Kind, root.Location)
	var walk func(nodes []*store.GraphNode, indent string)
	walk = func(nodes []*store.GraphNode, indent string) {
		for _, c := range nodes {
			loc := c.Location
			if c.External {
				loc = "external"
			}
			suffix := ""
			if c.Repeated {
				suffix = " (already shown)"
			}
			if c.Confidence == "name" {
				suffix += " (matched by method name)"
			}
			fmt.Fprintf(&b, "%s- %s [%s]%s\n", indent, c.Qualified, loc, suffix)
			walk(c.Children, indent+"  ")
		}
	}
	walk(root.Children, "  ")
	if n == 0 {
		b.WriteString("  (no edges found)\n")
	}
	return b.String()
}

func formatExternal(name string, refs []store.Ref) string {
	if len(refs) == 0 {
		return fmt.Sprintf("No symbol named %q, and no calls into an external package with that path. Use search_code to find the name.", name)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d call site(s) into %s:\n", len(refs), name)
	for _, r := range refs {
		fmt.Fprintf(&b, "- %s %s at %s:%d\n", r.Qualified, r.Confidence, r.CallFile, r.CallLine)
	}
	return b.String()
}
