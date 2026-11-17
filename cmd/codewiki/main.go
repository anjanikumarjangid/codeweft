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
  codewiki serve  [--db FILE]

By default the index is stored at PATH/.codewiki/index.db, and other commands
look for .codewiki/index.db in the current directory.
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

	opts := indexer.Options{IncludeTests: *tests}
	if *exclude != "" {
		opts.Exclude = strings.Split(*exclude, ",")
	}
	start := time.Now()
	res, err := indexer.Index(root, st, opts)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "indexed %d files, %d symbols (%d skipped) in %s\nindex: %s\n",
		res.Files, res.Symbols, res.Skipped, time.Since(start).Round(time.Millisecond), *db)
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

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	db := fs.String("db", defaultDB, "index file")
	fs.Parse(interspersed(fs, args)) //nolint:errcheck
	st, err := openExisting(*db)
	if err != nil {
		return err
	}
	defer st.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	// stdout carries the MCP protocol; anything else must go to stderr.
	return server.New(st).Run(ctx, &mcp.StdioTransport{})
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
