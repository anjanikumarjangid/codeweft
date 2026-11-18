package indexer

import (
	"encoding/json"
	"strings"

	"github.com/anjanikumarjangid/codeweft/internal/store"
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
	r, types, in, err := newResolver(tx, module)
	if err != nil {
		return 0, err
	}
	edges, testEdges, resolved := r.calls(in)
	if err := tx.ReplaceEdges(edges); err != nil {
		return 0, err
	}
	if err := tx.ReplaceTestEdges(testEdges); err != nil {
		return 0, err
	}
	return resolved, resolveTypes(tx, r, types)
}

// resolveFiles resolves again only the call sites of the given files,
// after an edit that kept every file's shape: the edges from those files'
// functions were removed (store.Tx.UpdateFile), every other edge and the
// type hierarchy still hold.
func resolveFiles(tx *store.Tx, module string, files []string) error {
	r, _, in, err := newResolver(tx, module, files...)
	if err != nil {
		return err
	}
	edges, testEdges, _ := r.calls(in)
	if err := tx.AddEdges(edges); err != nil {
		return err
	}
	return tx.AddTestEdges(testEdges)
}

// newResolver loads what resolution needs: every declaration, and the call
// sites of the given files (all files when none are given).
func newResolver(tx *store.Tx, module string, files ...string) (resolver, *store.TypeInput, *store.ResolveInput, error) {
	in, err := tx.LoadResolveInput(module, files...)
	if err != nil {
		return resolver{}, nil, nil, err
	}
	types, err := tx.LoadTypeInput()
	if err != nil {
		return resolver{}, nil, nil, err
	}
	r := resolver{in: in}
	// Twice: Java supertypes may be nested types inherited from other
	// supertypes, which needs the first pass to look them up.
	r.supers = r.declaredSupers(types.Supertypes)
	r.supers = r.declaredSupers(types.Supertypes)
	r.typer = newGoTyper(types, r)
	return r, types, in, nil
}

// calls resolves the loaded call sites into edges and test edges, and
// counts the edges into the repository's own code.
func (r resolver) calls(in *store.ResolveInput) ([]store.Edge, []store.TestEdge, int) {
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
	return edges, testEdges, resolved
}

