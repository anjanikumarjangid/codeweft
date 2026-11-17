package indexer

import (
	"encoding/json"
	"go/ast"
	"go/token"
)

// Go expression typing.
//
// The parser cannot know the type of a call's receiver when it depends on
// other packages (x := store.Open(); x.Close()), so it records how the
// type can be found, and the resolver works it out once every package is
// indexed. An expression is encoded as a JSON array:
//
//	["t", TYPE]          a type written in this file, e.g. "*Client" or "[]io.Reader"
//	["g", NAME]          a package-level name of this file's package
//	["p", PATH, NAME]    a package-level name of the imported package PATH
//	["c", FUNC]          the first result of calling FUNC, which is
//	    ["f", NAME]          a function (or a type: a conversion) of this package
//	    ["fp", PATH, NAME]   a function or type of the imported package PATH
//	    ["m", EXPR, NAME]    the method NAME on EXPR
//	["s", EXPR, NAME]    the field NAME of EXPR
//	["e", EXPR]          an element of EXPR: an index, or a range value
//
// Pointers and values are not told apart: a method is found either way.
// Anything else (a function value, a closure result, an untyped constant)
// is unknown, and the call falls back to the other resolution rules.

// goLocal is how a local name got its value.
type goLocal struct {
	typ    ast.Expr // a stated type: parameter, var x T, x.(T)
	val    ast.Expr // x := val (the first result, for a call)
	elemOf ast.Expr // for _, x := range elemOf
}

type goEncoder struct {
	fset     *token.FileSet
	src      []byte
	recvVar  string
	recvType ast.Expr
	aliases  map[string]string // import name -> path
	locals   map[string]goLocal
	visiting map[string]bool
}

// goLocals records the local names of a function and how each got its
// value. Scopes are ignored: a name declared twice keeps its last use.
func goLocals(d *ast.FuncDecl) map[string]goLocal {
	locals := map[string]goLocal{}
	addFields := func(fl *ast.FieldList) {
		if fl == nil {
			return
		}
		for _, f := range fl.List {
			for _, n := range f.Names {
				locals[n.Name] = goLocal{typ: f.Type}
			}
		}
	}
	addFields(d.Type.Params)
	addFields(d.Type.Results)
	if d.Body == nil {
		return locals
	}
	ast.Inspect(d.Body, func(n ast.Node) bool {
		switch t := n.(type) {
		case *ast.ValueSpec:
			for i, name := range t.Names {
				switch {
				case t.Type != nil:
					locals[name.Name] = goLocal{typ: t.Type}
				case len(t.Values) == len(t.Names):
					locals[name.Name] = goLocal{val: t.Values[i]}
				case len(t.Values) == 1 && i == 0:
					locals[name.Name] = goLocal{val: t.Values[0]}
				}
			}
		case *ast.AssignStmt:
			if t.Tok != token.DEFINE {
				return true
			}
			for i, lhs := range t.Lhs {
				id, ok := lhs.(*ast.Ident)
				if !ok || id.Name == "_" {
					continue
				}
				switch {
				case len(t.Rhs) == len(t.Lhs):
					locals[id.Name] = goLocal{val: t.Rhs[i]}
				case len(t.Rhs) == 1 && i == 0:
					// x, err := f() / v, ok := m[k] / x, ok := y.(T): the first
					// result is what the expression denotes.
					locals[id.Name] = goLocal{val: t.Rhs[0]}
				}
			}
		case *ast.RangeStmt:
			if t.Tok == token.DEFINE && t.Value != nil {
				if id, ok := t.Value.(*ast.Ident); ok && id.Name != "_" {
					locals[id.Name] = goLocal{elemOf: t.X}
				}
			}
		case *ast.TypeSwitchStmt:
			// switch v := x.(type): v has a different type in every case.
			if a, ok := t.Assign.(*ast.AssignStmt); ok && len(a.Lhs) == 1 {
				if id, ok := a.Lhs[0].(*ast.Ident); ok {
					delete(locals, id.Name)
				}
			}
		case *ast.FuncLit:
			addFields(t.Type.Params)
		}
		return true
	})
	return locals
}

func (g *goEncoder) typeText(e ast.Expr) string { return printNode(g.fset, e) }

