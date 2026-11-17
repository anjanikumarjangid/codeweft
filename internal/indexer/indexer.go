// Package indexer walks a repository, parses source files into symbols,
// imports, fields and call sites, and resolves call sites into a call graph.
//
// Go is parsed with the standard library (go/ast), which is exact for Go;
// Java and Python are parsed with tree-sitter. Every language produces the
// same FileResult, so storage, search and the call graph are
// language-independent.
package indexer

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/anjanikumarjangid/codewiki/internal/store"
)

// FileResult is everything extracted from one source file.
type FileResult struct {
	File       store.File
	Symbols    []store.Symbol
	Imports    []store.Import
	Fields     []store.Field
	Vars       []store.Var
	Supertypes []store.Supertype
	Calls      []store.RawCall
	Endpoints  []store.Endpoint
}

// language describes how to recognise and parse one language.
type language struct {
	name      string
	exts      []string
	isTest    func(rel string) bool
	generated func(src []byte) bool
	parse     func(rel string, src []byte) (*FileResult, error)
}

var languages = []language{
	{
		name:      "go",
		exts:      []string{".go"},
		isTest:    func(rel string) bool { return strings.HasSuffix(rel, "_test.go") },
		generated: isGeneratedGo,
		parse:     ParseGo,
	},
	{
		name:      "java",
		exts:      []string{".java"},
		isTest:    isJavaTest,
		generated: isGeneratedHeader,
		parse:     ParseJava,
	},
	{
		name:      "python",
		exts:      []string{".py"},
		isTest:    isPythonTest,
		generated: isGeneratedHeader,
		parse:     ParsePython,
	},
}

func languageFor(path string) *language {
	ext := filepath.Ext(path)
	for i := range languages {
		for _, e := range languages[i].exts {
			if e == ext {
				return &languages[i]
			}
		}
	}
	return nil
}

// Options controls what gets indexed.
//
// Every run records its options in the index, so later incremental runs
// (reindex, watch, serve --watch) cover the same set of files. Without that,
// a watcher started with default options would drop test files or
// re-add excluded directories on the first save.
type Options struct {
	IncludeTests bool     `json:"include_tests,omitempty"`  // index test files
	Exclude      []string `json:"exclude,omitempty"`        // extra directory names to skip
	MaxFileBytes int64    `json:"max_file_bytes,omitempty"` // skip larger files (likely generated); 0 means 1 MiB
	// SkipGenerated leaves out files marked as generated ("Code generated
	// ... DO NOT EDIT."). By default they are indexed and ranked last.
	SkipGenerated bool `json:"skip_generated,omitempty"`
}

const optionsKey = "options"

// StoredOptions returns the options of the last run recorded in the index,
// or the zero Options for an index that recorded none.
func StoredOptions(st *store.Store) (Options, error) {
	var opts Options
	v, err := st.Meta(optionsKey)
	if err != nil || v == "" {
		return opts, err
	}
	if err := json.Unmarshal([]byte(v), &opts); err != nil {
		return Options{}, fmt.Errorf("stored index options: %w", err)
	}
	return opts, nil
}

// Result summarises an indexing run.
type Result struct {
	Files     int // files parsed in this run
	TestFiles int // test files parsed in this run, indexed only for find-tests
	Unchanged int // files skipped because their content hash matched
	Removed   int // files deleted from the index
	Symbols   int // symbols written in this run
	Edges     int // resolved call edges in the whole index
	Generated int // generated files among Files
	Skipped   int // oversized or unparsable files, and generated ones with SkipGenerated
}

var defaultSkipDirs = map[string]bool{
	".git": true, ".codewiki": true, "vendor": true, "node_modules": true,
	"testdata": true, "third_party": true, "__pycache__": true, "venv": true,
}

