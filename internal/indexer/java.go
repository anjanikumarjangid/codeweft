package indexer

import (
	"encoding/json"
	"path"
	"strconv"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/java"

	"github.com/anjanikumarjangid/codeweft/internal/store"
)

// Type references emitted by the Java and Python parsers:
//
//	"@a.b.Type"  fully qualified (from an import), resolved if it is indexed
//	"~a.b.Type"  guess: same package/module; falls back to the short name
//	"?Type"      short name only; resolved if exactly one type has it

var javaPrimitives = map[string]bool{
	"void": true, "boolean": true, "byte": true, "char": true, "short": true,
	"int": true, "long": true, "float": true, "double": true, "var": true,
}

func isJavaTest(rel string) bool {
	return strings.HasSuffix(rel, "Test.java") || strings.HasSuffix(rel, "Tests.java") ||
		strings.HasSuffix(rel, "IT.java") || strings.Contains("/"+rel, "/src/test/")
}

type javaFile struct {
	src     []byte
	rel     string
	pkg     string
	imports map[string]string // simple name -> fully qualified name
	statics map[string]string // statically imported member -> owning class
	res     *FileResult
	// class is the innermost class being parsed: simple type names are
	// looked up from it (nested types, enclosing classes, inherited
	// member types, the package).
	class string
	// typeVars maps the type variables in scope to their bound's type
	// reference ("" when unbounded): class and method type parameters.
	typeVars map[string]string
	// consts holds the file's static final String constants with a
	// written-out value, by simple name, for evaluating URLs and names.
	consts map[string]string
	// stubs maps the fields of the class being parsed that hold a Temporal
	// activity stub or worker to what they hold (see temporalStub).
	stubs map[string]string
}

// withTypeParams adds the type parameters declared by n (a class or a
// method) to the scope while fn runs.
func (jf *javaFile) withTypeParams(n *sitter.Node, fn func()) {
	tps := field(n, "type_parameters")
	if tps == nil {
		fn()
		return
	}
	saved := jf.typeVars
	vars := make(map[string]string, len(saved))
	for k, v := range saved {
		vars[k] = v
	}
	jf.typeVars = vars
	for _, tp := range namedChildren(tps) {
		var name, bound string
		for _, c := range namedChildren(tp) {
			switch c.Type() {
			case "type_identifier", "identifier":
				if name == "" {
					name = text(c, jf.src)
				}
			case "type_bound":
				if b := namedChildren(c); len(b) > 0 {
					vars[name] = "" // the bound may mention the variable itself
					bound = jf.typeRef(b[0])
				}
			}
		}
		if name != "" {
			vars[name] = bound
		}
	}
	fn()
	jf.typeVars = saved
}

// ParseJava extracts classes, interfaces, enums, records, their methods,
// constructors and fields, and the call sites inside method bodies.
func ParseJava(rel string, src []byte) (*FileResult, error) {
	root, done, err := parseTree(java.GetLanguage(), src)
	if err != nil {
		return nil, err
	}
	defer done()

	jf := &javaFile{src: src, rel: rel, imports: map[string]string{}, statics: map[string]string{}, consts: map[string]string{}}
	dir := path.Dir(rel)
	jf.res = &FileResult{File: store.File{Path: rel, Dir: dir, Lang: "java"}}

	for _, n := range namedChildren(root) {
		switch n.Type() {
		case "package_declaration":
			for _, c := range namedChildren(n) {
				if c.Type() == "scoped_identifier" || c.Type() == "identifier" {
					jf.pkg = text(c, src)
				}
			}
		case "import_declaration":
			full, static, wildcard := "", false, false
			for _, c := range namedChildren(n) {
				switch c.Type() {
				case "scoped_identifier", "identifier":
					full = text(c, src)
				case "asterisk":
					wildcard = true
				}
			}
			static = strings.Contains(text(n, src), "import static")
			if full == "" {
				continue
			}
			alias := full[strings.LastIndex(full, ".")+1:]
			switch {
			case wildcard:
				alias = "*"
			case static:
				jf.statics[alias] = full[:strings.LastIndex(full, ".")]
			default:
				jf.imports[alias] = full
			}
			jf.res.Imports = append(jf.res.Imports, store.Import{Path: full, Alias: alias})
		}
	}
	if jf.pkg == "" {
		jf.pkg = strings.ReplaceAll(dir, "/", ".")
	}
	jf.res.File.Package = jf.pkg
	jf.res.File.Qual = jf.pkg

	for _, n := range namedChildren(root) {
		if isJavaType(n.Type()) {
			jf.typeDecl(n, jf.pkg, false)
			if jf.res.File.Doc == "" {
				jf.res.File.Doc = javadoc(n, src)
			}
		}
	}
	return jf.res, nil
}

