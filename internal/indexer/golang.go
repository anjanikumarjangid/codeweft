package indexer

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/anjanikumarjangid/codewiki/internal/store"
)

// generatedHeader is the convention from https://go.dev/s/generatedcode.
var generatedHeader = regexp.MustCompile(`(?m)^// Code generated .* DO NOT EDIT\.$`)

// isGeneratedGo reports whether a Go file carries the standard
// generated-code comment before its package clause.
func isGeneratedGo(src []byte) bool {
	if i := bytes.Index(src, []byte("\npackage ")); i >= 0 {
		src = src[:i]
	}
	return generatedHeader.Match(src)
}

var goBuiltins = map[string]bool{
	"append": true, "cap": true, "clear": true, "close": true, "complex": true, "copy": true,
	"delete": true, "imag": true, "len": true, "make": true, "max": true, "min": true, "new": true,
	"panic": true, "print": true, "println": true, "real": true, "recover": true,
}

// goQual returns the qualifier for a Go package: its directory, plus the
// package name when that differs from the directory name. Two "util"
// packages in different folders therefore never collide.
func goQual(dir, pkg string) string {
	if dir == "." {
		return pkg
	}
	if path.Base(dir) == pkg {
		return dir
	}
	return dir + "/" + pkg
}

// goImportName guesses the local name of an unaliased import:
// "github.com/x/yaml.v3" -> yaml, "github.com/x/pgx/v5" -> pgx, "go-redis" -> redis.
func goImportName(p string) string {
	parts := strings.Split(p, "/")
	name := parts[len(parts)-1]
	if len(parts) > 1 && len(name) > 1 && name[0] == 'v' && isDigits(name[1:]) {
		name = parts[len(parts)-2]
	}
	if i := strings.Index(name, ".v"); i > 0 && isDigits(name[i+2:]) {
		name = name[:i]
	}
	name = strings.TrimPrefix(name, "go-")
	return strings.ReplaceAll(name, "-", "_")
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// ParseGo extracts declarations, imports, struct fields and call sites from
// one Go file. relPath is the slash-separated path inside the repository.
func ParseGo(relPath string, src []byte) (*FileResult, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, relPath, src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	pkg := f.Name.Name
	dir := path.Dir(relPath)
	qual := goQual(dir, pkg)
	res := &FileResult{File: store.File{
		Path: relPath, Dir: dir, Lang: "go", Package: pkg, Qual: qual,
		Doc: strings.TrimSpace(f.Doc.Text()),
	}}

	aliases := map[string]string{} // local name -> import path
	for _, imp := range f.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		name := goImportName(p)
		if imp.Name != nil {
			name = imp.Name.Name
		}
		res.Imports = append(res.Imports, store.Import{Path: p, Alias: name})
		if name != "_" && name != "." {
			aliases[name] = p
		}
	}

	add := func(name, qualified, kind, sig, doc string, node ast.Node) {
		start, end := fset.Position(node.Pos()), fset.Position(node.End())
		res.Symbols = append(res.Symbols, store.Symbol{
			File: relPath, Lang: "go", Package: pkg, Name: name, Qualified: qualified, Kind: kind,
			Signature: sig, Doc: strings.TrimSpace(doc),
			StartLine: start.Line, EndLine: end.Line,
			Exported: ast.IsExported(name),
			Body:     string(src[start.Offset:end.Offset]),
		})
	}

	// typeRef turns a field type expression into a resolvable reference.
	typeRef := func(e ast.Expr) string {
		for {
			switch t := e.(type) {
			case *ast.StarExpr:
				e = t.X
			case *ast.IndexExpr:
				e = t.X
			case *ast.IndexListExpr:
				e = t.X
			case *ast.Ident:
				return qual + "." + t.Name
			case *ast.SelectorExpr:
				if x, ok := t.X.(*ast.Ident); ok {
					if p, ok := aliases[x.Name]; ok {
						return "@" + p + "." + t.Sel.Name
					}
				}
				return ""
			default:
				return ""
			}
		}
	}

	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			kind, qualified := "func", qual+"."+d.Name.Name
			recvVar, recvType := "", ""
			if d.Recv != nil && len(d.Recv.List) > 0 {
				kind = "method"
				recvType = qual + "." + receiverName(d.Recv.List[0].Type)
				qualified = recvType + "." + d.Name.Name
				if len(d.Recv.List[0].Names) > 0 {
					recvVar = d.Recv.List[0].Names[0].Name
				}
			}
			add(d.Name.Name, qualified, kind, funcSignature(fset, d), d.Doc.Text(), d)
			if d.Body != nil {
				vars := goVarTypes(d, typeRef)
				res.Calls = append(res.Calls, goCalls(fset, d.Body, qualified, qual, recvVar, recvType, aliases, vars)...)
			}

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
				owner := qual + "." + ts.Name.Name
				add(ts.Name.Name, owner, kind, sig, doc, node)

				switch t := ts.Type.(type) {
				case *ast.StructType:
					for _, fld := range t.Fields.List {
						ref := typeRef(fld.Type)
						if ref == "" {
							continue
						}
						names := fld.Names
						if len(names) == 0 { // embedded field: named after its type
							names = []*ast.Ident{{Name: ref[strings.LastIndex(ref, ".")+1:]}}
						}
						for _, n := range names {
							res.Fields = append(res.Fields, store.Field{Owner: owner, Name: n.Name, TypeRef: ref})
						}
					}
				case *ast.InterfaceType:
					for _, m := range t.Methods.List {
						ft, ok := m.Type.(*ast.FuncType)
						if !ok || len(m.Names) == 0 {
							continue // embedded interface
						}
						name := m.Names[0].Name
						sig := name + strings.TrimPrefix(printNode(fset, ft), "func")
						add(name, owner+"."+name, "interface method", sig, m.Doc.Text(), m)
					}
				}
			}
		}
	}
	res.Endpoints = goEndpoints(fset, f, qual, aliases)
	for i := range res.Endpoints {
		res.Endpoints[i].File = relPath
	}
	return res, nil
}

