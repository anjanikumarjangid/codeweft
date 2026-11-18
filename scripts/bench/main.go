// Command bench measures codewiki on a pinned corpus of repositories:
// indexing time, memory and index size, incremental re-indexing, the
// latency and response size of every MCP tool, and server startup.
//
//	go run -tags sqlite_fts5 ./scripts/bench [-corpus scripts/bench/corpus.json]
//	    [-cache DIR] [-runs 3] [-only NAME] [-out results.json] [-md results.md]
//
// Repositories are cloned at the pinned ref into the cache directory once.
// Indexing runs the codewiki binary (built from this checkout) as a
// separate process, so its peak memory can be read. Tool calls go through
// the real MCP server over an in-memory transport, as a client's would,
// with queries generated from the index itself: the most-called
// functions, the types with the most implementations, the largest files,
// and words from symbol names.
package main

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"
	"unicode"

	_ "github.com/mattn/go-sqlite3"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/anjanikumarjangid/codewiki/internal/server"
	"github.com/anjanikumarjangid/codewiki/internal/store"
)

type repo struct {
	Name   string `json:"name"`
	URL    string `json:"url,omitempty"`
	Ref    string `json:"ref,omitempty"`
	GoRoot bool   `json:"goroot,omitempty"` // the Go standard library of the toolchain running the benchmark
}

// Result is everything measured for one repository.
type Result struct {
	Repo    string `json:"repo"`
	Ref     string `json:"ref"`
	Files   int    `json:"files"`
	Tests   int    `json:"test_files"`
	Lines   int    `json:"lines"`
	Symbols int    `json:"symbols"`
	Edges   int    `json:"call_edges"`
	// Full index: wall time of each run, and the process's peak memory
	// and CPU time in the median run.
	IndexRuns  []float64 `json:"index_seconds"`
	IndexCPU   float64   `json:"index_cpu_seconds"`
	PeakRSSMB  float64   `json:"peak_rss_mb"`
	IndexMB    float64   `json:"index_mb"`
	NoopSec    float64   `json:"reindex_noop_seconds"`
	OneFileSec float64   `json:"reindex_one_file_seconds"`
	StartupMS  float64   `json:"serve_startup_ms"` // process start to first tool answer, index present
	Tools      []Tool    `json:"tools"`
}

// Tool is the latency and response size of one MCP tool over its queries.
type Tool struct {
	Name     string  `json:"name"`
	Calls    int     `json:"calls"`
	P50MS    float64 `json:"p50_ms"`
	P95MS    float64 `json:"p95_ms"`
	MaxMS    float64 `json:"max_ms"`
	P50Bytes int     `json:"p50_bytes"`
	P95Bytes int     `json:"p95_bytes"`
	MaxBytes int     `json:"max_bytes"`
}

func main() {
	corpusPath := flag.String("corpus", "scripts/bench/corpus.json", "corpus file")
	cache := flag.String("cache", filepath.Join(os.TempDir(), "codewiki-bench"), "where repositories are cloned")
	runs := flag.Int("runs", 3, "full index runs per repository")
	only := flag.String("only", "", "measure only the repositories whose name contains this")
	out := flag.String("out", "", "write results as JSON to this file")
	md := flag.String("md", "", "write a Markdown report to this file")
	flag.Parse()

	var corpus []repo
	data, err := os.ReadFile(*corpusPath)
	check(err)
	check(json.Unmarshal(data, &corpus))
	check(os.MkdirAll(*cache, 0o755))
	bin := filepath.Join(*cache, "codewiki")
	run("go", "build", "-tags", "sqlite_fts5", "-o", bin, "./cmd/codewiki")

	var results []Result
	for _, r := range corpus {
		if *only != "" && !strings.Contains(strings.ToLower(r.Name), strings.ToLower(*only)) {
			continue
		}
		dir, ref := fetch(r, *cache)
		log.Printf("%s (%s)", r.Name, ref)
		res := measure(bin, dir, *runs)
		res.Repo, res.Ref = r.Name, ref
		results = append(results, res)
		log.Printf("  %d files, %d symbols; index %.2fs (median), %.0f MB peak; tools measured", res.Files, res.Symbols,
			median(res.IndexRuns), res.PeakRSSMB)
	}
	report := Report{Machine: machine(), Commit: strings.TrimSpace(output("git", "rev-parse", "--short", "HEAD")),
		Date: time.Now().UTC().Format("2006-01-02"), Runs: *runs, Results: results}
	if *out != "" {
		b, _ := json.MarshalIndent(report, "", "  ")
		check(os.WriteFile(*out, append(b, '\n'), 0o644))
	}
	text := markdown(report)
	if *md != "" {
		check(os.WriteFile(*md, []byte(text), 0o644))
	}
	fmt.Print(text)
}