func isJavaType(t string) bool {
	switch t {
	case "class_declaration", "interface_declaration", "enum_declaration", "record_declaration", "annotation_type_declaration":
		return true
	}
	return false
}

// javadoc returns the /** */ comment directly before a declaration.
func javadoc(n *sitter.Node, src []byte) string {
	prev := n.PrevNamedSibling()
	if prev != nil && prev.Type() == "block_comment" {
		c := text(prev, src)
		if strings.HasPrefix(c, "/**") && line(n)-endLine(prev) <= 1 {
			return cleanDocComment(c)
		}
	}
	return ""
}

func modifiersOf(n *sitter.Node) *sitter.Node {
	for _, c := range namedChildren(n) {
		if c.Type() == "modifiers" {
			return c
		}
	}
	return nil
}

func hasModifier(n *sitter.Node, src []byte, mod string) bool {
	for _, c := range namedChildren(n) {
		if c.Type() == "modifiers" {
			for _, w := range strings.Fields(text(c, src)) {
				if w == mod {
					return true
				}
			}
		}
	}
	return false
}

// typeRef converts a type node into a type reference.
func (jf *javaFile) typeRef(n *sitter.Node) string {
	if n == nil {
		return ""
	}
	switch n.Type() {
	case "generic_type":
		for _, c := range namedChildren(n) {
			if c.Type() == "type_identifier" || c.Type() == "scoped_type_identifier" {
				return jf.typeRef(c)
			}
		}
		return ""
	case "scoped_type_identifier":
		// Outer.Inner is a nested type, found like Outer itself; anything
		// else (java.util.List) is a full name.
		t := text(n, jf.src)
		first, _, _ := strings.Cut(t, ".")
		if _, ok := jf.imports[first]; ok || isCapitalized(first) {
			return jf.nameRef(t)
		}
		return "@" + t
	case "type_identifier", "identifier":
		return jf.nameRef(text(n, jf.src))
	}
	return ""
}

func (jf *javaFile) nameRef(name string) string {
	if name == "" || javaPrimitives[name] {
		return ""
	}
	first, rest, _ := strings.Cut(name, ".")
	if bound, ok := jf.typeVars[first]; ok { // a type variable: its bound
		if rest != "" {
			return ""
		}
		return bound
	}
	if full, ok := jf.imports[first]; ok {
		if rest != "" {
			return "@" + full + "." + rest
		}
		return "@" + full
	}
	if jf.class != "" {
		return "^" + jf.class + "|" + name
	}
	return "~" + jf.pkg + "." + name
}

func (jf *javaFile) add(n *sitter.Node, name, qualified, kind, sig, doc string, exported bool) {
	jf.res.Symbols = append(jf.res.Symbols, store.Symbol{
		File: jf.rel, Lang: "java", Package: jf.pkg, Name: name, Qualified: qualified, Kind: kind,
		Signature: sig, Doc: doc, StartLine: line(n), EndLine: endLine(n), Exported: exported,
		Body: text(n, jf.src),
	})
}