// goVarTypes records the declared type of local names where the syntax
// states it: parameters, `var x T`, and `x := T{...}` / `x := &T{...}`.
// Scopes are ignored; shadowing is rare enough not to matter here.
func goVarTypes(d *ast.FuncDecl, typeRef func(ast.Expr) string) map[string]string {
	vars := map[string]string{}
	addFields := func(fl *ast.FieldList) {
		if fl == nil {
			return
		}
		for _, f := range fl.List {
			if ref := typeRef(f.Type); ref != "" {
				for _, n := range f.Names {
					vars[n.Name] = ref
				}
			}
		}
	}
	addFields(d.Type.Params)
	litType := func(e ast.Expr) ast.Expr {
		if u, ok := e.(*ast.UnaryExpr); ok && u.Op == token.AND {
			e = u.X
		}
		if cl, ok := e.(*ast.CompositeLit); ok {
			return cl.Type
		}
		return nil
	}
	ast.Inspect(d.Body, func(n ast.Node) bool {
		switch t := n.(type) {
		case *ast.ValueSpec:
			if t.Type != nil {
				if ref := typeRef(t.Type); ref != "" {
					for _, name := range t.Names {
						vars[name.Name] = ref
					}
				}
			}
		case *ast.AssignStmt:
			if t.Tok != token.DEFINE || len(t.Lhs) != len(t.Rhs) {
				return true
			}
			for i, lhs := range t.Lhs {
				id, ok := lhs.(*ast.Ident)
				if !ok {
					continue
				}
				if te := litType(t.Rhs[i]); te != nil {
					if ref := typeRef(te); ref != "" {
						vars[id.Name] = ref
					}
				}
			}
		case *ast.FuncLit:
			addFields(t.Type.Params)
		}
		return true
	})
	return vars
}

// goCalls collects call sites inside a function body, including calls inside
// closures, which are attributed to the enclosing function.
func goCalls(fset *token.FileSet, body *ast.BlockStmt, caller, qual, recvVar, recvType string, aliases, vars map[string]string) []store.RawCall {
	var out []store.RawCall
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		line := fset.Position(call.Pos()).Line
		fun := call.Fun
		for { // unwrap generic instantiation and parentheses
			switch t := fun.(type) {
			case *ast.IndexExpr:
				fun = t.X
				continue
			case *ast.IndexListExpr:
				fun = t.X
				continue
			case *ast.ParenExpr:
				fun = t.X
				continue
			}
			break
		}
		rc := store.RawCall{Caller: caller, Line: line}
		switch t := fun.(type) {
		case *ast.Ident:
			if goBuiltins[t.Name] {
				return true
			}
			rc.Form, rc.A = store.CallQualified, qual+"."+t.Name
		case *ast.SelectorExpr:
			name := t.Sel.Name
			switch x := t.X.(type) {
			case *ast.Ident:
				switch {
				case recvVar != "" && x.Name == recvVar:
					rc.Form, rc.A, rc.B = store.CallQualified, recvType+"."+name, name
				case vars[x.Name] != "":
					rc.Form, rc.A, rc.B = store.CallTyped, vars[x.Name], name
				case aliases[x.Name] != "":
					rc.Form, rc.A, rc.B = store.CallImport, aliases[x.Name], name
				default:
					rc.Form, rc.B = store.CallMethod, name
				}
			case *ast.SelectorExpr:
				id, ok := x.X.(*ast.Ident)
				switch {
				case ok && recvVar != "" && id.Name == recvVar:
					rc.Form, rc.A, rc.B, rc.C = store.CallField, recvType, x.Sel.Name, name
				case ok && vars[id.Name] != "":
					rc.Form, rc.A, rc.B, rc.C = store.CallField, vars[id.Name], x.Sel.Name, name
				default:
					rc.Form, rc.B = store.CallMethod, name
				}
			default:
				rc.Form, rc.B = store.CallMethod, name
			}
		default:
			return true // calls of function values, e.g. handlers[i](x)
		}
		out = append(out, rc)
		return true
	})
	return out
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
	header = strings.TrimSpace(strings.TrimSuffix(header, "="))
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