// encode returns the encoding of e, or nil when its type cannot be found.
func (g *goEncoder) encode(e ast.Expr) any {
	switch t := e.(type) {
	case *ast.ParenExpr:
		return g.encode(t.X)
	case *ast.StarExpr: // *p: the pointed-to value has the same methods
		return g.encode(t.X)
	case *ast.UnaryExpr:
		if t.Op == token.AND { // &x
			return g.encode(t.X)
		}
		return nil
	case *ast.CompositeLit:
		if t.Type != nil {
			return []any{"t", g.typeText(t.Type)}
		}
		return nil
	case *ast.TypeAssertExpr:
		if t.Type != nil {
			return []any{"t", g.typeText(t.Type)}
		}
		return nil
	case *ast.Ident:
		return g.ident(t.Name)
	case *ast.SelectorExpr:
		if id, ok := t.X.(*ast.Ident); ok && g.isPackage(id.Name) {
			return []any{"p", g.aliases[id.Name], t.Sel.Name}
		}
		if x := g.encode(t.X); x != nil {
			return []any{"s", x, t.Sel.Name}
		}
		return nil
	case *ast.IndexExpr:
		if x := g.encode(t.X); x != nil {
			return []any{"e", x}
		}
		return nil
	case *ast.CallExpr:
		return g.call(t)
	}
	return nil
}

// isPackage reports whether name refers to an imported package here, and
// not to a local that shadows it.
func (g *goEncoder) isPackage(name string) bool {
	if _, local := g.locals[name]; local || name == g.recvVar {
		return false
	}
	_, ok := g.aliases[name]
	return ok
}

func (g *goEncoder) ident(name string) any {
	if name == g.recvVar && g.recvType != nil {
		return []any{"t", g.typeText(g.recvType)}
	}
	l, ok := g.locals[name]
	if !ok {
		if goPredeclared[name] || name == "nil" || name == "true" || name == "false" || name == "iota" {
			return nil
		}
		return []any{"g", name}
	}
	if g.visiting[name] { // x := f(x)
		return nil
	}
	g.visiting[name] = true
	defer delete(g.visiting, name)
	switch {
	case l.typ != nil:
		return []any{"t", g.typeText(l.typ)}
	case l.val != nil:
		return g.encode(l.val)
	case l.elemOf != nil:
		if x := g.encode(l.elemOf); x != nil {
			return []any{"e", x}
		}
	}
	return nil
}

func (g *goEncoder) call(c *ast.CallExpr) any {
	fun := ast.Unparen(c.Fun)
	for { // generic instantiation f[T](x)
		if ix, ok := fun.(*ast.IndexExpr); ok {
			fun = ix.X
		} else if ix, ok := fun.(*ast.IndexListExpr); ok {
			fun = ix.X
		} else {
			break
		}
	}
	switch f := fun.(type) {
	case *ast.Ident:
		if _, local := g.locals[f.Name]; local {
			return nil // a function value
		}
		switch f.Name {
		case "new":
			if len(c.Args) == 1 {
				return []any{"t", g.typeText(c.Args[0])}
			}
			return nil
		case "make":
			if len(c.Args) > 0 {
				return []any{"t", g.typeText(c.Args[0])}
			}
			return nil
		case "append":
			if len(c.Args) > 0 {
				return g.encode(c.Args[0])
			}
			return nil
		}
		if goBuiltins[f.Name] || goPredeclared[f.Name] {
			return nil
		}
		return []any{"c", []any{"f", f.Name}}
	case *ast.SelectorExpr:
		if id, ok := f.X.(*ast.Ident); ok && g.isPackage(id.Name) {
			return []any{"c", []any{"fp", g.aliases[id.Name], f.Sel.Name}}
		}
		if x := g.encode(f.X); x != nil {
			return []any{"c", []any{"m", x, f.Sel.Name}}
		}
	case *ast.ArrayType, *ast.MapType, *ast.ChanType, *ast.FuncType, *ast.InterfaceType, *ast.StarExpr:
		return []any{"t", g.typeText(fun)} // a conversion: []byte(s)
	}
	return nil
}

// encodeJSON returns the JSON encoding of e, or "" when it is unknown.
func (g *goEncoder) encodeJSON(e ast.Expr) string {
	v := g.encode(e)
	if v == nil {
		return ""
	}
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}