// supertypes records what a class, interface, enum or record declares it
// extends or implements. An interface's "extends" list is recorded as
// extends; a class's "implements" list as implements.
func (jf *javaFile) supertypes(n *sitter.Node, qual string) {
	add := func(list *sitter.Node, kind string) {
		if list == nil {
			return
		}
		for _, c := range namedChildren(list) {
			if c.Type() == "type_list" {
				for _, t := range namedChildren(c) {
					if ref := jf.typeRef(t); ref != "" {
						jf.res.Supertypes = append(jf.res.Supertypes, store.Supertype{Owner: qual, Kind: kind, TypeRef: ref})
					}
				}
				continue
			}
			if ref := jf.typeRef(c); ref != "" { // superclass: a single type
				jf.res.Supertypes = append(jf.res.Supertypes, store.Supertype{Owner: qual, Kind: kind, TypeRef: ref})
			}
		}
	}
	add(field(n, "superclass"), store.SuperExtends)
	add(field(n, "interfaces"), store.SuperImplements)
	for _, c := range namedChildren(n) {
		if c.Type() == "extends_interfaces" {
			add(c, store.SuperExtends)
		}
	}
}

func (jf *javaFile) typeDecl(n *sitter.Node, prefix string, inInterface bool) {
	name := text(field(n, "name"), jf.src)
	if name == "" {
		return
	}
	qual := prefix + "." + name
	savedClass := jf.class
	jf.class = qual
	defer func() { jf.class = savedClass }()
	jf.withTypeParams(n, func() { jf.typeBody(n, name, qual, inInterface) })
}