type resolver struct {
	in    *store.ResolveInput
	lang  string // of the call being resolved: go, java or python
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

// objectMethods are the methods every Java class inherits from
// java.lang.Object, with their result type references ("" for primitives
// and void).
var objectMethods = map[string]string{
	"getClass": "@java.lang.Class", "toString": "@java.lang.String", "hashCode": "", "equals": "",
	"clone": "@java.lang.Object", "notify": "", "notifyAll": "", "wait": "", "finalize": "",
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

// declared returns the declaration of q that a call with args arguments
// can call (-1: any), if q is declared.
func (r resolver) declared(q string, args int) (int64, bool) {
	for _, o := range r.in.Overloads[q] {
		if o.Fits(args) {
			return o.ID, true
		}
	}
	return 0, false
}

// inherited finds method m declared on type t or on one of its supertypes,
// searched depth first in declaration order, among the overloads that take
// args arguments (-1: any number).
func (r resolver) inherited(t, m string, args int, seen map[string]bool) (int64, string) {
	if seen[t] {
		return 0, ""
	}
	seen[t] = true
	if id, ok := r.declared(t+"."+m, args); ok {
		return id, t + "." + m
	}
	for _, s := range r.supers[t] {
		if !s.external {
			if id, q := r.inherited(s.q, m, args, seen); id != 0 {
				return id, q
			}
		}
	}
	return 0, ""
}

// method finds method m on type t like inherited, preferring an overload
// that takes args arguments: Java overloads of one name can be declared in
// different classes (putBytes(byte[]) in a superclass, putBytes(byte[],
// int, int) in the class). Only if none fits is any overload taken.
func (r resolver) method(t, m string, args int) (int64, string) {
	if id, q := r.inherited(t, m, args, map[string]bool{}); id != 0 || args < 0 {
		return id, q
	}
	return r.inherited(t, m, -1, map[string]bool{})
}

// inheritedOrEnclosing resolves a call of m without a receiver in type
// owner: owner's own or inherited method, else, for a nested class, the
// method of the nearest enclosing class that has one (also inherited),
// preferring an overload that takes args arguments.
func (r resolver) inheritedOrEnclosing(owner, m string, args int) (int64, string) {
	for _, n := range []int{args, -1} {
		for t := owner; r.isType(t); t = t[:max(strings.LastIndex(t, "."), 0)] {
			if id, q := r.inherited(t, m, n, map[string]bool{}); id != 0 {
				return id, q
			}
			if !strings.Contains(t, ".") {
				break
			}
		}
		if args < 0 {
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
	if builtinMethods[r.lang][name] {
		// The value is more likely a built-in (a dict, a List) than the
		// repository's only method of that name: no guess.
		return 0, "", ""
	}
	// Only methods of the caller's language: a Go call on a client of
	// unknown type is not a call of a Python method of the same name.
	var ids []int64
	for _, id := range r.in.Methods[name] {
		if r.in.Langs[id] == r.lang {
			ids = append(ids, id)
		}
	}
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

// builtinMethods are method names of each language's built-in and
// standard-library types (Python dict, list, str, set, files; Java Object,
// String, collections, streams, exceptions). A call of one of them on a
// value of unknown type is not matched by name. Go's built-in types have
// no methods.
var builtinMethods = map[string]map[string]bool{
	"python": wordSet(`append extend insert pop remove clear copy count index reverse sort update get items keys
		values setdefault popitem fromkeys add discard union intersection difference issubset issuperset
		split rsplit join strip lstrip rstrip replace format lower upper startswith endswith encode decode find
		rfind partition rpartition splitlines title capitalize zfill read readline readlines write writelines close
		flush seek tell fileno isoformat strftime total_seconds group groups match search sub`),
	"java": wordSet(`get put set add addAll remove removeAll contains containsKey containsValue size isEmpty clear
		iterator next hasNext stream forEach map filter collect of equals hashCode toString compareTo length charAt
		substring indexOf trim split format getMessage getCause close read write flush append keySet values entrySet
		getKey getValue toArray sort valueOf intValue longValue booleanValue name ordinal clone getClass apply accept
		test run call join start orElse isPresent`),
}

func wordSet(s string) map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(s) {
		m[w] = true
	}
	return m
}

// langOf names the language of a source file by its extension.
func langOf(path string) string {
	switch {
	case strings.HasSuffix(path, ".java"):
		return "java"
	case strings.HasSuffix(path, ".py"):
		return "python"
	}
	return "go"
}

// callIn resolves a call site found in the file at path.
func (r resolver) callIn(c store.RawCall, path string) (int64, string, string) {
	r.lang = langOf(path)
	if c.Form == store.CallExpr {
		if r.lang != "go" { // Java and Python: the same expression form
			return r.javaExprCall(c)
		}
		return r.exprCall(c, path)
	}
	return r.call(c)
}

func (r resolver) call(c store.RawCall) (int64, string, string) {
	switch c.Form {
	case store.CallQualified:
		if c.B != "" && c.Args >= 0 && r.isType(strings.TrimSuffix(c.A, "."+c.B)) {
			// A method called without a receiver in a class: its own,
			// inherited from a supertype, or an enclosing class's (Java
			// nested classes), by arity first.
			if id, q := r.inheritedOrEnclosing(strings.TrimSuffix(c.A, "."+c.B), c.B, c.Args); id != 0 {
				return id, q, "exact"
			}
		}
		if id, q, conf := r.exact(c.A); id != 0 {
			return id, q, conf
		}
		if c.B != "" {
			if id, q := r.inheritedOrEnclosing(strings.TrimSuffix(c.A, "."+c.B), c.B, -1); id != 0 {
				return id, q, "exact"
			}
			if _, ok := objectMethods[c.B]; ok && r.isType(strings.TrimSuffix(c.A, "."+c.B)) {
				return 0, "java.lang.Object." + c.B, "external" // getClass() in any class
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
		if external && c.C == "anonymous" {
			// new Runnable() { ... }: whether an outside type is an
			// interface (nothing called) or a class is not known.
			return 0, "", ""
		}
		if external {
			return 0, t + "." + c.B, "external"
		}
		if id, q, conf := r.exact(t + "." + c.B); id != 0 {
			return id, q, conf
		}
		// `new Order(...)` on a type without an explicit constructor. An
		// interface has none to call: new Runnable() { ... } creates an
		// anonymous class, which is not a call into the interface.
		if c.B == t[strings.LastIndex(t, ".")+1:] {
			if id, ok := r.in.Symbols[t]; ok && (r.in.Kinds[id] == "interface" || r.in.Kinds[id] == "annotation") {
				return 0, "", ""
			}
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

// javaType works out the type of a Java expression encoded by the parser
// (java.go javaExpr): the qualified type, whether it is declared outside
// the repository, and whether it could be found at all.
func (r resolver) javaType(node any, depth int) (q string, external, known bool) {
	n, ok := node.([]any)
	if !ok || len(n) < 2 || depth > 12 {
		return "", false, false
	}
	str := func(i int) string {
		if i < len(n) {
			s, _ := n[i].(string)
			return s
		}
		return ""
	}
	fromRef := func(ref string) (string, bool, bool) {
		q, ext := r.typeRef(ref)
		return q, ext, q != ""
	}
	switch str(0) {
	case "t":
		return fromRef(str(1))
	case "f":
		owner, ext, ok := r.javaType(n[1], depth+1)
		if ext {
			return "", true, true // a field of an outside type: an outside value
		}
		if !ok {
			return "", false, false
		}
		if ref := r.javaField(owner, str(2), map[string]bool{}); ref != "" {
			return fromRef(ref)
		}
	case "c":
		owner, ext, ok := r.javaType(n[1], depth+1)
		if ext {
			return "", true, true // a method of an outside type: an outside value
		}
		if !ok {
			return "", false, false
		}
		if id, _ := r.method(owner, str(2), -1); id != 0 {
			return fromRef(r.in.Results[id])
		}
	case "F": // a module-level function (Python): its declared result
		if id, _, _ := r.exact(str(1)); id != 0 {
			return fromRef(r.in.Results[id])
		}
	case "C":
		if id, _ := r.inheritedOrEnclosing(str(1), str(2), -1); id != 0 {
			return fromRef(r.in.Results[id])
		}
		if ref, ok := objectMethods[str(2)]; ok && ref != "" { // getClass(), toString()
			return fromRef(ref)
		}
	}
	return "", false, false
}

// javaField returns the type reference of field name of type t, declared
// on t or inherited from a supertype.
func (r resolver) javaField(t, name string, seen map[string]bool) string {
	if seen[t] {
		return ""
	}
	seen[t] = true
	if ref, ok := r.in.Fields[[2]string{t, name}]; ok {
		return ref
	}
	for _, s := range r.supers[t] {
		if !s.external {
			if ref := r.javaField(s.q, name, seen); ref != "" {
				return ref
			}
		}
	}
	return ""
}

// javaExprCall resolves a Java method call on a receiver the parser
// encoded. A receiver of a type outside the repository gives an external
// reference; one of unknown type falls back to the unique method name.
func (r resolver) javaExprCall(c store.RawCall) (int64, string, string) {
	var node any
	if json.Unmarshal([]byte(c.A), &node) != nil {
		return r.byMethodName(c.B)
	}
	t, external, known := r.javaType(node, 0)
	switch {
	case !known:
		return r.byMethodName(c.B)
	case external && t == "":
		// A value that came from a type outside the repository (the result
		// of list.get(0), map.values()): its type is outside too, though
		// not known exactly. No guess by method name.
		return 0, "", ""
	case external:
		return 0, t + "." + c.B, "external"
	}
	if id, q := r.method(t, c.B, c.Args); id != 0 {
		return id, q, "exact"
	}
	// Declared on a supertype outside the repository, or on Object.
	for _, s := range r.supers[t] {
		if s.external {
			return 0, s.q + "." + c.B, "external"
		}
	}
	if _, ok := objectMethods[c.B]; ok {
		return 0, "java.lang.Object." + c.B, "external"
	}
	return r.byMethodName(c.B)
}