// Report is the whole benchmark run.
type Report struct {
	Machine string   `json:"machine"`
	Commit  string   `json:"codewiki_commit"`
	Date    string   `json:"date"`
	Runs    int      `json:"runs"`
	Results []Result `json:"results"`
}

// fetch clones a repository at its pinned ref (once) and returns its
// directory and the ref measured.
func fetch(r repo, cache string) (string, string) {
	dir := filepath.Join(cache, "repos", strings.ReplaceAll(strings.ToLower(r.Name), " ", "-"))
	if r.GoRoot {
		// A copy: the incremental run edits a file, never the toolchain's.
		version := strings.TrimSpace(output("go", "env", "GOVERSION"))
		dir += "-" + version
		if _, err := os.Stat(dir); err != nil {
			goroot := strings.TrimSpace(output("go", "env", "GOROOT"))
			run("cp", "-r", filepath.Join(goroot, "src"), dir)
		}
		return dir, version
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		check(os.MkdirAll(dir, 0o755))
		run("git", "-C", dir, "init", "-q")
		run("git", "-C", dir, "fetch", "-q", "--depth", "1", r.URL, r.Ref)
		run("git", "-C", dir, "checkout", "-q", "FETCH_HEAD")
	}
	return dir, r.Ref
}

func measure(bin, dir string, runs int) Result {
	var res Result
	tmp, err := os.MkdirTemp("", "codewiki-bench-*")
	check(err)
	defer os.RemoveAll(tmp)
	db := filepath.Join(tmp, "index.db")

	// Full index, from scratch each time.
	type indexRun struct {
		wall, cpu float64
		rss       int64
	}
	var all []indexRun
	for i := 0; i < runs; i++ {
		for _, f := range []string{db, db + "-wal", db + "-shm"} {
			os.Remove(f)
		}
		wall, cpu, rss := timed(bin, "index", "--db", db, dir)
		all = append(all, indexRun{wall, cpu, rss})
		res.IndexRuns = append(res.IndexRuns, round(wall, 3))
	}
	sort.Slice(all, func(i, j int) bool { return all[i].wall < all[j].wall })
	mid := all[len(all)/2]
	res.IndexCPU, res.PeakRSSMB = round(mid.cpu, 2), round(float64(mid.rss)/1024, 0)
	res.IndexMB = round(fileMB(db)+fileMB(db+"-wal"), 1)

	conn, err := sql.Open("sqlite3", "file:"+db+"?mode=ro")
	check(err)
	defer conn.Close()
	one := func(q string) int {
		var n int
		check(conn.QueryRow(q).Scan(&n))
		return n
	}
	res.Files = one(`SELECT COUNT(*) FROM files WHERE test = 0`)
	res.Tests = one(`SELECT COUNT(*) FROM files WHERE test = 1`)
	res.Symbols = one(`SELECT COUNT(*) FROM symbols`)
	res.Edges = one(`SELECT COUNT(*) FROM edges WHERE to_id IS NOT NULL`) // between the repository's own symbols
	res.Lines = countLines(conn, dir)

	// Incremental: nothing changed, then one file edited.
	res.NoopSec, _, _ = timed(bin, "reindex", "--db", db, dir)
	res.NoopSec = round(res.NoopSec, 3)
	edited := largestFile(conn, dir)
	orig, err := os.ReadFile(edited)
	check(err)
	check(os.WriteFile(edited, append(append([]byte{}, orig...), []byte(commentFor(edited))...), 0o644))
	res.OneFileSec, _, _ = timed(bin, "reindex", "--db", db, dir)
	res.OneFileSec = round(res.OneFileSec, 3)
	check(os.WriteFile(edited, orig, 0o644))
	timed(bin, "reindex", "--db", db, dir) // back to the checkout as it is

	res.Tools = tools(db, conn)
	res.StartupMS = startup(bin, db)
	return res
}