// typeBody parses a type declaration with its scope set up.
func (jf *javaFile) typeBody(n *sitter.Node, name, qual string, inInterface bool) {
	kind := strings.TrimSuffix(strings.TrimSuffix(n.Type(), "_declaration"), "_type")
	if n.Type() == "annotation_type_declaration" {
		kind = "annotation"
	}
	body := field(n, "body")
	jf.add(n, name, qual, kind, header(n, body, jf.src), javadoc(n, jf.src),
		hasModifier(n, jf.src, "public") || inInterface)
	isInterface := n.Type() == "interface_declaration"
	jf.supertypes(n, qual)
	classPrefix := ""
	if _, p, _, ok := javaRoute(modifiersOf(n), jf.src); ok {
		classPrefix = p
	}
	// An @FeignClient interface declares the HTTP calls this service makes:
	// its mapped methods are uses, not routes.
	feignPrefix, feign := jf.feignClient(modifiersOf(n))
	if feign {
		classPrefix = joinRoute(feignPrefix, classPrefix)
	}

	temporalKind, temporalPrefix := jf.temporalInterface(n)
	if svc := jf.grpcService(n); svc != "" {
		// Its public methods, and those of the classes extending it.
		jf.res.Contracts = append(jf.res.Contracts, store.Contract{Kind: store.ContractGRPC, Role: store.RoleProvides,
			Name: svc + "/", Symbol: store.ClassMethods + qual, Line: line(n), Exact: true})
	}
	savedStubs := jf.stubs
	jf.stubs = map[string]string{}
	defer func() { jf.stubs = savedStubs }()

	jf.res.Contracts = append(jf.res.Contracts, jf.kafkaListener(n, qual)...)
	jf.res.Contracts = append(jf.res.Contracts, jf.brokerListeners(n, qual)...)
	fields := map[string]string{}
	// Record components are fields with generated accessor methods.
	if n.Type() == "record_declaration" {
		for _, p := range namedChildren(field(n, "parameters")) {
			pn, pt := text(field(p, "name"), jf.src), field(p, "type")
			if pn == "" {
				continue
			}
			if ref := jf.typeRef(pt); ref != "" {
				fields[pn] = ref
				jf.res.Fields = append(jf.res.Fields, store.Field{Owner: qual, Name: pn, TypeRef: ref})
			}
			jf.add(p, pn, qual+"."+pn, "method", text(pt, jf.src)+" "+pn+"()", "Accessor for the record component "+pn+".", true)
			jf.res.Symbols[len(jf.res.Symbols)-1].Result = jf.typeRef(pt)
			jf.res.Symbols[len(jf.res.Symbols)-1].Arity = "0"
		}
	}
	members := namedChildren(body)
	for _, m := range members {
		if m.Type() == "enum_body_declarations" {
			members = append(members, namedChildren(m)...)
		}
	}
	for _, m := range members {
		if m.Type() == "field_declaration" || m.Type() == "constant_declaration" {
			jf.stringConsts(m)
			if ph := jf.valuePlaceholder(m); ph != "" {
				for _, d := range namedChildren(m) {
					if d.Type() == "variable_declarator" {
						jf.consts[text(field(d, "name"), jf.src)] = ph
					}
				}
			}
			ref := jf.typeRef(field(m, "type"))
			for _, d := range namedChildren(m) {
				if d.Type() == "variable_declarator" {
					fn := text(field(d, "name"), jf.src)
					fields[fn] = ref
					if st := jf.temporalStub(field(d, "value")); st != "" {
						jf.stubs[fn] = st
					} else if st := jf.grpcStub(field(m, "type"), field(d, "value")); st != "" {
						jf.stubs[fn] = st
					}
					if ref != "" {
						jf.res.Fields = append(jf.res.Fields, store.Field{Owner: qual, Name: fn, TypeRef: ref})
					}
				}
			}
		}
	}
	for _, m := range members {
		switch {
		case isJavaType(m.Type()):
			jf.typeDecl(m, qual, isInterface)
		case m.Type() == "method_declaration" || m.Type() == "constructor_declaration" || m.Type() == "compact_constructor_declaration":
			mname := text(field(m, "name"), jf.src)
			kind := "method"
			if m.Type() != "method_declaration" {
				kind, mname = "constructor", name
			}
			mbody := field(m, "body")
			if isInterface && mbody == nil {
				kind = "interface method"
			}
			mq := qual + "." + mname
			jf.add(m, mname, mq, kind, header(m, mbody, jf.src), javadoc(m, jf.src),
				isInterface || hasModifier(m, jf.src, "public"))
			sym := &jf.res.Symbols[len(jf.res.Symbols)-1]
			sym.Arity = arity(field(m, "parameters"))
			jf.res.Contracts = append(jf.res.Contracts, jf.kafkaListener(m, mq)...)
			jf.res.Contracts = append(jf.res.Contracts, jf.brokerListeners(m, mq)...)
			if temporalKind != "" && kind != "constructor" {
				jf.res.Contracts = append(jf.res.Contracts, jf.temporalMethod(m, temporalKind, temporalPrefix, name, mname, mq)...)
			}
			jf.withTypeParams(m, func() {
				if t := field(m, "type"); t != nil {
					sym.Result = jf.typeRef(t) // the declared return type
				}
			})
			if mbody != nil {
				jf.withTypeParams(m, func() {
					jf.calls(m, mbody, mq, qual, fields)
					if kind == "constructor" && field(n, "superclass") != nil && !explicitConstructorCall(mbody) {
						// A constructor without this(...) or super(...) calls
						// the superclass constructor implicitly.
						jf.res.Calls = append(jf.res.Calls, store.RawCall{Caller: mq, Form: store.CallSuper, A: qual, Line: line(m)})
					}
				})
			}
			if method, p, fw, ok := javaRoute(modifiersOf(m), jf.src); ok && method != "" {
				route := joinRoute(classPrefix, p)
				if route == "" {
					route = "/"
				}
				if feign {
					if method == "ANY" {
						method = ""
					}
					if c, ok := httpUse(method, route, mq, line(m)); ok {
						jf.res.Contracts = append(jf.res.Contracts, c)
					}
					continue
				}
				jf.res.Endpoints = append(jf.res.Endpoints, store.Endpoint{
					Method: method, Path: route, Handler: mq, Framework: fw, File: jf.rel, Line: line(m),
				})
			}
		}
	}
}

