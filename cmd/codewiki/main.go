// Command codewiki indexes a codebase and serves it to AI coding agents over MCP.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/anjanikumarjangid/codewiki/internal/indexer"
	"github.com/anjanikumarjangid/codewiki/internal/server"
	"github.com/anjanikumarjangid/codewiki/internal/store"
)

const usage = `codewiki: structured codebase context for AI coding agents

Usage:
  codewiki index  [--db FILE] [--include-tests] [--exclude dir1,dir2] PATH
  codewiki query  [--db FILE] [--kind KIND] [--limit N] [--json] TEXT...
  codewiki symbol [--db FILE] [--json] NAME
  codewiki reindex [--db FILE] [--include-tests] [--exclude dir1,dir2] [PATH]
  codewiki deps   [--db FILE] [--callers] [--depth N] [--external] NAME
  codewiki impl   [--db FILE] [--up] [--depth N] NAME
  codewiki file   [--db FILE] [--json] PATH
  codewiki apis   [--db FILE] [--method M] [--json] [ROUTE]
  codewiki overview [--db FILE] [--json]
  codewiki watch  [--db FILE] [--interval 2s] [PATH]
  codewiki serve  [--db FILE] [--root PATH] [--watch=false]

By default the index is stored at PATH/.codewiki/index.db, and other commands
look for .codewiki/index.db in the current directory. reindex, watch and
serve --watch reuse the --include-tests and --exclude settings the index was
built with; flags passed to reindex override them.

serve needs no flags when started inside a repository: it finds the
repository root (the nearest directory with .codewiki/ or .git), builds the
index in the background if there is none, and keeps it up to date.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "index":
		err = runIndex(os.Args[2:])
	case "query":
		err = runQuery(os.Args[2:])
	case "symbol":
		err = runSymbol(os.Args[2:])
	case "deps":
		err = runDeps(os.Args[2:])
	case "impl":
		err = runImpl(os.Args[2:])
	case "reindex":
		err = runReindex(os.Args[2:])
	case "file":
		err = runFile(os.Args[2:])
	case "apis":
		err = runAPIs(os.Args[2:])
	case "overview":
		err = runOverview(os.Args[2:])
	case "watch":
		err = runWatch(os.Args[2:])
	case "serve":
		err = runServe(os.Args[2:])
	case "version", "--version":
		fmt.Println("codewiki", server.Version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		err = fmt.Errorf("unknown command %q\n\n%s", os.Args[1], usage)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "codewiki:", err)
		os.Exit(1)
	}
}

const defaultDB = ".codewiki/index.db"

func runIndex(args []string) error {
	fs := flag.NewFlagSet("index", flag.ExitOnError)
	db := fs.String("db", "", "index file (default PATH/"+defaultDB+")")
	tests := fs.Bool("include-tests", false, "also index *_test.go files")
	exclude := fs.String("exclude", "", "comma-separated directory names to skip")
	fs.Parse(interspersed(fs, args)) //nolint:errcheck // ExitOnError
	if fs.NArg() != 1 {
		return errors.New("index needs exactly one PATH")
	}
	root := fs.Arg(0)
	if *db == "" {
		*db = filepath.Join(root, defaultDB)
	}
	st, err := store.Open(*db)
	if err != nil {
		return err
	}
	defer st.Close()
	ensureGitignore(filepath.Dir(*db))

	opts := indexer.Options{IncludeTests: *tests, Exclude: splitList(*exclude)}
	start := time.Now()
	res, err := indexer.Index(root, st, opts)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "indexed %d files, %d symbols, %d call edges (%d skipped) in %s\nindex: %s\n",
		res.Files, res.Symbols, res.Edges, res.Skipped, time.Since(start).Round(time.Millisecond), *db)
	return nil
}

func openExisting(path string) (*store.Store, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("no index at %s (run `codewiki index PATH` first, or pass --db)", path)
	}
	return store.Open(path)
}

func runQuery(args []string) error {
	fs := flag.NewFlagSet("query", flag.ExitOnError)
	db := fs.String("db", defaultDB, "index file")
	kind := fs.String("kind", "", "filter: func, method, struct, interface, type")
	limit := fs.Int("limit", 10, "maximum results")
	asJSON := fs.Bool("json", false, "print JSON")
	fs.Parse(interspersed(fs, args)) //nolint:errcheck
	if fs.NArg() == 0 {
		return errors.New("query needs search text")
	}
	st, err := openExisting(*db)
	if err != nil {
		return err
	}
	defer st.Close()
	syms, err := st.Search(strings.Join(fs.Args(), " "), store.SearchOptions{Limit: *limit, Kind: *kind})
	if err != nil {
		return err
	}
	if *asJSON {
		return printJSON(syms)
	}
	if len(syms) == 0 {
		fmt.Println("no matches")
	}
	for _, s := range syms {
		fmt.Printf("%-50s %-9s %s:%d\n    %s\n", s.Qualified, s.Kind, s.File, s.StartLine, firstLine(s.Signature))
	}
	return nil
}

func runSymbol(args []string) error {
	fs := flag.NewFlagSet("symbol", flag.ExitOnError)
	db := fs.String("db", defaultDB, "index file")
	asJSON := fs.Bool("json", false, "print JSON")
	fs.Parse(interspersed(fs, args)) //nolint:errcheck
	if fs.NArg() != 1 {
		return errors.New("symbol needs exactly one NAME")
	}
	st, err := openExisting(*db)
	if err != nil {
		return err
	}
	defer st.Close()
	syms, err := st.Lookup(fs.Arg(0))
	if err != nil {
		return err
	}
	if *asJSON {
		return printJSON(syms)
	}
	if len(syms) == 0 {
		return fmt.Errorf("no symbol named %q", fs.Arg(0))
	}
	for i, s := range syms {
		if i > 0 {
			fmt.Println("---")
		}
		fmt.Printf("%s (%s)  %s:%d-%d\n", s.Qualified, s.Kind, s.File, s.StartLine, s.EndLine)
		if s.Doc != "" {
			fmt.Printf("\n%s\n", s.Doc)
		}
		fmt.Printf("\n%s\n", s.Body)
	}
	return nil
}

// runReindex re-parses only changed files. PATH defaults to the root
// recorded in the index.
func runReindex(args []string) error {
	fs := flag.NewFlagSet("reindex", flag.ExitOnError)
	db := fs.String("db", defaultDB, "index file")
	tests := fs.Bool("include-tests", false, "also index test files")
	exclude := fs.String("exclude", "", "comma-separated directory names to skip")
	fs.Parse(interspersed(fs, args)) //nolint:errcheck
	st, err := openExisting(*db)
	if err != nil {
		return err
	}
	defer st.Close()
	root := fs.Arg(0)
	if root == "" {
		if root, err = st.Meta("root"); err != nil || root == "" {
			return errors.New("no PATH given and none recorded in the index")
		}
	}
	// Start from the options the index was built with; flags given on this
	// command line override them (and are recorded for later runs).
	opts, err := indexer.StoredOptions(st)
	if err != nil {
		return err
	}
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "include-tests":
			opts.IncludeTests = *tests
		case "exclude":
			opts.Exclude = splitList(*exclude)
		}
	})
	start := time.Now()
	res, err := indexer.Reindex(root, st, opts)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "re-indexed %d changed file(s), %d unchanged, %d removed; %d call edges; %s\n",
		res.Files, res.Unchanged, res.Removed, res.Edges, time.Since(start).Round(time.Millisecond))
	return nil
}

func runFile(args []string) error {
	fs := flag.NewFlagSet("file", flag.ExitOnError)
	db := fs.String("db", defaultDB, "index file")
	asJSON := fs.Bool("json", false, "print JSON")
	fs.Parse(interspersed(fs, args)) //nolint:errcheck
	if fs.NArg() != 1 {
		return errors.New("file needs exactly one PATH")
	}
	st, err := openExisting(*db)
	if err != nil {
		return err
	}
	defer st.Close()
	paths, err := st.FindFiles(fs.Arg(0))
	if err != nil {
		return err
	}
	if len(paths) != 1 {
		if len(paths) == 0 {
			return fmt.Errorf("no indexed file matches %q", fs.Arg(0))
		}
		return fmt.Errorf("%q matches several files: %s", fs.Arg(0), strings.Join(paths, ", "))
	}
	sum, err := st.FileSummary(paths[0])
	if err != nil {
		return err
	}
	if *asJSON {
		return printJSON(sum)
	}
	fmt.Printf("%s (%s, package %s)\n", sum.Path, sum.Lang, sum.Package)
	if sum.Doc != "" {
		fmt.Printf("\n%s\n", sum.Doc)
	}
	fmt.Printf("\nimports: %s\n\n", strings.Join(sum.Imports, ", "))
	for _, d := range sum.Symbols {
		fmt.Printf("%5d  %s\n", d.Line, firstLine(d.Signature))
	}
	for _, l := range sum.DependsOn {
		fmt.Printf("calls into   %s (%d)\n", l.Path, l.Calls)
	}
	for _, l := range sum.UsedBy {
		fmt.Printf("called from  %s (%d)\n", l.Path, l.Calls)
	}
	return nil
}

func runDeps(args []string) error {
	fs := flag.NewFlagSet("deps", flag.ExitOnError)
	db := fs.String("db", defaultDB, "index file")
	callers := fs.Bool("callers", false, "show what calls NAME instead of what it calls")
	depth := fs.Int("depth", 2, "levels to expand")
	external := fs.Bool("external", false, "include calls into packages outside the repository")
	fs.Parse(interspersed(fs, args)) //nolint:errcheck
	if fs.NArg() != 1 {
		return errors.New("deps needs exactly one NAME")
	}
	st, err := openExisting(*db)
	if err != nil {
		return err
	}
	defer st.Close()
	syms, err := st.Lookup(fs.Arg(0))
	if err != nil {
		return err
	}
	if len(syms) > 1 && store.SameName(syms) {
		syms = syms[:1] // overloads share one node in the graph
	}
	if len(syms) != 1 {
		if len(syms) == 0 {
			return fmt.Errorf("no symbol named %q", fs.Arg(0))
		}
		for _, s := range syms {
			fmt.Println(s.Qualified)
		}
		return fmt.Errorf("%q is ambiguous; use one of the qualified names above", fs.Arg(0))
	}
	dir := "callees"
	if *callers {
		dir = "callers"
	}
	root, _, err := st.Graph(syms[0], dir, *depth, *external, 500)
	if err != nil {
		return err
	}
	fmt.Printf("%s (%s) %s\n", root.Qualified, root.Kind, root.Location)
	var walk func([]*store.GraphNode, string)
	walk = func(nodes []*store.GraphNode, indent string) {
		for _, n := range nodes {
			loc := n.Location
			if n.External {
				loc = "external"
			}
			mark := ""
			if n.Repeated {
				mark = " (repeated)"
			}
			fmt.Printf("%s%s  %s%s\n", indent, n.Qualified, loc, mark)
			walk(n.Children, indent+"  ")
		}
	}
	walk(root.Children, "  ")
	return nil
}

// runImpl prints what implements, extends or overrides NAME, or with --up,
// what NAME implements, extends or overrides.
func runImpl(args []string) error {
	fs := flag.NewFlagSet("impl", flag.ExitOnError)
	db := fs.String("db", defaultDB, "index file")
	up := fs.Bool("up", false, "show what NAME extends, implements or overrides")
	depth := fs.Int("depth", 5, "levels to follow")
	asJSON := fs.Bool("json", false, "print JSON")
	fs.Parse(interspersed(fs, args)) //nolint:errcheck
	if fs.NArg() != 1 {
		return errors.New("impl needs exactly one NAME")
	}
	st, err := openExisting(*db)
	if err != nil {
		return err
	}
	defer st.Close()
	syms, err := st.Lookup(fs.Arg(0))
	if err != nil {
		return err
	}
	if len(syms) > 1 && store.SameName(syms) {
		syms = syms[:1]
	}
	var links []store.TypeLink
	switch {
	case len(syms) == 0 && !*up:
		links, err = st.ExternalSubtypes(fs.Arg(0))
	case len(syms) == 0:
		return fmt.Errorf("no symbol named %q", fs.Arg(0))
	case len(syms) > 1:
		for _, s := range syms {
			fmt.Println(s.Qualified)
		}
		return fmt.Errorf("%q is ambiguous; use one of the qualified names above", fs.Arg(0))
	default:
		s := syms[0]
		switch {
		case s.Kind == "method" || s.Kind == "interface method":
			if *up {
				links, err = st.MethodSupers(s, *depth)
			} else {
				links, err = st.MethodImplementations(s, *depth)
			}
		case *up:
			links, err = st.Supertypes(s.ID, *depth)
		default:
			links, err = st.Subtypes(s.ID, *depth)
		}
	}
	if err != nil {
		return err
	}
	if *asJSON {
		return printJSON(links)
	}
	for _, l := range links {
		loc := l.Location
		if l.External {
			loc = "external"
		}
		fmt.Printf("%s%-50s %-11s %s\n", strings.Repeat("  ", max(l.Depth-1, 0)), l.Qualified, l.How, loc)
	}
	if len(links) == 0 {
		fmt.Println("nothing found")
	}
	return nil
}

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	db := fs.String("db", "", "index file (default ROOT/"+defaultDB+")")
	rootFlag := fs.String("root", "", "repository to serve (default: found from the current directory)")
	watch := fs.Bool("watch", true, "keep the index up to date while serving (--watch=false to serve it as is)")
	fs.Parse(interspersed(fs, args)) //nolint:errcheck
	cfg, err := resolveServe(*db, *rootFlag)
	if err != nil {
		return err
	}
	st, err := store.Open(cfg.db)
	if err != nil {
		return err
	}
	defer st.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ready := startIndexing(ctx, st, cfg, *watch, func(msg string) { fmt.Fprintln(os.Stderr, "codewiki:", msg) })
	// stdout carries the MCP protocol; anything else must go to stderr.
	return server.New(st, server.Options{Ready: ready}).Run(ctx, &mcp.StdioTransport{})
}

// serveConfig says which repository serve answers for and where its index is.
type serveConfig struct {
	root   string // repository root; may be "" only when the index records one
	db     string
	exists bool // an index file already exists at db
}

// resolveServe works out the repository and index for serve, so that a
// client can start `codewiki serve` from anywhere inside a repository with
// no flags. With --db the index's recorded root is used unless --root is
// given; otherwise the root is found by walking up from --root or the
// current directory, and the index lives at ROOT/.codewiki/index.db.
func resolveServe(db, root string) (serveConfig, error) {
	if db != "" {
		_, err := os.Stat(db)
		cfg := serveConfig{db: db, exists: err == nil}
		if root != "" {
			abs, err := filepath.Abs(root)
			cfg.root = abs
			return cfg, err
		}
		if !cfg.exists {
			return cfg, fmt.Errorf("no index at %s; pass --root to build one there", db)
		}
		return cfg, nil
	}
	start := root
	if start == "" {
		var err error
		if start, err = os.Getwd(); err != nil {
			return serveConfig{}, err
		}
	}
	start, err := filepath.Abs(start)
	if err != nil {
		return serveConfig{}, err
	}
	if root == "" {
		start = findRoot(start)
	}
	db = filepath.Join(start, defaultDB)
	_, err = os.Stat(db)
	return serveConfig{root: start, db: db, exists: err == nil}, nil
}

// findRoot returns the nearest directory at or above dir that holds a
// codewiki index or a .git entry, or dir itself if there is none.
func findRoot(dir string) string {
	for d := dir; ; {
		if _, err := os.Stat(filepath.Join(d, defaultDB)); err == nil {
			return d
		}
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			return d
		}
		parent := filepath.Dir(d)
		if parent == d {
			return dir
		}
		d = parent
	}
}

// startIndexing brings the index up to date in the background and returns
// the readiness check for the MCP server. The server starts answering the
// protocol right away, so a large repository cannot time out the client's
// startup handshake.
//
//   - No index yet: build it. Until it is built every tool reports that,
//     instead of returning empty results the agent would trust.
//   - Existing index with watch: indexer.Watch catches up on edits made
//     while nothing was running, then keeps watching. Queries are answered
//     from the existing index meanwhile.
//   - Existing index without watch: serve it as it is.
func startIndexing(ctx context.Context, st *store.Store, cfg serveConfig, watch bool, logf func(string)) func() error {
	var status atomic.Pointer[error]
	ready := func() error {
		if p := status.Load(); p != nil {
			return *p
		}
		return nil
	}
	setStatus := func(err error) {
		if err == nil {
			status.Store(nil)
		} else {
			status.Store(&err)
		}
	}
	root := cfg.root
	if root == "" {
		root, _ = st.Meta("root")
	}
	build := !cfg.exists
	if !build {
		if files, _, _, err := st.Stats(); err == nil && files == 0 {
			build = true // an earlier first build was interrupted
		}
	}
	if !build && !watch {
		return ready
	}
	if root == "" {
		setStatus(errors.New("the index does not record which repository it belongs to; rebuild it with `codewiki index PATH`"))
		return ready
	}
	if build {
		setStatus(fmt.Errorf("codewiki is building the index of %s for the first time; try again in a few seconds", root))
		ensureGitignore(filepath.Dir(cfg.db))
	}
	go func() {
		opts, err := indexer.StoredOptions(st)
		if err == nil && build {
			start := time.Now()
			logf("indexing " + root)
			if _, err = indexer.Index(root, st, opts); err == nil {
				logf(fmt.Sprintf("indexed %s in %s", root, time.Since(start).Round(time.Millisecond)))
			}
		}
		if err != nil {
			logf("indexing failed: " + err.Error())
			setStatus(fmt.Errorf("codewiki could not index %s: %w", root, err))
			return
		}
		setStatus(nil)
		if !watch {
			return
		}
		// Watch first catches up on edits made while no server was running
		// (and, after a first build, on edits made during it).
		err = indexer.Watch(ctx, root, st, opts, 2*time.Second, func(res indexer.Result, err error) {
			if err != nil {
				logf("re-index failed: " + err.Error())
			}
		})
		if err != nil {
			logf("watch stopped: " + err.Error())
		}
	}()
	return ready
}

// ensureGitignore makes an index directory named .codewiki ignore itself,
// so the index never shows up in the repository's git status.
func ensureGitignore(dir string) {
	if filepath.Base(dir) != ".codewiki" {
		return
	}
	p := filepath.Join(dir, ".gitignore")
	if _, err := os.Stat(p); err == nil {
		return
	}
	_ = os.MkdirAll(dir, 0o755)
	_ = os.WriteFile(p, []byte("# Created by codewiki: the index is a local cache.\n*\n"), 0o644)
}

func runWatch(args []string) error {
	fs := flag.NewFlagSet("watch", flag.ExitOnError)
	db := fs.String("db", defaultDB, "index file")
	interval := fs.Duration("interval", 2*time.Second, "how often to check for changes")
	fs.Parse(interspersed(fs, args)) //nolint:errcheck
	st, err := openExisting(*db)
	if err != nil {
		return err
	}
	defer st.Close()
	root := fs.Arg(0)
	if root == "" {
		if root, err = st.Meta("root"); err != nil || root == "" {
			return errors.New("no PATH given and none recorded in the index")
		}
	}
	opts, err := indexer.StoredOptions(st)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	fmt.Fprintf(os.Stderr, "watching %s (Ctrl-C to stop)\n", root)
	return indexer.Watch(ctx, root, st, opts, *interval, func(res indexer.Result, err error) {
		if err != nil {
			fmt.Fprintln(os.Stderr, "re-index failed:", err)
			return
		}
		fmt.Fprintf(os.Stderr, "%s re-indexed %d file(s), %d removed; %d call edges\n",
			time.Now().Format("15:04:05"), res.Files, res.Removed, res.Edges)
	})
}

func runAPIs(args []string) error {
	fs := flag.NewFlagSet("apis", flag.ExitOnError)
	db := fs.String("db", defaultDB, "index file")
	method := fs.String("method", "", "only this HTTP method")
	asJSON := fs.Bool("json", false, "print JSON")
	fs.Parse(interspersed(fs, args)) //nolint:errcheck
	st, err := openExisting(*db)
	if err != nil {
		return err
	}
	defer st.Close()
	eps, err := st.ListEndpoints(store.EndpointFilter{Path: fs.Arg(0), Method: *method, Limit: 1000})
	if err != nil {
		return err
	}
	if *asJSON {
		return printJSON(eps)
	}
	for _, e := range eps {
		loc := e.HandlerLocation
		if loc == "" {
			loc = fmt.Sprintf("%s:%d", e.File, e.Line)
		}
		fmt.Printf("%-7s %-40s %s  %s  [%s]\n", e.Method, e.Path, e.Handler, loc, e.Framework)
	}
	if len(eps) == 0 {
		fmt.Println("no HTTP endpoints found")
	}
	return nil
}

func runOverview(args []string) error {
	fs := flag.NewFlagSet("overview", flag.ExitOnError)
	db := fs.String("db", defaultDB, "index file")
	asJSON := fs.Bool("json", false, "print JSON")
	fs.Parse(interspersed(fs, args)) //nolint:errcheck
	st, err := openExisting(*db)
	if err != nil {
		return err
	}
	defer st.Close()
	o, err := st.BuildOverview(15)
	if err != nil {
		return err
	}
	if *asJSON {
		return printJSON(o)
	}
	fmt.Print(server.FormatOverview(o))
	return nil
}

// interspersed lets flags appear after positional arguments, so both
// `codewiki query --limit 3 rate limiter` and `codewiki query rate limiter --limit 3` work.
func interspersed(fs *flag.FlagSet, args []string) []string {
	var flags, rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			rest = append(rest, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			rest = append(rest, a)
			continue
		}
		flags = append(flags, a)
		name := strings.TrimLeft(a, "-")
		if strings.Contains(name, "=") {
			continue
		}
		if f := fs.Lookup(name); f != nil && i+1 < len(args) {
			if bf, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && bf.IsBoolFlag() {
				continue
			}
			i++
			flags = append(flags, args[i])
		}
	}
	return append(flags, rest...)
}

// splitList splits a comma-separated flag value, dropping empty entries.
func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i] + " ..."
	}
	return s
}
