package indexer

import (
	"go/ast"
	"go/parser"
	"go/types"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/anjanikumarjangid/codeweft/internal/store"
)

// resolveTypes builds the type hierarchy from declared supertypes, and for
// Go, from method sets:
//
//   - declared: Java extends/implements, Python base classes and Go
//     embedding are resolved like field types, inside or outside the
//     repository;
//   - method set: Go has no implements clause, so a Go type implements an
//     interface when its method set (its own methods plus those promoted
//     from embedded types) has every method of the interface with the same
//     parameter and result types. Type names are resolved through each
//     file's package and imports, so payments.Card written in one package
//     and Card written inside package payments compare equal, while
//     fs.File and http.File do not.
//
// An interface that embeds an interface from outside the repository has a
// method set the index cannot see in full, so no type is said to implement
// it: the relation would be a guess.
func resolveTypes(tx *store.Tx, r resolver, in *store.TypeInput) error {
	var edges []store.TypeEdge
	embeds := map[string][]string{} // Go type -> resolved embedded types ("" when unknown)
	for _, st := range in.Supertypes {
		sub, ok := r.in.Symbols[st.Owner]
		if !ok {
			continue
		}
		q, external := st.TypeRef, true
		if st.TypeRef != "error" {
			q, external = r.typeRef(st.TypeRef)
		}
		if q == "" {
			continue
		}
		var super int64
		if !external {
			super = r.in.Symbols[q] // 0 if a same-package name is not declared
		}
		edges = append(edges, store.TypeEdge{SubID: sub, SuperID: super, SuperRef: q, How: st.Kind})
		if _, isGo := in.GoTypes[st.Owner]; isGo {
			switch {
			case q == "error":
				embeds[st.Owner] = append(embeds[st.Owner], "error")
			case super == 0:
				embeds[st.Owner] = append(embeds[st.Owner], "")
			default:
				embeds[st.Owner] = append(embeds[st.Owner], q)
			}
		}
	}
	edges = append(edges, goImplementations(in, embeds, r.in.Symbols, r.internalQual)...)
	return tx.ReplaceTypeEdges(edges)
}

// promoted is a method in a method set: its shape, and how deep in the
// embedding it was found (0 for a method declared on the type itself).
type promoted struct {
	shape string
	depth int
	pkg   string // package of the type that declares the method
}

// methodSet is the methods of a Go type by name. complete is false when an
// embedded type outside the repository may contribute methods the index
// does not know.
type methodSet struct {
	methods  map[string]promoted
	complete bool
}

func goImplementations(in *store.TypeInput, embeds map[string][]string, ids map[string]int64, internal func(string) (string, bool)) []store.TypeEdge {
	names := newGoNames(in, internal)
	sets := map[string]methodSet{}
	var setOf func(t string, visiting map[string]bool) methodSet
	setOf = func(t string, visiting map[string]bool) methodSet {
		if ms, ok := sets[t]; ok {
			return ms
		}
		ms := methodSet{methods: map[string]promoted{}, complete: true}
		iface := in.GoTypes[t].Kind == "interface"
		for name, d := range in.GoMethods[t] {
			ms.methods[name] = promoted{shape: names.shape(d, name, iface), pkg: t[:strings.LastIndex(t, ".")]}
		}
		// Promoted methods follow Go's selector rule: the shallowest
		// embedding wins, and two at the same depth cancel each other out
		// (the selector would be ambiguous), hiding deeper ones too.
		// Interfaces are different: embedded interfaces may overlap, and the
		// method set is their union (identical signatures required).
		found := map[string][]promoted{}
		visiting[t] = true
		for _, e := range embeds[t] {
			switch {
			case e == "error":
				found["Error"] = append(found["Error"], promoted{shape: "()(string)", depth: 1})
			case e == "" || in.GoTypes[e].Kind == "":
				ms.complete = false
			case visiting[e]:
				// embedding cycle: invalid Go, ignore
			default:
				sub := setOf(e, visiting)
				for name, m := range sub.methods {
					found[name] = append(found[name], promoted{shape: m.shape, depth: m.depth + 1, pkg: m.pkg})
				}
				if !sub.complete {
					ms.complete = false
				}
			}
		}
		delete(visiting, t)
		for name, cands := range found {
			if _, own := ms.methods[name]; own {
				continue
			}
			best := cands[0]
			ambiguous := false
			for _, c := range cands[1:] {
				switch {
				case iface:
					ambiguous = ambiguous || c.shape != best.shape
				case c.depth < best.depth:
					best, ambiguous = c, false
				case c.depth == best.depth:
					ambiguous = true
				}
			}
			if ambiguous {
				best.shape = "" // present but unusable: matches no interface
			}
			ms.methods[name] = best
		}
		sets[t] = ms
		return ms
	}

	var ifaces, concrete []string
	for t, d := range in.GoTypes {
		if d.Kind == "interface" {
			ifaces = append(ifaces, t)
		} else {
			concrete = append(concrete, t)
		}
	}
	slices.Sort(ifaces)
	slices.Sort(concrete)
	byMethod := map[string][]string{} // method name -> concrete types with it
	for _, t := range concrete {
		for name := range setOf(t, map[string]bool{}).methods {
			byMethod[name] = append(byMethod[name], t)
		}
	}

	var edges []store.TypeEdge
	for _, i := range ifaces {
		want := setOf(i, map[string]bool{})
		if !want.complete || len(want.methods) == 0 {
			continue // unknown method set, or an empty interface everything satisfies
		}
		// Only types that have the interface's rarest method can match.
		rarest := ""
		for name := range want.methods {
			if rarest == "" || len(byMethod[name]) < len(byMethod[rarest]) ||
				(len(byMethod[name]) == len(byMethod[rarest]) && name < rarest) {
				rarest = name
			}
		}
	candidates:
		for _, t := range byMethod[rarest] {
			have := sets[t].methods
			for name, m := range want.methods {
				if m.shape == "" || have[name].shape != m.shape {
					continue candidates
				}
				// An unexported method name is only the same method when it
				// is declared in the same package as the interface's; a type
				// elsewhere can still have it, promoted from an embedded type.
				if r, _ := utf8.DecodeRuneInString(name); !unicode.IsUpper(r) && have[name].pkg != m.pkg {
					continue candidates
				}
			}
			edges = append(edges, store.TypeEdge{SubID: ids[t], SuperID: ids[i], SuperRef: i, How: store.SuperMethodSet})
		}
	}
	return edges
}