// calls records method invocations and constructor calls in a body.
func (jf *javaFile) calls(decl, body *sitter.Node, caller, owner string, fields map[string]string) {
	locals := map[string]string{}
	stubs := map[string]string{} // Temporal stubs and workers: the class's fields, then locals
	for k, v := range jf.stubs {
		stubs[k] = v
	}
	// param records a parameter-like declaration: a method or lambda
	// parameter, a varargs parameter (an array: its methods are Object's),
	// a catch parameter (of the first type of a multi-catch), or a for-each
	// variable.
	param := func(n *sitter.Node) {
		var typ, name *sitter.Node
		switch n.Type() {
		case "formal_parameter", "enhanced_for_statement":
			typ, name = field(n, "type"), field(n, "name")
		case "spread_parameter":
			for _, c := range namedChildren(n) {
				if c.Type() == "variable_declarator" {
					name = field(c, "name")
				}
			}
			if name != nil {
				locals[text(name, jf.src)] = "@java.lang.Object"
			}
			return
		case "catch_formal_parameter":
			for _, c := range namedChildren(n) {
				switch c.Type() {
				case "catch_type":
					if ts := namedChildren(c); len(ts) > 0 {
						typ = ts[0]
					}
				case "identifier":
					name = c
				}
			}
		}
		if typ == nil || name == nil {
			return
		}
		if ref := jf.typeRef(typ); ref != "" {
			locals[text(name, jf.src)] = ref
		}
		if st := jf.grpcStub(typ, nil); st != "" {
			stubs[text(name, jf.src)] = st
		}
	}
	for _, p := range namedChildren(field(decl, "parameters")) {
		param(p)
	}
	walk(body, func(n *sitter.Node) bool {
		switch n.Type() {
		case "formal_parameter", "spread_parameter", "catch_formal_parameter", "enhanced_for_statement":
			param(n)
		case "local_variable_declaration":
			ref := jf.typeRef(field(n, "type"))
			for _, d := range namedChildren(n) {
				if d.Type() != "variable_declarator" {
					continue
				}
				r := ref
				if v := field(d, "value"); r == "" && v != nil && v.Type() == "object_creation_expression" {
					r = jf.typeRef(field(v, "type")) // var x = new T()
				}
				if r != "" {
					locals[text(field(d, "name"), jf.src)] = r
				}
				if st := jf.temporalStub(field(d, "value")); st != "" {
					stubs[text(field(d, "name"), jf.src)] = st
				} else if st := jf.grpcStub(field(n, "type"), field(d, "value")); st != "" {
					stubs[text(field(d, "name"), jf.src)] = st
				}
			}
		case "lambda_expression", "class_body":
			// Calls inside lambdas belong to the enclosing method; anonymous
			// class bodies are skipped.
			if n.Type() == "lambda_expression" {
				jf.lambdaParam(n, locals, fields)
			}
			return n.Type() == "lambda_expression"
		case "object_creation_expression":
			t := field(n, "type")
			if ref := jf.typeRef(t); ref != "" {
				short := ref[strings.LastIndexAny(ref, ".@~?^|")+1:]
				rc := store.RawCall{Caller: caller, Form: store.CallTyped, A: ref, B: short, Line: line(n), Args: argCount(n)}
				if hasChild(n, "class_body") {
					rc.C = "anonymous" // new T() { ... }: runs T's constructor only if T is a class
				}
				jf.res.Calls = append(jf.res.Calls, rc)
			}
		case "explicit_constructor_invocation":
			// super(...) calls the superclass constructor; this(...) another
			// constructor of the same class.
			rc := store.RawCall{Caller: caller, Line: line(n), Args: argCount(n)}
			switch c := field(n, "constructor"); {
			case c == nil:
				return true
			case c.Type() == "super":
				rc.Form, rc.A = store.CallSuper, owner
			case c.Type() == "this":
				short := owner[strings.LastIndex(owner, ".")+1:]
				rc.Form, rc.A, rc.B = store.CallQualified, owner+"."+short, short
			default:
				return true
			}
			jf.res.Calls = append(jf.res.Calls, rc)
		case "method_invocation":
			name := text(field(n, "name"), jf.src)
			if c, ok := jf.httpClientCall(n, locals, fields, caller); ok {
				jf.res.Contracts = append(jf.res.Contracts, c)
			}
			jf.res.Contracts = append(jf.res.Contracts, jf.temporalCall(n, stubs, locals, fields, caller)...)
			if c, ok := jf.grpcCall(n, stubs, caller); ok {
				jf.res.Contracts = append(jf.res.Contracts, c)
			}
			jf.res.Contracts = append(jf.res.Contracts, jf.kafkaCall(n, locals, fields, caller)...)
			jf.res.Contracts = append(jf.res.Contracts, jf.messagingCall(n, locals, fields, caller)...)
			rc := store.RawCall{Caller: caller, Line: line(n), Args: argCount(n)}
			obj := field(n, "object")
			switch {
			case obj == nil && jf.statics[name] != "": // import static a.b.C.name
				rc.Form, rc.A, rc.B = store.CallImport, jf.statics[name], name
			case obj == nil || obj.Type() == "this":
				rc.Form, rc.A, rc.B = store.CallQualified, owner+"."+name, name
			case obj.Type() == "super":
				rc.Form, rc.A, rc.B = store.CallSuper, owner, name
			case obj.Type() == "identifier" && hasChild(n, "super"):
				// Interface.super.m(): the interface's default method.
				rc.Form, rc.A, rc.B = store.CallTyped, jf.nameRef(text(obj, jf.src)), name
			case obj.Type() == "identifier":
				x := text(obj, jf.src)
				switch {
				case locals[x] != "":
					rc.Form, rc.A, rc.B = store.CallTyped, locals[x], name
				case fields[x] != "":
					rc.Form, rc.A, rc.B, rc.C = store.CallField, owner, x, name
				case isCapitalized(x): // static call: Type.method()
					rc.Form, rc.A, rc.B = store.CallTyped, jf.nameRef(x), name
				default: // a field inherited from a supertype
					jf.exprCall(&rc, obj, owner, locals, fields, name)
				}
			case obj.Type() == "field_access" && field(obj, "object") != nil && field(obj, "object").Type() == "this":
				rc.Form, rc.A, rc.B, rc.C = store.CallField, owner, text(field(obj, "field"), jf.src), name
			default:
				jf.exprCall(&rc, obj, owner, locals, fields, name)
			}
			if rc.Form == store.CallTyped && rc.A == "" {
				return true
			}
			jf.res.Calls = append(jf.res.Calls, rc)
		}
		return true
	})
}

