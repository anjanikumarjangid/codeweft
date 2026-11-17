package indexer

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"strings"

	"github.com/anjanikumarjangid/codewiki/internal/store"
)

// goType is a type found for an expression: either a type expression
// written in a file, or a named type resolved to its qualified name.
type goType struct {
	expr  ast.Expr
	file  int64
	named string
}

// goTyper works out the types of encoded Go expressions (see goexpr.go)
// from the declarations in the index: function and method results, field
// types, package-level variables, element types, aliases and embedding.
type goTyper struct {
	in       *store.TypeInput
	names    *goNames
	internal func(importPath string) (string, bool)
	embeds   map[string][]string // Go type -> embedded types inside the repository
	exprs    map[string]ast.Expr // parsed type expressions, by text
	vars     map[string]*goType  // package-level variable -> its type (nil: unknown)
}

const maxTypeDepth = 12

func newGoTyper(in *store.TypeInput, r resolver) *goTyper {
	g := &goTyper{in: in, names: newGoNames(in, r.internalQual), internal: r.internalQual,
		embeds: map[string][]string{}, exprs: map[string]ast.Expr{}, vars: map[string]*goType{}}
	for _, st := range in.Supertypes {
		if _, isGo := in.GoTypes[st.Owner]; !isGo || (st.Kind != store.SuperEmbeds && st.Kind != store.SuperExtends) {
			continue
		}
		if q, external := r.typeRef(st.TypeRef); !external && in.GoTypes[q].Kind != "" {
			g.embeds[st.Owner] = append(g.embeds[st.Owner], q)
		}
	}
	return g
}

func (g *goTyper) parse(text string) ast.Expr {
	if e, ok := g.exprs[text]; ok {
		return e
	}
	e, err := parser.ParseExpr(text)
	if err != nil {
		e = nil
	}
	g.exprs[text] = e
	return e
}

func (g *goTyper) written(text string, file int64) *goType {
	if e := g.parse(text); e != nil {
		return &goType{expr: e, file: file}
	}
	return nil
}

// qualify turns an import path into the qualifier symbols use.
func (g *goTyper) qualify(path string) string {
	if q, ok := g.internal(path); ok {
		return q
	}
	return path
}

// eval returns the type of an encoded expression found in file, or nil.
func (g *goTyper) eval(node any, file int64, depth int) *goType {
	n, ok := node.([]any)
	if !ok || len(n) == 0 || depth > maxTypeDepth {
		return nil
	}
	str := func(i int) string {
		if i < len(n) {
			s, _ := n[i].(string)
			return s
		}
		return ""
	}
	switch str(0) {
	case "t":
		return g.written(str(1), file)
	case "g":
		return g.variable(g.in.GoFiles[file].Qual+"."+str(1), depth)
	case "p":
		return g.variable(g.qualify(str(1))+"."+str(2), depth)
	case "c":
		f, ok := n[1].([]any)
		if !ok || len(f) < 2 {
			return nil
		}
		switch f[0] {
		case "f":
			name, _ := f[1].(string)
			return g.callee(g.in.GoFiles[file].Qual+"."+name, depth)
		case "fp":
			if len(f) < 3 {
				return nil
			}
			path, _ := f[1].(string)
			name, _ := f[2].(string)
			return g.callee(g.qualify(path)+"."+name, depth)
		case "m":
			if len(f) < 3 {
				return nil
			}
			name, _ := f[2].(string)
			q, _ := g.named(g.eval(f[1], file, depth+1), depth+1)
			if q == "" {
				return nil
			}
			if _, d, ok := g.method(q, name, depth+1); ok {
				return g.result(d, name, true)
			}
		}
		return nil
	case "s":
		if len(n) < 3 {
			return nil
		}
		q, _ := g.named(g.eval(n[1], file, depth+1), depth+1)
		if q == "" {
			return nil
		}
		return g.field(q, str(2), depth+1)
	case "e":
		if len(n) < 2 {
			return nil
		}
		return g.elem(g.eval(n[1], file, depth+1), depth+1)
	}
	return nil
}