// outputDirs are names build tools write their output to (Gradle, Maven,
// Python packaging, JavaScript bundlers). They are skipped unless the
// directory is itself a Go package: Go's own go/build is one.
var outputDirs = map[string]bool{"build": true, "dist": true, "target": true}

// skipDir reports whether the walk should not enter directory p, named
// name. Hidden directories, the default skip list and the directories
// excluded by the user are skipped; build output directories are skipped
// unless they directly hold Go files.
func skipDir(p, name string, exclude map[string]bool) bool {
	switch {
	case strings.HasPrefix(name, "."), defaultSkipDirs[name], exclude[name]:
		return true
	case outputDirs[name]:
		return !hasGoFiles(p)
	}
	return false
}

func hasGoFiles(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".go") {
			return true
		}
	}
	return false
}

// excludeSet turns the user's --exclude list into a set.
func excludeSet(names []string) map[string]bool {
	set := map[string]bool{}
	for _, n := range names {
		if n = strings.TrimSpace(n); n != "" {
			set[n] = true
		}
	}
	return set
}

// Index fully re-indexes the repository at root.
func Index(root string, st *store.Store, opts Options) (Result, error) {
	return run(root, st, opts, false)
}

// Reindex updates an existing index: only files whose content hash changed
// are re-parsed, deleted files are removed, and the call graph is rebuilt
// from the stored call sites, so edges into changed files stay correct.
func Reindex(root string, st *store.Store, opts Options) (Result, error) {
	return run(root, st, opts, true)
}

func run(root string, st *store.Store, opts Options, incremental bool) (Result, error) {
	var res Result
	root, err := filepath.Abs(root)
	if err != nil {
		return res, err
	}
	saved, err := json.Marshal(opts) // as given, before defaults are filled in
	if err != nil {
		return res, err
	}
	if opts.MaxFileBytes == 0 {
		opts.MaxFileBytes = 1 << 20
	}
	exclude := excludeSet(opts.Exclude)

	tx, err := st.Begin()
	if err != nil {
		return res, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	known := map[string]string{}
	if incremental {
		if known, err = tx.FileHashes(); err != nil {
			return res, err
		}
	} else if err := tx.Reset(); err != nil {
		return res, err
	}
	// 1. Walk the tree and collect candidate files.
	type job struct {
		abs, rel string
		lang     *language
		testOnly bool // a test file indexed only for find-tests, not as code
	}
	var jobs []job
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			if p != root && skipDir(p, d.Name(), exclude) {
				return filepath.SkipDir
			}
			return nil
		}
		lang := languageFor(p)
		if lang == nil {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Size() > opts.MaxFileBytes {
			res.Skipped++
			return nil
		}
		jobs = append(jobs, job{abs: p, rel: rel, lang: lang, testOnly: !opts.IncludeTests && lang.isTest(rel)})
		return nil
	})
	if err != nil {
		return res, err
	}

	// 2. Read, hash and parse in parallel. Parsing is CPU-bound and
	// independent per file; results keep walk order so ids are stable.
	type outcome struct {
		fr        *FileResult
		hash      string
		unchanged bool
		skipped   bool
		err       error
	}
	results := make([]outcome, len(jobs))
	work := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < runtime.GOMAXPROCS(0); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range work {
				j := jobs[i]
				src, err := os.ReadFile(j.abs)
				if err != nil {
					results[i].err = err
					continue
				}
				generated := j.lang.generated(src)
				if generated && opts.SkipGenerated {
					results[i].skipped = true
					continue
				}
				sum := sha256.Sum256(src)
				hash := hex.EncodeToString(sum[:])
				if j.testOnly {
					hash = "test:" + hash // see store.Tx.FileHashes
				}
				results[i].hash = hash
				if known[j.rel] == hash {
					results[i].unchanged = true
					continue
				}
				fr, err := j.lang.parse(j.rel, src)
				if err != nil {
					// A file that does not parse should not stop the whole index.
					results[i].skipped = true
					continue
				}
				fr.File.Hash = strings.TrimPrefix(hash, "test:")
				fr.File.Test = j.testOnly
				fr.File.Generated = generated
				results[i].fr = fr
			}
		}()
	}
	for i := range jobs {
		work <- i
	}
	close(work)
	wg.Wait()

	// 3. Write sequentially: SQLite has a single writer anyway.
	seen := map[string]bool{}
	for i, r := range results {
		rel := jobs[i].rel
		switch {
		case r.err != nil:
			return res, r.err
		case r.skipped:
			res.Skipped++
			continue
		}
		seen[rel] = true
		if r.unchanged {
			res.Unchanged++
			continue
		}
		if _, ok := known[rel]; ok {
			if err := tx.DeleteFile(rel); err != nil {
				return res, err
			}
		}
		isTest := jobs[i].lang.isTest(rel)
		if err := write(tx, r.fr, isTest); err != nil {
			return res, fmt.Errorf("%s: %w", rel, err)
		}
		if r.fr.File.Test {
			res.TestFiles++
			continue
		}
		res.Files++
		res.Symbols += len(r.fr.Symbols)
		if r.fr.File.Generated {
			res.Generated++
		}
	}

	for p := range known {
		if !seen[p] {
			if err := tx.DeleteFile(p); err != nil {
				return res, err
			}
			res.Removed++
		}
	}

	if res.Edges, err = resolve(tx, goModule(root)); err != nil {
		return res, fmt.Errorf("resolve calls: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return res, err
	}
	if err := st.SetMeta(optionsKey, string(saved)); err != nil {
		return res, err
	}
	return res, st.SetMeta("root", root)
}