// timed runs the codewiki binary and returns its wall time, CPU time (user
// and system) and peak resident memory in KiB.
func timed(bin string, args ...string) (wall, cpu float64, rssKB int64) {
	cmd := exec.Command(bin, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	start := time.Now()
	if err := cmd.Run(); err != nil {
		log.Fatalf("%s %v: %v\n%s", bin, args, err, stderr.String())
	}
	wall = time.Since(start).Seconds()
	if ru, ok := cmd.ProcessState.SysUsage().(*syscall.Rusage); ok {
		cpu = time.Duration(ru.Utime.Nano() + ru.Stime.Nano()).Seconds()
		rssKB = ru.Maxrss
	}
	return wall, cpu, rssKB
}

// largestFile is the non-test, non-generated source file with the most
// symbols: the edit an incremental run is measured with.
func largestFile(conn *sql.DB, dir string) string {
	var p string
	check(conn.QueryRow(`SELECT f.path FROM files f JOIN symbols s ON s.file_id = f.id
		WHERE f.test = 0 AND f.generated = 0 AND f.lang != 'proto'
		GROUP BY f.id ORDER BY COUNT(*) DESC, f.path LIMIT 1`).Scan(&p))
	return filepath.Join(dir, p)
}

func commentFor(path string) string {
	if strings.HasSuffix(path, ".py") {
		return "\n# codewiki benchmark edit\n"
	}
	return "\n// codewiki benchmark edit\n"
}

func countLines(conn *sql.DB, dir string) int {
	rows, err := conn.Query(`SELECT path FROM files WHERE test = 0`)
	check(err)
	defer rows.Close()
	total := 0
	for rows.Next() {
		var p string
		check(rows.Scan(&p))
		f, err := os.Open(filepath.Join(dir, p))
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1<<20), 1<<24)
		for sc.Scan() {
			total++
		}
		f.Close()
	}
	return total
}

// call is one tool call to measure.
type call struct {
	tool string
	args map[string]any
}

// queries generates each tool's queries from the index.
func queries(conn *sql.DB) []call {
	list := func(q string, n int) []string {
		rows, err := conn.Query(q, n)
		check(err)
		defer rows.Close()
		var out []string
		for rows.Next() {
			var s string
			check(rows.Scan(&s))
			out = append(out, s)
		}
		return out
	}
	hot := list(`SELECT s.qualified FROM edges e JOIN symbols s ON s.id = e.to_id
		GROUP BY s.qualified ORDER BY COUNT(DISTINCT e.from_id) DESC, s.qualified LIMIT ?`, 50)
	types := list(`SELECT s.qualified FROM type_edges e JOIN symbols s ON s.id = e.super_id
		GROUP BY s.qualified ORDER BY COUNT(*) DESC, s.qualified LIMIT ?`, 30)
	files := list(`SELECT f.path FROM files f JOIN symbols s ON s.file_id = f.id WHERE f.test = 0
		GROUP BY f.id ORDER BY COUNT(*) DESC, f.path LIMIT ?`, 30)
	names := list(`SELECT name FROM symbols WHERE exported = 1 AND kind IN ('func', 'method', 'class', 'struct', 'interface')
		GROUP BY name ORDER BY COUNT(*) DESC, name LIMIT ?`, 400)
	var calls []call
	// Search: one word, and two words, taken from symbol names.
	var words []string
	for _, n := range names {
		words = append(words, splitWords(n)...)
	}
	words = dedupe(words)
	for i := 0; i+1 < len(words) && len(calls) < 50; i += 7 {
		calls = append(calls, call{"search_code", map[string]any{"query": words[i]}})
		calls = append(calls, call{"search_code", map[string]any{"query": words[i] + " " + words[i+1]}})
	}
	for _, q := range hot {
		calls = append(calls, call{"get_symbol", map[string]any{"name": q}})
		calls = append(calls, call{"find_tests", map[string]any{"symbol": q}})
	}
	for _, q := range hot[:min(len(hot), 25)] {
		calls = append(calls, call{"get_dependency_graph", map[string]any{"symbol": q, "direction": "callers"}})
		calls = append(calls, call{"get_dependency_graph", map[string]any{"symbol": q, "direction": "callees"}})
	}
	for _, q := range types {
		calls = append(calls, call{"get_implementations", map[string]any{"symbol": q}})
	}
	for _, f := range files {
		calls = append(calls, call{"get_file_summary", map[string]any{"path": f}})
	}
	for i := 0; i < 5; i++ {
		calls = append(calls, call{"get_architecture_overview", map[string]any{}})
		calls = append(calls, call{"list_apis", map[string]any{}})
		calls = append(calls, call{"get_service_links", map[string]any{}})
	}
	return calls
}