// goNames prints Go types in a canonical form, so signatures written in
// different files compare equal exactly when they denote the same types:
// every type name is qualified through the declaring file's package and
// imports, parameter names are dropped, predeclared aliases get one
// spelling (byte -> uint8, rune -> int32, interface{} -> any), and type
// aliases declared in the repository are replaced by what they stand for.
type goNames struct {
	files    map[int64]store.GoFile
	internal func(importPath string) (string, bool)
	aliases  map[string]goAlias // qualified alias -> its definition
}

type goAlias struct {
	expr ast.Expr
	file int64
}

func newGoNames(in *store.TypeInput, internal func(string) (string, bool)) *goNames {
	g := &goNames{files: in.GoFiles, internal: internal, aliases: map[string]goAlias{}}
	for q, d := range in.GoTypes {
		// Alias declarations are recorded as "type Name = Target".
		if i := strings.Index(d.Signature, " = "); d.Kind == "type" && i > 0 && !strings.Contains(d.Signature[:i], "[") {
			if e, err := parser.ParseExpr(d.Signature[i+3:]); err == nil {
				g.aliases[q] = goAlias{expr: e, file: d.File}
			}
		}
	}
	return g
}

// shape reduces a method or interface method declaration to its canonical
// parameter and result types, e.g. "(string,int64)(string,error)", or ""
// if the signature cannot be read.
func (g *goNames) shape(d store.GoDecl, name string, interfaceMethod bool) string {
	var rest string
	if interfaceMethod {
		rest = strings.TrimPrefix(d.Signature, name)
	} else {
		i := strings.Index(d.Signature, ") "+name+"(") // after the receiver
		if i < 0 {
			return ""
		}
		rest = d.Signature[i+2+len(name):]
	}
	e, err := parser.ParseExpr("func" + rest)
	if err != nil {
		return ""
	}
	ft, ok := e.(*ast.FuncType)
	if !ok {
		return ""
	}
	return g.fields(ft.Params, d.File, 0) + g.fields(ft.Results, d.File, 0)
}

func (g *goNames) fields(fl *ast.FieldList, file int64, depth int) string {
	var parts []string
	if fl != nil {
		for _, f := range fl.List {
			t := g.typeString(f.Type, file, depth)
			for range max(1, len(f.Names)) {
				parts = append(parts, t)
			}
		}
	}
	return "(" + strings.Join(parts, ",") + ")"
}

var goCanonical = map[string]string{"byte": "uint8", "rune": "int32"}

func (g *goNames) typeString(e ast.Expr, file int64, depth int) string {
	ts := func(x ast.Expr) string { return g.typeString(x, file, depth) }
	switch t := e.(type) {
	case *ast.Ident:
		if goPredeclared[t.Name] {
			if c, ok := goCanonical[t.Name]; ok {
				return c
			}
			return t.Name
		}
		return g.named(g.files[file].Qual+"."+t.Name, depth)
	case *ast.SelectorExpr:
		x, ok := t.X.(*ast.Ident)
		if !ok {
			return types.ExprString(e)
		}
		p, ok := g.files[file].Imports[x.Name]
		if !ok {
			return x.Name + "." + t.Sel.Name // unknown import: keep as written
		}
		if q, ok := g.internal(p); ok {
			p = q
		}
		return g.named(p+"."+t.Sel.Name, depth)
	case *ast.StarExpr:
		return "*" + ts(t.X)
	case *ast.ParenExpr:
		return ts(t.X)
	case *ast.Ellipsis:
		return "..." + ts(t.Elt)
	case *ast.ArrayType:
		if t.Len == nil {
			return "[]" + ts(t.Elt)
		}
		return "[" + types.ExprString(t.Len) + "]" + ts(t.Elt)
	case *ast.MapType:
		return "map[" + ts(t.Key) + "]" + ts(t.Value)
	case *ast.ChanType:
		switch t.Dir {
		case ast.SEND:
			return "chan<- " + ts(t.Value)
		case ast.RECV:
			return "<-chan " + ts(t.Value)
		}
		return "chan " + ts(t.Value)
	case *ast.FuncType:
		return "func" + g.fields(t.Params, file, depth) + g.fields(t.Results, file, depth)
	case *ast.InterfaceType:
		if t.Methods == nil || len(t.Methods.List) == 0 {
			return "any"
		}
	case *ast.IndexExpr:
		return ts(t.X) + "[" + ts(t.Index) + "]"
	case *ast.IndexListExpr:
		args := make([]string, len(t.Indices))
		for i, x := range t.Indices {
			args[i] = ts(x)
		}
		return ts(t.X) + "[" + strings.Join(args, ",") + "]"
	}
	return types.ExprString(e)
}

// named resolves a qualified type name through repository aliases.
func (g *goNames) named(q string, depth int) string {
	if a, ok := g.aliases[q]; ok && depth < 5 {
		return g.typeString(a.expr, a.file, depth+1)
	}
	return q
}
