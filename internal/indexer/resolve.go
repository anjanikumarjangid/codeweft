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
	types, err := tx.LoadTypeInput()
	if err != nil {
		return 0, err
	}
	r := resolver{in: in}
	// Twice: Java supertypes may be nested types inherited from other
	// supertypes, which needs the first pass to look them up.
	r.supers = r.declaredSupers(types.Supertypes)
	r.supers = r.declaredSupers(types.Supertypes)
	r.typer = newGoTyper(types, r)
	var edges []store.Edge
	var testEdges []store.TestEdge
	resolved := 0
	for i, c := range in.Calls {
		path := in.CallFiles[i]
		// Calls made in tests: into the repository's code, or into another
		// test function such as a helper.
		if from, ok := in.TestFuncs[c.Caller]; ok {
			if toID, _, _ := r.callIn(c, path); toID != 0 {
				testEdges = append(testEdges, store.TestEdge{FromTest: from, ToID: toID, Line: c.Line})
			} else if to := r.testCallee(c); to != 0 && to != from {
				testEdges = append(testEdges, store.TestEdge{FromTest: from, ToTest: to, Line: c.Line})
			}
		}
		from, ok := in.Symbols[c.Caller]
		if !ok {
			continue
		}
		toID, ref, conf := r.callIn(c, path)
		if ref == "" {
			continue
		}
		if toID != 0 {
			resolved++
		}
		edges = append(edges, store.Edge{FromID: from, ToID: toID, ToRef: ref, Line: c.Line, Confidence: conf})
	}
	if err := tx.ReplaceEdges(edges); err != nil {
		return 0, err
	}
	if err := tx.ReplaceTestEdges(testEdges); err != nil {
		return 0, err
	}
	return resolved, resolveTypes(tx, r, types)
}

type resolver struct {
	in    *store.ResolveInput
	typer *goTyper
	// supers lists each type's declared supertypes (extends and implements,
	// not Go embedding), in declaration order: for a Java class its
	// superclass first, for a Python class its bases left to right.
	supers map[string][]superType
}

type superType struct {
	q        string // qualified name
	kind     string // store.SuperExtends or store.SuperImplements
	external bool   // declared outside the repository
}

func (r resolver) declaredSupers(sts []store.Supertype) map[string][]superType {
	out := map[string][]superType{}
	for _, st := range sts {
		if st.Kind != store.SuperExtends && st.Kind != store.SuperImplements {
			continue
		}
		if q, external := r.typeRef(st.TypeRef); q != "" {
			out[st.Owner] = append(out[st.Owner], superType{q: q, kind: st.Kind, external: external})
		}
	}
	return out
}

// javaLang are the java.lang types used without an import.
var javaLang = map[string]bool{
	"Object": true, "String": true, "StringBuilder": true, "CharSequence": true, "Integer": true, "Long": true,
	"Short": true, "Byte": true, "Character": true, "Boolean": true, "Double": true, "Float": true, "Number": true,
	"Math": true, "StrictMath": true, "System": true, "Thread": true, "ThreadLocal": true, "Runtime": true,
	"Class": true, "ClassLoader": true, "Enum": true, "Record": true, "Iterable": true, "Comparable": true,
	"Runnable": true, "AutoCloseable": true, "Cloneable": true, "Appendable": true, "Readable": true, "Void": true,
	"Throwable": true, "Exception": true, "Error": true, "RuntimeException": true, "AssertionError": true,
	"IllegalArgumentException": true, "IllegalStateException": true, "NullPointerException": true,
	"UnsupportedOperationException": true, "IndexOutOfBoundsException": true, "ArrayIndexOutOfBoundsException": true,
	"ClassCastException": true, "ArithmeticException": true, "InterruptedException": true,
	"CloneNotSupportedException": true, "NumberFormatException": true, "SecurityException": true,
	"StackOverflowError": true, "OutOfMemoryError": true, "ReflectiveOperationException": true,
	"StackTraceElement": true, "Process": true, "ProcessBuilder": true, "SuppressWarnings": true, "Override": true,
	"Deprecated": true, "FunctionalInterface": true, "SafeVarargs": true,
}

// scopedType resolves a Java simple type name as written inside a class:
// "SCOPE|Name" or "SCOPE|Outer.Inner". Like javac, it looks for a member
// type of the class, of its enclosing classes, and of their supertypes,
// then a type of the package, then java.lang; else a short name that only
// one type has.
func (r resolver) scopedType(ref string) (string, bool) {
	scope, name, _ := strings.Cut(ref, "|")
	first, rest, _ := strings.Cut(name, ".")
	join := func(q string) string {
		if rest != "" {
			return q + "." + rest
		}
		return q
	}
	for s := scope; r.isType(s); s = s[:max(strings.LastIndex(s, "."), 0)] {
		if q := r.memberType(s, first, map[string]bool{}); q != "" {
			return join(q), false
		}
		if !strings.Contains(s, ".") {
			break
		}
	}
	// The package: the longest prefix of the scope that declares the type
	// (the scope itself may not be indexed, such as a class in a test file).
	for p := scope; p != ""; p = p[:max(strings.LastIndex(p, "."), 0)] {
		if r.isType(p + "." + first) {
			return join(p + "." + first), false
		}
		if !strings.Contains(p, ".") {
			break
		}
	}
	if javaLang[first] {
		return join("java.lang." + first), true
	}
	return r.typeRef("?" + name[strings.LastIndex(name, ".")+1:])
}