// variable returns the type of a package-level variable.
func (g *goTyper) variable(q string, depth int) *goType {
	if t, ok := g.vars[q]; ok {
		return t
	}
	d, ok := g.in.GoVars[q]
	if !ok {
		return nil
	}
	g.vars[q] = nil // guards against var a = b; var b = a
	var node any
	if json.Unmarshal([]byte(d.Signature), &node) != nil {
		return nil
	}
	t := g.eval(node, d.File, depth+1)
	g.vars[q] = t
	return t
}

// callee returns the result type of calling q: a function, or a type
// (a conversion such as Celsius(x)).
func (g *goTyper) callee(q string, depth int) *goType {
	if g.in.GoTypes[q].Kind != "" {
		return &goType{named: q}
	}
	if d, ok := g.in.GoFuncs[q]; ok {
		return g.result(d, q[strings.LastIndex(q, ".")+1:], false)
	}
	return nil
}

// result returns the first result type of a function or method declaration.
func (g *goTyper) result(d store.GoDecl, name string, isMethod bool) *goType {
	sig := d.Signature
	var rest string
	if isMethod {
		if d.Kind == "interface method" {
			rest = strings.TrimPrefix(sig, name)
		} else if i := strings.Index(sig, ") "+name+"("); i >= 0 {
			rest = sig[i+2+len(name):]
		}
	} else if i := strings.Index(sig, "func "+name); i >= 0 {
		rest = sig[i+5+len(name):]
		if strings.HasPrefix(rest, "[") { // type parameters
			rest = rest[closing(rest)+1:]
		}
	}
	if rest == "" {
		return nil
	}
	ft, ok := g.parse("func" + rest).(*ast.FuncType)
	if !ok || ft.Results == nil || len(ft.Results.List) == 0 {
		return nil
	}
	return &goType{expr: ft.Results.List[0].Type, file: d.File}
}

