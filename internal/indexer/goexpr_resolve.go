package indexer

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"

	"github.com/anjanikumarjangid/codewiki/internal/store"
)

// goType is a type found for an expression: either a type expression
// written in a file, or a named type resolved to its qualified name.
type goType struct {
	expr  ast.Expr
	file  int64
	named string
	// bind gives the types of type parameters named in expr: the
	// arguments of a generic type's instantiation, or, for a type
	// parameter with a self-referential constraint (P Point[P]), the
	// constraint itself.
	bind map[string]*goType
}

// sub is a part of t's type expression, read in t's context.
func (t *goType) sub(e ast.Expr) *goType { return &goType{expr: e, file: t.file, bind: t.bind} }

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
		t := g.written(str(1), file)
		if t != nil && len(n) > 2 {
			if m, ok := n[2].(map[string]any); ok {
				t.bind = map[string]*goType{}
				for name, enc := range m {
					if b := g.eval(enc, file, depth+1); b != nil {
						t.bind[name] = b
					}
				}
			}
		}
		return t
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
			recv := g.eval(f[1], file, depth+1)
			if owner, d, ok := g.methodOf(recv, name, depth+1); ok {
				res := g.result(d, name, true)
				if res != nil {
					res.bind = g.instance(owner, recv, depth+1)
				}
				return res
			}
		}
		return nil
	case "T":
		// A type parameter of the function itself: its constraint, in
		// which the parameter's own name stands for the constraint.
		t := g.written(str(1), file)
		if t != nil {
			t.bind = map[string]*goType{str(2): t}
		}
		return t
	case "tp":
		// The constraint of type parameter number n[2] of the generic
		// type n[1], declared as `type Curve[P Point[P]] struct`.
		if len(n) < 3 {
			return nil
		}
		idx, _ := n[2].(float64)
		return g.typeParam(g.in.GoFiles[file].Qual+"."+str(1), int(idx))
	case "s":
		if len(n) < 3 {
			return nil
		}
		return g.fieldOf(g.eval(n[1], file, depth+1), str(2), depth+1)
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

// typeParams returns the type parameter list of the generic type q, from
// its declaration, and the file it is declared in.
func (g *goTyper) typeParams(q string) ([]*ast.Field, int64) {
	d := g.in.GoTypes[q]
	s := strings.TrimPrefix(d.Signature, "type ")
	open := strings.IndexByte(s, '[')
	if open < 0 || strings.IndexByte(s[:open], ' ') >= 0 {
		return nil, 0
	}
	// Parse the parameter list in a declaration of its own:
	// type X[P Point[P], Q any] int.
	f, err := parser.ParseFile(token.NewFileSet(), "", "package p; type X"+s[open:open+closing(s[open:])+1]+" int", 0)
	if err != nil || len(f.Decls) == 0 {
		return nil, 0
	}
	ts := f.Decls[0].(*ast.GenDecl).Specs[0].(*ast.TypeSpec)
	if ts.TypeParams == nil {
		return nil, 0
	}
	return ts.TypeParams.List, d.File
}

// typeParamNames lists the type parameter names of the generic type q.
func (g *goTyper) typeParamNames(q string) []string {
	list, _ := g.typeParams(q)
	var names []string
	for _, f := range list {
		for _, n := range f.Names {
			names = append(names, n.Name)
		}
	}
	return names
}

// typeParam returns the constraint of the i-th type parameter of the
// generic type q; in it, the parameter's own name stands for the
// constraint (P Point[P]).
func (g *goTyper) typeParam(q string, i int) *goType {
	list, file := g.typeParams(q)
	n := 0
	for _, f := range list {
		for _, name := range f.Names {
			if n == i {
				t := &goType{expr: f.Type, file: file}
				t.bind = map[string]*goType{name.Name: t}
				return t
			}
			n++
		}
	}
	return nil
}