// memberType finds the member type name declared in type t or inherited
// from its supertypes.
func (r resolver) memberType(t, name string, seen map[string]bool) string {
	if seen[t] {
		return ""
	}
	seen[t] = true
	if r.isType(t + "." + name) {
		return t + "." + name
	}
	for _, s := range r.supers[t] {
		if !s.external {
			if q := r.memberType(s.q, name, seen); q != "" {
				return q
			}
		}
	}
	return ""
}

// isType reports whether q is a type declared in the repository.
func (r resolver) isType(q string) bool {
	id, ok := r.in.Symbols[q]
	if !ok {
		return false
	}
	switch r.in.Kinds[id] {
	case "class", "interface", "enum", "record", "struct", "type", "annotation":
		return true
	}
	return false
}

// inherited finds method m declared on type t or on one of its supertypes,
// searched depth first in declaration order.
func (r resolver) inherited(t, m string, seen map[string]bool) (int64, string) {
	if seen[t] {
		return 0, ""
	}
	seen[t] = true
	if id, ok := r.in.Symbols[t+"."+m]; ok {
		return id, t + "." + m
	}
	for _, s := range r.supers[t] {
		if !s.external {
			if id, q := r.inherited(s.q, m, seen); id != 0 {
				return id, q
			}
		}
	}
	return 0, ""
}

// inheritedOrEnclosing resolves a call of m without a receiver in type
// owner: owner's own or inherited method, else, for a nested class, the
// method of the nearest enclosing class that has one (also inherited).
func (r resolver) inheritedOrEnclosing(owner, m string) (int64, string) {
	for t := owner; r.isType(t); t = t[:max(strings.LastIndex(t, "."), 0)] {
		if id, q := r.inherited(t, m, map[string]bool{}); id != 0 {
			return id, q
		}
		if !strings.Contains(t, ".") {
			break
		}
	}
	return 0, ""
}

// superCall resolves super.m() (or super().m() in Python) made in type
// owner: the first supertype, searched depth-first in declaration order,
// that declares m. The search starts from what owner extends: in Java
// super.m() names the superclass (with everything it inherits, interfaces
// included), never an interface of owner itself, which takes
// Interface.super.m(); in Python every base class counts as extended, and
// the order is the method resolution order except in diamond-shaped
// hierarchies. With m == "", super(...) in a Java constructor: the
// constructor of the superclass. A method found on no indexed supertype
// is attributed to the first supertype outside the repository, if any.
func (r resolver) superCall(owner, m string) (int64, string, string) {
	seen := map[string]bool{owner: true}
	firstExternal := ""
	var search func(t string) (int64, string)
	search = func(t string) (int64, string) {
		for _, s := range r.supers[t] {
			if seen[s.q] || (t == owner && s.kind != store.SuperExtends) {
				continue
			}
			seen[s.q] = true
			if s.external {
				if firstExternal == "" {
					firstExternal = s.q
				}
				continue
			}
			if m == "" { // constructor: only the superclass, named after it
				short := s.q[strings.LastIndex(s.q, ".")+1:]
				if id, ok := r.in.Symbols[s.q+"."+short]; ok {
					return id, s.q + "." + short
				}
				return r.in.Symbols[s.q], s.q // implicit default constructor
			}
			if id, ok := r.in.Symbols[s.q+"."+m]; ok {
				return id, s.q + "." + m
			}
			if id, q := search(s.q); id != 0 {
				return id, q
			}
		}
		return 0, ""
	}
	if id, q := search(owner); id != 0 {
		return id, q, "exact"
	}
	if firstExternal != "" && m != "" {
		return 0, firstExternal + "." + m, "external"
	}
	return 0, "", ""
}

// testCallee resolves a call made in a test to another function declared
// in a test file, such as a setup helper. Only exact names are used: the
// same package, or a method on the caller's receiver.
func (r resolver) testCallee(c store.RawCall) int64 {
	switch c.Form {
	case store.CallQualified:
		return r.in.TestFuncs[c.A]
	case store.CallImport:
		return r.in.TestFuncs[c.A+"."+c.B] // Java/Python modules
	}
	return 0
}

func (r resolver) exact(q string) (int64, string, string) {
	// A Python name may reach its definition through modules that import
	// and re-export it, as packages do in __init__.py: requests.get is
	// requests.api.get. Follow at most a few such hops.
	for hops := 0; hops < 4; hops++ {
		if id, ok := r.in.Symbols[q]; ok {
			return id, q, "exact"
		}
		next, ok := r.in.ReExports[q]
		if !ok || next == q {
			break
		}
		q = next
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
	case m == "std":
		// GOROOT/src declares `module std`, and its import paths are plain
		// directories: math/rand/v2 is the directory of package rand.
		dir = importPath
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
	case strings.HasPrefix(ref, "^"):
		return r.scopedType(ref[1:])
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

// callIn resolves a call site found in the file at path.
func (r resolver) callIn(c store.RawCall, path string) (int64, string, string) {
	if c.Form == store.CallExpr {
		return r.exprCall(c, path)
	}
	return r.call(c)
}

func (r resolver) call(c store.RawCall) (int64, string, string) {
	switch c.Form {
	case store.CallQualified:
		if id, q, conf := r.exact(c.A); id != 0 {
			return id, q, conf
		}
		if c.B != "" {
			// A method called without a receiver that its class does not
			// declare: inherited from a supertype, or a method of an
			// enclosing class (Java nested classes).
			if id, q := r.inheritedOrEnclosing(strings.TrimSuffix(c.A, "."+c.B), c.B); id != 0 {
				return id, q, "exact"
			}
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

	case store.CallSuper:
		return r.superCall(c.A, c.B)

	case store.CallMethod:
		return r.byMethodName(c.B)
	}
	return 0, "", ""
}