// hasChild reports whether n has a direct child of type typ, named or not.
func hasChild(n *sitter.Node, typ string) bool {
	for i := 0; i < int(n.ChildCount()); i++ {
		if n.Child(i).Type() == typ {
			return true
		}
	}
	return false
}

// explicitConstructorCall reports whether a constructor body starts with
// this(...) or super(...).
func explicitConstructorCall(body *sitter.Node) bool {
	for _, c := range namedChildren(body) {
		if c.Type() == "line_comment" || c.Type() == "block_comment" {
			continue
		}
		return c.Type() == "explicit_constructor_invocation"
	}
	return false
}

// exprCall makes rc a call on a receiver whose type the resolver works out
// (store.CallExpr), or, when the receiver cannot be described, a call
// matched by method name.
func (jf *javaFile) exprCall(rc *store.RawCall, obj *sitter.Node, owner string, locals, fields map[string]string, name string) {
	if e := jf.javaExpr(obj, owner, locals, fields, 0); e != nil {
		if b, err := json.Marshal(e); err == nil {
			rc.Form, rc.A, rc.B = store.CallExpr, string(b), name
			return
		}
	}
	rc.Form, rc.B = store.CallMethod, name
}

// javaExpr encodes an expression whose type the resolver can work out
// from declarations, as a JSON-able tree:
//
//	["t", REF]           a type reference
//	["f", EXPR, NAME]    the field NAME of EXPR (also inherited)
//	["c", EXPR, NAME]    the result of calling method NAME on EXPR
//	["C", CLASS, NAME]   the result of calling NAME without a receiver in
//	                     CLASS (its own, inherited or an enclosing class's)
//
// It returns nil for anything else (an array element, a lambda's value).
func (jf *javaFile) javaExpr(n *sitter.Node, owner string, locals, fields map[string]string, depth int) any {
	if n == nil || depth > 10 {
		return nil
	}
	this := []any{"t", "@" + owner}
	switch n.Type() {
	case "parenthesized_expression":
		if kids := namedChildren(n); len(kids) == 1 {
			return jf.javaExpr(kids[0], owner, locals, fields, depth+1)
		}
	case "this":
		return this
	case "identifier":
		x := text(n, jf.src)
		switch {
		case locals[x] != "":
			return []any{"t", locals[x]}
		case fields[x] != "":
			return []any{"t", fields[x]}
		case isCapitalized(x):
			return nil // a class: static calls are typed calls
		}
		return []any{"f", this, x} // a field inherited from a supertype
	case "object_creation_expression", "cast_expression":
		if ref := jf.typeRef(field(n, "type")); ref != "" {
			return []any{"t", ref}
		}
	case "field_access":
		obj, f := field(n, "object"), text(field(n, "field"), jf.src)
		if obj != nil && obj.Type() == "this" {
			return []any{"f", this, f}
		}
		if x := jf.javaExpr(obj, owner, locals, fields, depth+1); x != nil {
			return []any{"f", x, f}
		}
	case "method_invocation":
		obj, m := field(n, "object"), text(field(n, "name"), jf.src)
		switch {
		case obj == nil || obj.Type() == "this":
			return []any{"C", owner, m}
		case obj.Type() == "super":
			return nil
		case obj.Type() == "identifier" && locals[text(obj, jf.src)] == "" && fields[text(obj, jf.src)] == "" &&
			isCapitalized(text(obj, jf.src)):
			if ref := jf.nameRef(text(obj, jf.src)); ref != "" { // Type.staticMethod()
				return []any{"c", []any{"t", ref}, m}
			}
		default:
			if x := jf.javaExpr(obj, owner, locals, fields, depth+1); x != nil {
				return []any{"c", x, m}
			}
		}
	}
	return nil
}

