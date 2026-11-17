package indexer

import (
	"strings"

	"github.com/anjanikumarjangid/codewiki/internal/store"
)

// resolve turns every raw call site in the index into an edge.
//
// Resolution is syntactic, not type-checked: it never needs the repository
// to build or its dependencies to be downloaded. In order of preference:
//
//   - exact: the callee's qualified name is known from the syntax
//     (same-package call, call on the method receiver, call through a
//     package import, or call through a receiver field whose declared type
//     is known);
//   - external: the callee is in an imported package outside the repository
//     (kept as a reference such as "database/sql.Open");
//   - typed: the value's type is stated in the syntax (a parameter, a
//     `var x T` declaration or an `x := T{}` literal), so the method is
//     looked up on that type, inside or outside the repository;
//   - name: the call is on a value of unknown type, and exactly one method
//     with that name exists in the repository.
//
// Anything else (ambiguous names, calls of function values) is dropped
// rather than guessed.
func resolve(tx *store.Tx, module string) (int, error) {
	in, err := tx.LoadResolveInput(module)
	if err != nil {
		return 0, err
	}
	r := resolver{in}
	var edges []store.Edge
	resolved := 0
	for _, c := range in.Calls {
		from, ok := in.Symbols[c.Caller]
		if !ok {
			continue
		}
		toID, ref, conf := r.call(c)
		if ref == "" {
			continue
		}
		if toID != 0 {
			resolved++
		}
		edges = append(edges, store.Edge{FromID: from, ToID: toID, ToRef: ref, Line: c.Line, Confidence: conf})
	}
	return resolved, tx.ReplaceEdges(edges)
}

type resolver struct{ in *store.ResolveInput }

func (r resolver) exact(q string) (int64, string, string) {
	if id, ok := r.in.Symbols[q]; ok {
		return id, q, "exact"
	}
	return 0, "", ""
}

// byMethodName links to the only method with this name, if there is one.
// Overloads (several methods sharing one qualified name) count as one.
func (r resolver) byMethodName(name string) (int64, string, string) {
	ids := r.in.Methods[name]
	if len(ids) == 0 {
		return 0, "", ""
	}
	q := r.in.Qualified[ids[0]]
	for _, id := range ids[1:] {
		if r.in.Qualified[id] != q {
			return 0, "", ""
		}
	}
	return r.in.Symbols[q], q, "name"
}

// internalQual maps a Go import path inside the module to its qualifier.
func (r resolver) internalQual(importPath string) (string, bool) {
	m := r.in.Module
	if m == "" {
		return "", false
	}
	dir := ""
	switch {
	case importPath == m:
		dir = "."
	case strings.HasPrefix(importPath, m+"/"):
		dir = strings.TrimPrefix(importPath, m+"/")
	default:
		return "", false
	}
	q, ok := r.in.DirQual[dir]
	return q, ok
}

// typeRef resolves a type reference to a qualified type name, reporting
// whether the type lives outside the repository. Reference forms:
//
//	"pkg.Type"     Go same-package type: always internal
//	"@path.Type"   explicit: a Go import path, or a Java/Python full name
//	"~mod.Type"    guess for Java/Python: same package or module first,
//	               then the short name
//	"?Type"        short name: internal only if exactly one type has it
func (r resolver) typeRef(ref string) (q string, external bool) {
	switch {
	case ref == "":
		return "", false
	case strings.HasPrefix(ref, "@"):
		ref = ref[1:]
		if _, ok := r.in.Symbols[ref]; ok {
			return ref, false
		}
		if i := strings.LastIndex(ref, "."); i > 0 {
			if qual, ok := r.internalQual(ref[:i]); ok {
				return qual + "." + ref[i+1:], false
			}
		}
		return ref, true
	case strings.HasPrefix(ref, "~"):
		ref = ref[1:]
		if _, ok := r.in.Symbols[ref]; ok {
			return ref, false
		}
		return r.typeRef("?" + ref[strings.LastIndex(ref, ".")+1:])
	case strings.HasPrefix(ref, "?"):
		name := ref[1:]
		if qs := r.in.Types[name]; len(qs) == 1 {
			return qs[0], false
		}
		return name, true
	}
	return ref, false
}

func (r resolver) call(c store.RawCall) (int64, string, string) {
	switch c.Form {
	case store.CallQualified:
		if id, q, conf := r.exact(c.A); id != 0 {
			return id, q, conf
		}
		// A method call on the receiver that is not declared on the receiver
		// type is usually promoted from an embedded field.
		if c.B != "" {
			return r.byMethodName(c.B)
		}
		return 0, "", ""

	case store.CallImport:
		if id, q, conf := r.exact(c.A + "." + c.B); id != 0 { // Java/Python modules
			return id, q, conf
		}
		if qual, ok := r.internalQual(c.A); ok { // Go packages inside the module
			return r.exact(qual + "." + c.B)
		}
		return 0, c.A + "." + c.B, "external"

	case store.CallTyped:
		t, external := r.typeRef(c.A)
		if t == "" {
			return 0, "", ""
		}
		if external {
			return 0, t + "." + c.B, "external"
		}
		if id, q, conf := r.exact(t + "." + c.B); id != 0 {
			return id, q, conf
		}
		// `new Order(...)` on a type without an explicit constructor.
		if c.B == t[strings.LastIndex(t, ".")+1:] {
			return r.exact(t)
		}
		return r.byMethodName(c.B)

	case store.CallField:
		owner, external := r.typeRef(c.A)
		if external {
			return 0, "", "" // a field of a type outside the repository
		}
		if ref, ok := r.in.Fields[[2]string{owner, c.B}]; ok {
			t, external := r.typeRef(ref)
			if external {
				return 0, t + "." + c.C, "external"
			}
			if id, q, conf := r.exact(t + "." + c.C); id != 0 {
				return id, q, conf
			}
		}
		return r.byMethodName(c.C)

	case store.CallTypeName:
		if qs := r.in.Types[c.A]; len(qs) == 1 {
			if id, q, conf := r.exact(qs[0] + "." + c.B); id != 0 {
				return id, q, conf
			}
		}
		return r.byMethodName(c.B)

	case store.CallMethod:
		return r.byMethodName(c.B)
	}
	return 0, "", ""
}
