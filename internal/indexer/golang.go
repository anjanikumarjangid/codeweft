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

	"github.com/anjanikumarjangid/codeweft/internal/store"
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
// "github.com/x/yaml.v3" -> yaml, "github.com/x/pgx/v5" -> pgx, "go-redis" -> redis,
// "kafka-go" -> kafka.
func goImportName(p string) string {
	parts := strings.Split(p, "/")
	name := parts[len(parts)-1]
	if len(parts) > 1 && len(name) > 1 && name[0] == 'v' && isDigits(name[1:]) {
		name = parts[len(parts)-2]
	}
	if i := strings.Index(name, ".v"); i > 0 && isDigits(name[i+2:]) {
		name = name[:i]
	}
	name = strings.TrimSuffix(strings.TrimPrefix(name, "go-"), "-go")
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

	typeRef := func(e ast.Expr) string { return goTypeRef(e, qual, aliases) }

	consts := goStringConsts(f)
	clientFields := goGRPCClientFields(f)
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			kind, qualified := "func", qual+"."+d.Name.Name
			if d.Recv != nil && len(d.Recv.List) > 0 {
				kind = "method"
				qualified = qual + "." + receiverName(d.Recv.List[0].Type) + "." + d.Name.Name
			}
			add(d.Name.Name, qualified, kind, funcSignature(fset, d), d.Doc.Text(), d)
			if d.Body != nil {
				enc := &goEncoder{fset: fset, src: src, aliases: aliases,
					locals: goLocals(d), visiting: map[goLocalKey]bool{}, typeParams: goTypeParams(fset, d)}
				res.Calls = append(res.Calls, goCalls(fset, d.Body, qualified, qual, aliases, enc)...)
				res.Contracts = append(res.Contracts, goHTTPClientCalls(fset, d.Body, qualified, aliases, consts, enc)...)
				res.Contracts = append(res.Contracts, goTemporalCalls(fset, d.Body, qualified, qual, aliases, consts, enc)...)
				res.Contracts = append(res.Contracts, goGRPCCalls(fset, d.Body, qualified, qual, aliases, enc, clientFields)...)
				res.Contracts = append(res.Contracts, goKafkaCalls(fset, d.Body, qualified, qual, aliases, consts, enc)...)
				res.Contracts = append(res.Contracts, goMessagingCalls(fset, d.Body, qualified, aliases, consts, enc)...)
			}
			if c, ok := goWorkflowDef(fset, d, qualified, aliases); ok {
				res.Contracts = append(res.Contracts, c)
			}

		case *ast.GenDecl:
			if d.Tok == token.VAR {
				// Package-level variables: how their type can be found.
				enc := &goEncoder{fset: fset, src: src, aliases: aliases, locals: map[string][]goLocal{}, visiting: map[goLocalKey]bool{}}
				for _, spec := range d.Specs {
					vs := spec.(*ast.ValueSpec)
					for i, n := range vs.Names {
						var e string
						switch {
						case vs.Type != nil:
							e = enc.encodeJSON(&ast.CompositeLit{Type: vs.Type})
						case len(vs.Values) == len(vs.Names):
							e = enc.encodeJSON(vs.Values[i])
						case len(vs.Values) == 1 && i == 0:
							e = enc.encodeJSON(vs.Values[0])
						}
						if e != "" && n.Name != "_" {
							res.Vars = append(res.Vars, store.Var{Qualified: qual + "." + n.Name, Expr: e})
						}
					}
				}
				continue
			}
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
					if svc := goGRPCServer(t); svc != "" {
						res.Contracts = append(res.Contracts, store.Contract{Kind: store.ContractGRPC, Role: store.RoleProvides,
							Name: svc + "/", Symbol: store.StructMethods + owner, Line: fset.Position(ts.Pos()).Line, Exact: true})
					}
					for _, fld := range t.Fields.List {
						ref := typeRef(fld.Type)
						names := fld.Names
						if len(names) == 0 { // embedded field: named after its type
							if ref == "" {
								continue
							}
							names = []*ast.Ident{{Name: ref[strings.LastIndex(ref, ".")+1:]}}
							if ref := embeddedRef(fld.Type, typeRef); ref != "" {
								res.Supertypes = append(res.Supertypes, store.Supertype{Owner: owner, Kind: store.SuperEmbeds, TypeRef: ref})
							}
						}
						for _, n := range names {
							res.Fields = append(res.Fields, store.Field{Owner: owner, Name: n.Name, TypeRef: ref,
								TypeExpr: printNode(fset, fld.Type)})
						}
					}
				case *ast.InterfaceType:
					for _, m := range t.Methods.List {
						ft, ok := m.Type.(*ast.FuncType)
						if !ok || len(m.Names) == 0 {
							// An embedded interface: this interface extends it.
							// (Type-set elements such as ~int | string give no ref.)
							if ref := embeddedRef(m.Type, typeRef); ref != "" {
								res.Supertypes = append(res.Supertypes, store.Supertype{Owner: owner, Kind: store.SuperExtends, TypeRef: ref})
							}
							continue
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

// goTypeRef turns a type expression into a resolvable reference: "QUAL.T"
// for a type of this package, "@PATH.T" for one of an imported package.
func goTypeRef(e ast.Expr, qual string, aliases map[string]string) string {
	for {
		switch t := e.(type) {
		case *ast.StarExpr:
			e = t.X
		case *ast.ParenExpr:
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

// goPredeclared are the predeclared type names, which typeRef would
// otherwise mistake for types of the current package.
var goPredeclared = map[string]bool{
	"any": true, "bool": true, "byte": true, "comparable": true, "complex64": true, "complex128": true,
	"error": true, "float32": true, "float64": true, "int": true, "int8": true, "int16": true,
	"int32": true, "int64": true, "rune": true, "string": true, "uint": true, "uint8": true,
	"uint16": true, "uint32": true, "uint64": true, "uintptr": true,
}

// embeddedRef is the type reference of an embedded field or interface.
// The predeclared error interface is kept as "error", whose method set is
// known; other predeclared types have no methods and give "".
func embeddedRef(e ast.Expr, typeRef func(ast.Expr) string) string {
	if id, ok := e.(*ast.Ident); ok && goPredeclared[id.Name] {
		if id.Name == "error" {
			return "error"
		}
		return ""
	}
	return typeRef(e)
}

// goCalls collects call sites inside a function body, including calls inside
// closures, which are attributed to the enclosing function.
func goCalls(fset *token.FileSet, body *ast.BlockStmt, caller, qual string, aliases map[string]string, enc *goEncoder) []store.RawCall {
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
			if id, ok := t.X.(*ast.Ident); ok && enc.isPackage(id.Name, id.Pos()) {
				rc.Form, rc.A, rc.B = store.CallImport, aliases[id.Name], name
			} else if x := enc.encodeJSON(t.X); x != "" {
				// A method on a value: its type is worked out by the resolver.
				rc.Form, rc.A, rc.B = store.CallExpr, x, name
			} else {
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