// argCount is the number of arguments of a call or object creation.
func argCount(n *sitter.Node) int {
	args := field(n, "arguments")
	if args == nil {
		return -1
	}
	count := 0
	for _, c := range namedChildren(args) {
		if c.Type() != "line_comment" && c.Type() != "block_comment" {
			count++
		}
	}
	return count
}

// arity describes a parameter list: "2", or "1+" when the last parameter
// is varargs (it accepts one or more parameters before it, then any number).
func arity(params *sitter.Node) string {
	n, varargs := 0, false
	for _, p := range namedChildren(params) {
		switch p.Type() {
		case "formal_parameter":
			n++
		case "spread_parameter":
			varargs = true
		}
	}
	if varargs {
		return strconv.Itoa(n) + "+"
	}
	return strconv.Itoa(n)
}

// stringConsts records a field declaration's String constants whose value
// is written out: static final String BASE = "/api/orders".
func (jf *javaFile) stringConsts(m *sitter.Node) {
	if t := field(m, "type"); t == nil || text(t, jf.src) != "String" {
		return
	}
	if m.Type() == "field_declaration" && (!hasModifier(m, jf.src, "static") || !hasModifier(m, jf.src, "final")) {
		return
	}
	for _, d := range namedChildren(m) {
		if d.Type() == "variable_declarator" {
			if v := field(d, "value"); v != nil {
				if s := jf.template(v); !strings.Contains(s, "*") {
					jf.consts[text(field(d, "name"), jf.src)] = s
				}
			}
		}
	}
}