// splitWords splits a camelCase or snake_case name into lower-case words
// of three letters or more.
func splitWords(name string) []string {
	var out []string
	var cur []rune
	flush := func() {
		if len(cur) >= 3 {
			out = append(out, strings.ToLower(string(cur)))
		}
		cur = nil
	}
	rs := []rune(name)
	for i, r := range rs {
		switch {
		case r == '_' || !unicode.IsLetter(r):
			flush()
		case unicode.IsUpper(r) && i > 0 && (unicode.IsLower(rs[i-1]) || (i+1 < len(rs) && unicode.IsLower(rs[i+1]))):
			flush()
			cur = append(cur, r)
		default:
			cur = append(cur, r)
		}
	}
	flush()
	return out
}

func dedupe(ss []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range ss {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// tools measures every tool call through the MCP server.
func tools(db string, conn *sql.DB) []Tool {
	st, err := store.Open(db)
	check(err)
	defer st.Close()
	ctx := context.Background()
	st1, ct := mcp.NewInMemoryTransports()
	ss, err := server.New(st, server.Options{}).Connect(ctx, st1, nil)
	check(err)
	defer ss.Close()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "bench", Version: "v0"}, nil).Connect(ctx, ct, nil)
	check(err)
	defer cs.Close()

	type sample struct {
		ms    float64
		bytes int
	}
	byTool := map[string][]sample{}
	var order []string
	calls := queries(conn)
	// Warm the SQLite page cache once, as a running server would be.
	for _, c := range calls {
		_, _ = cs.CallTool(ctx, &mcp.CallToolParams{Name: c.tool, Arguments: c.args})
	}
	for _, c := range calls {
		start := time.Now()
		r, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: c.tool, Arguments: c.args})
		ms := float64(time.Since(start).Microseconds()) / 1000
		check(err)
		n := 0
		for _, ct := range r.Content {
			if t, ok := ct.(*mcp.TextContent); ok {
				n += len(t.Text)
			}
		}
		if _, seen := byTool[c.tool]; !seen {
			order = append(order, c.tool)
		}
		byTool[c.tool] = append(byTool[c.tool], sample{ms, n})
	}
	var out []Tool
	for _, name := range order {
		ss := byTool[name]
		ms := make([]float64, len(ss))
		bs := make([]float64, len(ss))
		for i, s := range ss {
			ms[i], bs[i] = s.ms, float64(s.bytes)
		}
		out = append(out, Tool{Name: name, Calls: len(ss),
			P50MS: round(pct(ms, 50), 2), P95MS: round(pct(ms, 95), 2), MaxMS: round(pct(ms, 100), 2),
			P50Bytes: int(pct(bs, 50)), P95Bytes: int(pct(bs, 95)), MaxBytes: int(pct(bs, 100))})
	}
	return out
}

// startup measures `codewiki serve` on an existing index: from starting the
// process to the answer of its first tool call, the median of 5 starts.
func startup(bin, db string) float64 {
	var times []float64
	for i := 0; i < 5; i++ {
		cmd := exec.Command(bin, "serve", "--db", db, "--watch=false")
		in, err := cmd.StdinPipe()
		check(err)
		outp, err := cmd.StdoutPipe()
		check(err)
		start := time.Now()
		check(cmd.Start())
		rd := bufio.NewReader(outp)
		send := func(m string) { _, err := io.WriteString(in, m+"\n"); check(err) }
		send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"bench","version":"0"}}}`)
		_, err = rd.ReadString('\n')
		check(err)
		send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
		send(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"get_architecture_overview","arguments":{}}}`)
		_, err = rd.ReadString('\n')
		check(err)
		times = append(times, float64(time.Since(start).Microseconds())/1000)
		in.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}
	return round(median(times), 1)
}

