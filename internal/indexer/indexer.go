// Package indexer walks a repository and extracts declarations into the store.
//
// v0.1 supports Go using the standard library parser (go/ast), which is exact
// for Go and needs no external grammar. Other languages come later via
// tree-sitter behind the same Symbol model.
package indexer

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/anjanikumarjangid/codewiki/internal/store"
)

// Options controls what gets indexed.
type Options struct {
	IncludeTests bool     // index *_test.go files
	Exclude      []string // extra directory names to skip
	MaxFileBytes int64    // skip larger files (likely generated); 0 means 1 MiB
}

// Result summarises an indexing run.
type Result struct {
	Files   int
	Symbols int
	Skipped int
}

var defaultSkipDirs = map[string]bool{
	".git": true, ".codewiki": true, "vendor": true, "node_modules": true,
	"testdata": true, "third_party": true,
}

// Index fully re-indexes the repository at root into st.
func Index(root string, st *store.Store, opts Options) (Result, error) {
	var res Result
	root, err := filepath.Abs(root)
	if err != nil {
		return res, err
	}
	if opts.MaxFileBytes == 0 {
		opts.MaxFileBytes = 1 << 20
	}
	skip := map[string]bool{}
	for k := range defaultSkipDirs {
		skip[k] = true
	}
	for _, d := range opts.Exclude {
		skip[d] = true
	}

	tx, err := st.Begin()
	if err != nil {
		return res, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	if err := tx.Reset(); err != nil {
		return res, err
	}

	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			if p != root && (skip[d.Name()] || strings.HasPrefix(d.Name(), ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") {
			return nil
		}
		if !opts.IncludeTests && strings.HasSuffix(p, "_test.go") {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Size() > opts.MaxFileBytes {
			res.Skipped++
			return nil
		}
		src, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if isGenerated(src) {
			res.Skipped++
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)

		syms, err := ParseGo(rel, src)
		if err != nil {
			// A file that does not parse should not stop the whole index.
			res.Skipped++
			return nil
		}
		sum := sha256.Sum256(src)
		fileID, err := tx.AddFile(store.File{Path: rel, Lang: "go", Hash: hex.EncodeToString(sum[:])})
		if err != nil {
			return err
		}
		for _, s := range syms {
			if err := tx.AddSymbol(fileID, s); err != nil {
				return fmt.Errorf("%s: %w", rel, err)
			}
		}
		res.Files++
		res.Symbols += len(syms)
		return nil
	})
	if err != nil {
		return res, err
	}
	if err := tx.Commit(); err != nil {
		return res, err
	}
	return res, st.SetMeta("root", root)
}

// generatedHeader is the convention from https://go.dev/s/generatedcode.
var generatedHeader = regexp.MustCompile(`(?m)^// Code generated .* DO NOT EDIT\.$`)

// isGenerated reports whether a Go file carries the standard generated-code
// comment before its package clause.
func isGenerated(src []byte) bool {
	if i := bytes.Index(src, []byte("\npackage ")); i >= 0 {
		src = src[:i]
	}
	return generatedHeader.Match(src)
}

// ParseGo extracts functions, methods and type declarations from one Go file.
// relPath is the slash-separated path inside the repository.
func ParseGo(relPath string, src []byte) ([]store.Symbol, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, relPath, src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	pkg := f.Name.Name
	// Qualify by directory as well as package name, so two "util" packages in
	// different folders do not collide.
	pkgPath := path.Dir(relPath)
	qual := pkg
	if pkgPath != "." {
		qual = pkgPath + "/" + pkg
		if path.Base(pkgPath) == pkg {
			qual = pkgPath
		}
	}

	var out []store.Symbol
	add := func(name, qualified, kind, sig, doc string, node ast.Node) {
		start, end := fset.Position(node.Pos()), fset.Position(node.End())
		out = append(out, store.Symbol{
			File:      relPath,
			Package:   pkg,
			Name:      name,
			Qualified: qualified,
			Kind:      kind,
			Signature: sig,
			Doc:       strings.TrimSpace(doc),
			StartLine: start.Line,
			EndLine:   end.Line,
			Body:      string(src[start.Offset:end.Offset]),
		})
	}

	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			kind, qualified := "func", qual+"."+d.Name.Name
			if d.Recv != nil && len(d.Recv.List) > 0 {
				kind = "method"
				qualified = qual + "." + receiverName(d.Recv.List[0].Type) + "." + d.Name.Name
			}
			add(d.Name.Name, qualified, kind, funcSignature(fset, d), d.Doc.Text(), d)

		case *ast.GenDecl:
			if d.Tok != token.TYPE {
				continue
			}
			for _, spec := range d.Specs {
				ts := spec.(*ast.TypeSpec)
				doc := ts.Doc.Text()
				if doc == "" && len(d.Specs) == 1 {
					doc = d.Doc.Text()
				}
				kind, sig := typeKindAndSignature(fset, src, ts)
				var node ast.Node = ts
				if len(d.Specs) == 1 {
					node = d // include "type" keyword and doc position
				}
				add(ts.Name.Name, qual+"."+ts.Name.Name, kind, sig, doc, node)
			}
		}
	}
	return out, nil
}

// receiverName returns the bare type name of a method receiver,
// stripping pointers and type parameters: (*Cache[K, V]) -> Cache.
func receiverName(expr ast.Expr) string {
	for {
		switch t := expr.(type) {
		case *ast.StarExpr:
			expr = t.X
		case *ast.IndexExpr:
			expr = t.X
		case *ast.IndexListExpr:
			expr = t.X
		case *ast.Ident:
			return t.Name
		default:
			return "?"
		}
	}
}

// funcSignature prints a function declaration without its body or doc.
func funcSignature(fset *token.FileSet, d *ast.FuncDecl) string {
	header := &ast.FuncDecl{Recv: d.Recv, Name: d.Name, Type: d.Type}
	return printNode(fset, header)
}

// typeKindAndSignature classifies a type declaration. Structs are summarised
// by their header (fields are in the body); interfaces keep their full method
// set because that is the useful part; other types print their definition.
func typeKindAndSignature(fset *token.FileSet, src []byte, ts *ast.TypeSpec) (string, string) {
	// Header text straight from the source, e.g. "Cache[K comparable, V any]".
	header := strings.TrimSpace(string(src[fset.Position(ts.Pos()).Offset:fset.Position(ts.Type.Pos()).Offset]))
	header = strings.TrimSuffix(strings.TrimSpace(strings.TrimSuffix(header, "=")), " ")
	switch ts.Type.(type) {
	case *ast.StructType:
		return "struct", "type " + header + " struct"
	case *ast.InterfaceType:
		return "interface", "type " + header + " " + printNode(fset, ts.Type)
	}
	op := " "
	if ts.Assign.IsValid() {
		op = " = "
	}
	return "type", "type " + header + op + printNode(fset, ts.Type)
}

func printNode(fset *token.FileSet, node any) string {
	var buf bytes.Buffer
	cfg := printer.Config{Mode: printer.UseSpaces | printer.TabIndent, Tabwidth: 8}
	if err := cfg.Fprint(&buf, fset, node); err != nil {
		return ""
	}
	return buf.String()
}