// template evaluates a Java string expression as far as the source
// allows, writing "*" for each part it cannot see: literals, +, the file's
// String constants, and String.format (each verb a "*").
func (jf *javaFile) template(n *sitter.Node) string {
	if n == nil {
		return "*"
	}
	switch n.Type() {
	case "string_literal":
		return strings.Trim(text(n, jf.src), `"`)
	case "parenthesized_expression":
		if kids := namedChildren(n); len(kids) == 1 {
			return jf.template(kids[0])
		}
	case "binary_expression":
		if op := field(n, "operator"); op != nil && text(op, jf.src) == "+" {
			return jf.template(field(n, "left")) + jf.template(field(n, "right"))
		}
	case "identifier":
		if v, ok := jf.consts[text(n, jf.src)]; ok {
			return v
		}
	case "method_invocation":
		obj, name := field(n, "object"), text(field(n, "name"), jf.src)
		args := namedChildren(field(n, "arguments"))
		switch {
		case obj != nil && text(obj, jf.src) == "String" && name == "format" && len(args) > 0:
			return fmtVerb.ReplaceAllString(jf.template(args[0]), "*")
		case obj != nil && text(obj, jf.src) == "System" && name == "getenv" && len(args) == 1:
			// An environment variable: resolved from configuration files.
			if k := jf.template(args[0]); !strings.ContainsAny(k, "*$") {
				return "${" + k + "}"
			}
		}
	}
	return "*"
}

// feignClient reports whether a type is an @FeignClient interface, and the
// path prefix its annotation declares (path = "/api").
func (jf *javaFile) feignClient(mods *sitter.Node) (string, bool) {
	for _, a := range namedChildren(mods) {
		if a.Type() != "annotation" && a.Type() != "marker_annotation" {
			continue
		}
		name := text(field(a, "name"), jf.src)
		if name[strings.LastIndex(name, ".")+1:] != "FeignClient" {
			continue
		}
		prefix := ""
		for _, p := range namedChildren(field(a, "arguments")) {
			if p.Type() == "element_value_pair" && text(field(p, "key"), jf.src) == "path" {
				prefix = jf.template(field(p, "value"))
			}
		}
		return prefix, true
	}
	return "", false
}

// restTemplateMethods maps RestTemplate methods to the HTTP method they send.
var restTemplateMethods = map[string]string{
	"getForObject": "GET", "getForEntity": "GET", "postForObject": "POST", "postForEntity": "POST",
	"postForLocation": "POST", "put": "PUT", "delete": "DELETE", "patchForObject": "PATCH",
	"exchange": "", "execute": "",
}

// httpClientCall recognizes an HTTP request made with Spring's
// RestTemplate (restTemplate.getForObject(url, ...), exchange(url,
// HttpMethod.POST, ...)) or a WebClient/RestClient chain
// (client.get().uri("/orders/{id}", id)).
func (jf *javaFile) httpClientCall(n *sitter.Node, locals, fields map[string]string, caller string) (store.Contract, bool) {
	name := text(field(n, "name"), jf.src)
	obj := field(n, "object")
	args := namedChildren(field(n, "arguments"))
	if obj == nil || len(args) == 0 {
		return store.Contract{}, false
	}
	httpMethodArg := func(a *sitter.Node) string {
		t := text(a, jf.src)
		return strings.ToUpper(t[strings.LastIndex(t, ".")+1:])
	}
	if method, ok := restTemplateMethods[name]; ok && obj.Type() == "identifier" {
		ref := locals[text(obj, jf.src)]
		if ref == "" {
			ref = fields[text(obj, jf.src)]
		}
		if !strings.HasSuffix(ref, "RestTemplate") {
			return store.Contract{}, false
		}
		if (name == "exchange" || name == "execute") && len(args) > 1 {
			method = httpMethodArg(args[1])
		}
		return httpUse(method, jf.template(args[0]), caller, line(n))
	}
	if name == "uri" && obj.Type() == "method_invocation" {
		verb := text(field(obj, "name"), jf.src)
		switch verb {
		case "get", "post", "put", "patch", "delete", "head", "options":
			return httpUse(strings.ToUpper(verb), jf.template(args[0]), caller, line(n))
		case "method":
			if ma := namedChildren(field(obj, "arguments")); len(ma) > 0 {
				return httpUse(httpMethodArg(ma[0]), jf.template(args[0]), caller, line(n))
			}
		}
	}
	return store.Contract{}, false
}
