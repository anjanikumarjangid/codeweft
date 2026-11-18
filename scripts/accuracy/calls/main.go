// Command calls type-checks every package of the Go standard library and
// writes the ground truth for codeweft's call graph:
//
//	OUTDIR/calls.txt    every static call edge "caller callee"
//	OUTDIR/callers.txt  every function and method declaration it saw
//
// Names follow codeweft's qualification (package directory, then Func or
// Type.Method). Calls inside closures are attributed to the enclosing
// declaration, as codeweft does. Calls of function values, builtins and
// conversions have no static callee and are not edges. Only the files the
// current GOOS/GOARCH builds are type-checked.
//
// Usage: go run ./calls OUTDIR
package main

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

type pkgInfo struct {
	ImportPath string
	Dir        string
	GoFiles    []string
	CgoFiles   []string
}

// qual is codeweft's qualifier for a package: its path, plus the package
// name when that differs from the last path element.
func qual(p *types.Package) string {
	if path.Base(p.Path()) == p.Name() {
		return p.Path()
	}
	return p.Path() + "/" + p.Name()
}

// name returns codeweft's qualified name for a function or method, or ""
// for a method of an unnamed type.
func name(f *types.Func) string {
	f = f.Origin()
	if f.Pkg() == nil {
		return ""
	}
	recv := f.Type().(*types.Signature).Recv()
	if recv == nil {
		return qual(f.Pkg()) + "." + f.Name()
	}
	t := recv.Type()
	if p, ok := t.(*types.Pointer); ok {
		t = p.Elem()
	}
	if n, ok := t.(*types.Named); ok {
		return qual(n.Obj().Pkg()) + "." + n.Obj().Name() + "." + f.Name()
	}
	return ""
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: calls OUTDIR")
		os.Exit(2)
	}
	out, err := exec.Command("go", "list", "-json", "std").Output()
	if err != nil {
		panic(err)
	}
	fset := token.NewFileSet()
	imp := importer.ForCompiler(fset, "source", nil)
	edges, callers := map[string]bool{}, map[string]bool{}
	dec := json.NewDecoder(strings.NewReader(string(out)))
	pkgs := 0
	for dec.More() {
		var p pkgInfo
		if err := dec.Decode(&p); err != nil {
			panic(err)
		}
		if strings.HasPrefix(p.ImportPath, "vendor/") {
			continue // codeweft does not index vendor/
		}
		var files []*ast.File
		for _, f := range append(p.GoFiles, p.CgoFiles...) {
			af, err := parser.ParseFile(fset, filepath.Join(p.Dir, f), nil, parser.SkipObjectResolution)
			if err != nil {
				panic(err)
			}
			files = append(files, af)
		}
		info := &types.Info{Uses: map[*ast.Ident]types.Object{}, Defs: map[*ast.Ident]types.Object{}}
		conf := types.Config{Importer: imp, FakeImportC: true, Error: func(error) {}}
		if _, err := conf.Check(p.ImportPath, fset, files, info); err != nil {
			// cgo-only code cannot be fully checked; the rest is kept.
			fmt.Fprintln(os.Stderr, "type errors in", p.ImportPath, "(kept):", err)
		}
		pkgs++
		for _, af := range files {
			for _, d := range af.Decls {
				fd, ok := d.(*ast.FuncDecl)
				if !ok || fd.Body == nil {
					continue
				}
				fn, ok := info.Defs[fd.Name].(*types.Func)
				if !ok || name(fn) == "" {
					continue
				}
				caller := name(fn)
				callers[caller] = true
				ast.Inspect(fd.Body, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					fun := ast.Unparen(call.Fun)
					for { // generic instantiation: f[T](x)
						if ix, ok := fun.(*ast.IndexExpr); ok {
							fun = ix.X
						} else if ix, ok := fun.(*ast.IndexListExpr); ok {
							fun = ix.X
						} else {
							break
						}
					}
					var id *ast.Ident
					switch t := fun.(type) {
					case *ast.Ident:
						id = t
					case *ast.SelectorExpr:
						id = t.Sel
					}
					if id == nil {
						return true
					}
					if callee, ok := info.Uses[id].(*types.Func); ok && name(callee) != "" {
						edges[caller+" "+name(callee)] = true
					}
					return true
				})
			}
		}
	}
	write(filepath.Join(os.Args[1], "calls.txt"), edges)
	write(filepath.Join(os.Args[1], "callers.txt"), callers)
	fmt.Fprintln(os.Stderr, pkgs, "packages,", len(callers), "functions,", len(edges), "distinct call edges")
}

func write(file string, set map[string]bool) {
	lines := make([]string, 0, len(set))
	for l := range set {
		lines = append(lines, l)
	}
	sort.Strings(lines)
	if err := os.WriteFile(file, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		panic(err)
	}
}