// instance binds the type parameters of the generic type owner to the
// type arguments recv was instantiated with (Curve[P256Point]), so that a
// method result written as a parameter (func (c *Curve[P]) Base() P) gets
// the argument's type. It returns nil when recv is not an instance of
// owner, for example when the method is promoted from an embedded type.
func (g *goTyper) instance(owner string, recv *goType, depth int) map[string]*goType {
	if recv == nil || depth > maxTypeDepth {
		return nil
	}
	params := g.typeParamNames(owner)
	if len(params) == 0 {
		return nil
	}
	e := recv.expr
	for {
		if p, ok := e.(*ast.ParenExpr); ok {
			e = p.X
		} else if s, ok := e.(*ast.StarExpr); ok {
			e = s.X
		} else {
			break
		}
	}
	if id, ok := e.(*ast.Ident); ok { // a type parameter: what it is bound to
		if b, ok := recv.bind[id.Name]; ok && b != recv {
			return g.instance(owner, b, depth+1)
		}
		return nil
	}
	var args []ast.Expr
	switch x := e.(type) {
	case *ast.IndexExpr:
		args = []ast.Expr{x.Index}
	case *ast.IndexListExpr:
		args = x.Indices
	}
	if len(args) != len(params) {
		return nil
	}
	if q, _ := g.named(recv, depth); q != owner {
		return nil
	}
	bind := map[string]*goType{}
	for i, p := range params {
		bind[p] = recv.sub(args[i])
	}
	return bind
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
		if b, ok := t.bind[x.Name]; ok && b.expr != x {
			return g.named(b, depth+1)
		}
		if goPredeclared[x.Name] {
			return "", true
		}
		q = g.in.GoFiles[t.file].Qual + "." + x.Name
		if _, alias := g.names.aliases[q]; g.in.GoTypes[q].Kind == "" && !alias {
			// Not a type of the package: a type parameter, or a type
			// declared inside a function. Unknown, never "external".
			return "", false
		}
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
		return t.sub(x.Elt)
	case *ast.MapType:
		return t.sub(x.Value)
	case *ast.ChanType:
		return t.sub(x.Value)
	case *ast.Ellipsis:
		return t.sub(x.Elt)
	}
	if q, _ := g.named(t.sub(e), depth+1); q != "" {
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

// literal returns the struct or interface type literal t denotes, if it is
// one: the type of `var mu struct{ sync.Mutex; n int }`.
func literal(t *goType) ast.Expr {
	if t == nil || t.expr == nil {
		return nil
	}
	e := t.expr
	for {
		if p, ok := e.(*ast.ParenExpr); ok {
			e = p.X
		} else if s, ok := e.(*ast.StarExpr); ok {
			e = s.X
		} else {
			break
		}
	}
	switch e.(type) {
	case *ast.StructType, *ast.InterfaceType:
		return e
	}
	return nil
}

// embeddedName is the field name of an embedded field: its type's name.
func embeddedName(e ast.Expr) string {
	for {
		switch x := e.(type) {
		case *ast.StarExpr:
			e = x.X
		case *ast.IndexExpr:
			e = x.X
		case *ast.IndexListExpr:
			e = x.X
		case *ast.SelectorExpr:
			return x.Sel.Name
		case *ast.Ident:
			return x.Name
		default:
			return ""
		}
	}
}

// fieldOf returns the type of field name of t, a named type or a struct
// type literal, also when promoted from an embedded field.
func (g *goTyper) fieldOf(t *goType, name string, depth int) *goType {
	if t == nil || depth > maxTypeDepth {
		return nil
	}
	if q, _ := g.named(t, depth); q != "" {
		return g.field(q, name, depth)
	}
	st, ok := literal(t).(*ast.StructType)
	if !ok {
		return nil
	}
	var embedded []*goType
	for _, f := range st.Fields.List {
		if len(f.Names) == 0 {
			et := t.sub(f.Type)
			if embeddedName(f.Type) == name {
				return et
			}
			embedded = append(embedded, et)
			continue
		}
		for _, n := range f.Names {
			if n.Name == name {
				return t.sub(f.Type)
			}
		}
	}
	for _, et := range embedded {
		if ft := g.fieldOf(et, name, depth+1); ft != nil {
			return ft
		}
	}
	return nil
}

// methodOf finds method name on t: a named type, or a struct or interface
// type literal through its embedded types. It returns the declaring type.
func (g *goTyper) methodOf(t *goType, name string, depth int) (string, store.GoDecl, bool) {
	if t == nil || depth > maxTypeDepth {
		return "", store.GoDecl{}, false
	}
	if q, _ := g.named(t, depth); q != "" {
		return g.method(q, name, depth)
	}
	var embedded []ast.Expr
	switch lit := literal(t).(type) {
	case *ast.StructType:
		for _, f := range lit.Fields.List {
			if len(f.Names) == 0 {
				embedded = append(embedded, f.Type)
			}
		}
	case *ast.InterfaceType:
		for _, m := range lit.Methods.List {
			if len(m.Names) == 0 {
				embedded = append(embedded, m.Type)
			}
		}
	}
	for _, e := range embedded {
		if owner, d, ok := g.methodOf(t.sub(e), name, depth+1); ok {
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
	t := g.eval(node, file, 0)
	q, known := g.named(t, 0)
	switch {
	case !known:
		return r.byMethodName(c.B)
	case q == "":
		// A struct or interface literal: a method promoted from what it
		// embeds. Anything else (error, a slice, a function type): no edge.
		if owner, _, ok := g.methodOf(t, c.B, 0); ok {
			return r.exact(owner + "." + c.B)
		}
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