func machine() string {
	cpu := "unknown CPU"
	if data, err := os.ReadFile("/proc/cpuinfo"); err == nil {
		for _, l := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(l, "model name") {
				cpu = strings.TrimSpace(strings.SplitN(l, ":", 2)[1])
				break
			}
		}
	}
	mem := ""
	if data, err := os.ReadFile("/proc/meminfo"); err == nil {
		var kb float64
		fmt.Sscanf(strings.TrimPrefix(strings.SplitN(string(data), "\n", 2)[0], "MemTotal:"), "%f", &kb)
		mem = fmt.Sprintf(", %.0f GB RAM", kb/1024/1024)
	}
	return fmt.Sprintf("%s/%s, %d vCPUs (%s)%s, %s", runtime.GOOS, runtime.GOARCH, runtime.NumCPU(), cpu, mem, runtime.Version())
}

func markdown(r Report) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Machine: %s. codewiki %s, %s. Full index: median of %d runs (min-max).\n\n", r.Machine, r.Commit, r.Date, r.Runs)
	b.WriteString("| Repository | Files | Lines | Symbols | Call edges | Full index | CPU | Peak memory | Index size | Re-index, no change | Re-index, one file | Server start |\n")
	b.WriteString("|---|---|---|---|---|---|---|---|---|---|---|---|\n")
	for _, x := range r.Results {
		lo, hi := minmax(x.IndexRuns)
		fmt.Fprintf(&b, "| %s `%s` | %s | %s | %s | %s | %.2f s (%.2f-%.2f) | %.1f s | %.0f MB | %.0f MB | %.2f s | %.2f s | %.0f ms |\n",
			x.Repo, short(x.Ref), comma(x.Files), comma(x.Lines), comma(x.Symbols), comma(x.Edges), median(x.IndexRuns), lo, hi,
			x.IndexCPU, x.PeakRSSMB, x.IndexMB, x.NoopSec, x.OneFileSec, x.StartupMS)
	}
	if len(r.Results) == 0 {
		return b.String()
	}
	header := "| Tool |"
	sep := "|---|"
	for _, x := range r.Results {
		header += " " + x.Repo + " |"
		sep += "---|"
	}
	var names []string
	for _, t := range r.Results[0].Tools {
		names = append(names, t.Name)
	}
	table := func(title string, cell func(Tool) string) {
		fmt.Fprintf(&b, "\n%s\n\n%s\n%s\n", title, header, sep)
		for _, n := range names {
			fmt.Fprintf(&b, "| `%s` |", n)
			for _, x := range r.Results {
				for _, t := range x.Tools {
					if t.Name == n {
						fmt.Fprintf(&b, " %s |", cell(t))
					}
				}
			}
			b.WriteString("\n")
		}
	}
	table("Tool latency through the MCP protocol, p50 / p95 in ms:", func(t Tool) string {
		return num(t.P50MS) + " / " + num(t.P95MS)
	})
	table("Response size, p50 / largest in KB (about 250 tokens per KB):", func(t Tool) string {
		return fmt.Sprintf("%.1f / %.1f", float64(t.P50Bytes)/1024, float64(t.MaxBytes)/1024)
	})
	return b.String()
}

func num(f float64) string {
	if f < 10 {
		return fmt.Sprintf("%.1f", f)
	}
	return fmt.Sprintf("%.0f", f)
}

func comma(n int) string {
	s := fmt.Sprint(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

func pct(v []float64, p float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	i := int(math.Ceil(p/100*float64(len(s)))) - 1
	return s[max(0, min(i, len(s)-1))]
}

func median(v []float64) float64 {
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	if len(s) == 0 {
		return 0
	}
	return s[len(s)/2]
}

func minmax(v []float64) (float64, float64) {
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	return s[0], s[len(s)-1]
}

func round(f float64, d int) float64 {
	p := math.Pow(10, float64(d))
	return math.Round(f*p) / p
}

func fileMB(p string) float64 {
	fi, err := os.Stat(p)
	if err != nil {
		return 0
	}
	return float64(fi.Size()) / (1 << 20)
}

func run(name string, args ...string) {
	cmd := exec.Command(name, args...)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Run(); err != nil {
		log.Fatalf("%s %v: %v", name, args, err)
	}
}

func output(name string, args ...string) string {
	b, err := exec.Command(name, args...).Output()
	check(err)
	return string(b)
}

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

// short abbreviates a commit hash; tags and versions are kept.
func short(ref string) string {
	if len(ref) == 40 {
		return ref[:7]
	}
	return ref
}