// write stores one parsed file. A test file also records its functions as
// test functions; a test-only file (the default for test files) stores
// nothing else but its call sites, so test code never shows up in search,
// the call graph or the type hierarchy, and test doubles never make a
// call to the repository's own code ambiguous.
func write(tx *store.Tx, fr *FileResult, isTest bool) error {
	id, err := tx.AddFile(fr.File)
	if err != nil {
		return err
	}
	if isTest {
		for _, s := range fr.Symbols {
			if s.Kind == "func" || s.Kind == "method" {
				tf := store.TestFunc{Qualified: s.Qualified, Line: s.StartLine, Entry: isTestEntry(fr.File.Lang, s)}
				if err := tx.AddTestFunc(id, tf); err != nil {
					return err
				}
			}
		}
	}
	if fr.File.Test {
		for _, imp := range fr.Imports { // the context of its calls
			if err := tx.AddImport(id, imp); err != nil {
				return err
			}
		}
		for _, c := range fr.Calls {
			if err := tx.AddRawCall(id, c); err != nil {
				return err
			}
		}
		return nil
	}
	for _, s := range fr.Symbols {
		if err := tx.AddSymbol(id, s); err != nil {
			return err
		}
	}
	for _, imp := range fr.Imports {
		if err := tx.AddImport(id, imp); err != nil {
			return err
		}
	}
	for _, f := range fr.Fields {
		if err := tx.AddField(id, f); err != nil {
			return err
		}
	}
	for _, v := range fr.Vars {
		if err := tx.AddVar(id, v); err != nil {
			return err
		}
	}
	for _, st := range fr.Supertypes {
		if err := tx.AddSupertype(id, st); err != nil {
			return err
		}
	}
	for _, c := range fr.Calls {
		if err := tx.AddRawCall(id, c); err != nil {
			return err
		}
	}
	for _, e := range fr.Endpoints {
		if err := tx.AddEndpoint(id, e); err != nil {
			return err
		}
	}
	return nil
}

// goModule reads the module path from root/go.mod, or "" if there is none.
func goModule(root string) string {
	data, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return ""
	}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if rest, ok := strings.CutPrefix(line, "module "); ok {
			return strings.Trim(strings.TrimSpace(rest), `"`)
		}
	}
	return ""
}