// closing returns the index of the bracket closing s[0].
func closing(s string) int {
	depth := 0
	for i, r := range s {
		switch r {
		case '[', '(':
			depth++
		case ']', ')':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return len(s) - 1
}

// named returns the qualified named type that t denotes once pointers are
// stripped, and whether t was known at all: a known type that is not a
// named type of the repository or an import (a slice, a function type, a
// predeclared type) gives "", true.
func (g *goTyper) named(t *goType, depth int) (string, bool) {
	if t == nil || depth > maxTypeDepth {
		return "", false
	}
	if t.named != "" {
		return t.named, true
	}
	e := t.expr
	for {
		switch x := e.(type) {
		case *ast.ParenExpr:
			e = x.X
			continue
		case *ast.StarExpr:
			e = x.X
			continue
		case *ast.IndexExpr: // a generic type instantiated: List[int]
			e = x.X
			continue
		case *ast.IndexListExpr:
			e = x.X
			continue
		}
		break
	}
	var q string
	switch x := e.(type) {
	case *ast.Ident:
		if goPredeclared[x.Name] {
			return "", true
		}
		q = g.in.GoFiles[t.file].Qual + "." + x.Name
	case *ast.SelectorExpr:
		id, ok := x.X.(*ast.Ident)
		if !ok {
			return "", false
		}
		path, ok := g.in.GoFiles[t.file].Imports[id.Name]
		if !ok {
			return "", false
		}
		q = g.qualify(path) + "." + x.Sel.Name
	default:
		return "", true
	}
	if a, ok := g.names.aliases[q]; ok {
		return g.named(&goType{expr: a.expr, file: a.file}, depth+1)
	}
	return q, true
}

// underlying returns the definition of a named non-struct, non-interface
// type of the repository: Celsius in `type Temps []Celsius`.
func (g *goTyper) underlying(q string) *goType {
	d := g.in.GoTypes[q]
	if d.Kind != "type" {
		return nil
	}
	// "type Name[T any] Definition": skip the name and type parameters.
	s := strings.TrimPrefix(d.Signature, "type ")
	i := 0
	for i < len(s) && s[i] != ' ' && s[i] != '[' {
		i++
	}
	if i < len(s) && s[i] == '[' {
		i += closing(s[i:]) + 1
	}
	s = strings.TrimSpace(s[i:])
	if strings.HasPrefix(s, "=") {
		return nil // an alias: followed by named
	}
	return g.written(s, d.File)
}

// elem returns the element type of a slice, array, map (its values) or
// channel, also through a named type defined as one.
func (g *goTyper) elem(t *goType, depth int) *goType {
	if t == nil || depth > maxTypeDepth {
		return nil
	}
	if t.named != "" {
		return g.elem(g.underlying(t.named), depth+1)
	}
	e := t.expr
	for {
		if p, ok := e.(*ast.ParenExpr); ok {
			e = p.X
		} else if s, ok := e.(*ast.StarExpr); ok { // *[N]T
			e = s.X
		} else {
			break
		}
	}
	switch x := e.(type) {
	case *ast.ArrayType:
		return &goType{expr: x.Elt, file: t.file}
	case *ast.MapType:
		return &goType{expr: x.Value, file: t.file}
	case *ast.ChanType:
		return &goType{expr: x.Value, file: t.file}
	case *ast.Ellipsis:
		return &goType{expr: x.Elt, file: t.file}
	}
	if q, _ := g.named(&goType{expr: e, file: t.file}, depth+1); q != "" {
		return g.elem(g.underlying(q), depth+1)
	}
	return nil
}

// field returns the type of field name of type q, also when promoted from
// an embedded type (the shallowest wins).
func (g *goTyper) field(q, name string, depth int) *goType {
	if depth > maxTypeDepth {
		return nil
	}
	if d, ok := g.in.GoFields[[2]string{q, name}]; ok {
		return g.written(d.Signature, d.File)
	}
	for _, e := range g.embeds[q] {
		if t := g.field(e, name, depth+1); t != nil {
			return t
		}
	}
	return nil
}

// method finds method name on type q: declared on q, or promoted from an
// embedded type, or inherited by an interface from an embedded interface.
// It returns the type that declares it.
func (g *goTyper) method(q, name string, depth int) (string, store.GoDecl, bool) {
	if depth > maxTypeDepth {
		return "", store.GoDecl{}, false
	}
	if d, ok := g.in.GoMethods[q][name]; ok {
		return q, d, true
	}
	for _, e := range g.embeds[q] {
		if owner, d, ok := g.method(e, name, depth+1); ok {
			return owner, d, true
		}
	}
	return "", store.GoDecl{}, false
}

// exprCall resolves a Go method call whose receiver was encoded by the
// parser. A receiver of unknown type falls back to the unique method name;
// one of a known type that is not a named type (a predeclared type such
// as error, a slice, a function type) gives no edge rather than a guess.
func (r resolver) exprCall(c store.RawCall, path string) (int64, string, string) {
	g := r.typer
	file, ok := g.in.GoPaths[path]
	var node any
	if !ok || json.Unmarshal([]byte(c.A), &node) != nil {
		return r.byMethodName(c.B)
	}
	q, known := g.named(g.eval(node, file, 0), 0)
	switch {
	case !known:
		return r.byMethodName(c.B)
	case q == "":
		return 0, "", ""
	case g.in.GoTypes[q].Kind == "":
		return 0, q + "." + c.B, "external" // a type outside the repository
	}
	if owner, _, ok := g.method(q, c.B, 0); ok {
		return r.exact(owner + "." + c.B)
	}
	if g.field(q, c.B, 0) != nil {
		return 0, "", "" // calling a field holding a function value
	}
	return r.byMethodName(c.B) // e.g. promoted from a type outside the repository
}
