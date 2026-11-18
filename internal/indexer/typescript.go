package indexer

import (
	"path"
	"regexp"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/typescript/tsx"
	"github.com/smacker/go-tree-sitter/typescript/typescript"

	"github.com/anjanikumarjangid/codeweft/internal/store"
)

// TypeScript and JavaScript.
//
// A file is a module named by its path without extension ("src/orders/service";
// an index file is named by its directory), and symbols are qualified by it:
// src/orders/service.OrderService.place. Imports (ES modules and require)
// map local names to the modules they come from, relative paths resolved to
// those module names, packages kept as written ("express"), so calls
// resolve through the same rules as Java and Python. .ts files are parsed
// with the TypeScript grammar; .tsx and JavaScript (which may hold JSX)
// with the TSX grammar.

// isTSTest reports whether a file holds tests: *.test.ts, *.spec.js, or
// anything under a __tests__, test or tests directory.
func isTSTest(rel string) bool {
	base := path.Base(rel)
	for _, dir := range []string{"/__tests__/", "/test/", "/tests/"} {
		if strings.Contains("/"+rel, dir) {
			return true
		}
	}
	stem := strings.TrimSuffix(base, path.Ext(base))
	return strings.HasSuffix(stem, ".test") || strings.HasSuffix(stem, ".spec")
}

// tsModule is the module name of a file: its path without extension, or
// its directory for an index file.
func tsModule(rel string) string {
	p := strings.TrimSuffix(rel, path.Ext(rel))
	if path.Base(p) == "index" {
		if d := path.Dir(p); d != "." {
			return d
		}
	}
	return p
}

// tsImport is what a local name imported from another module refers to.
type tsImport struct {
	module string // module name: "src/orders/service", or a package ("express")
	member string // the exported name; "" for the module itself (import * as x, require)
}

type tsFile struct {
	src     []byte
	rel     string
	module  string
	isTest  bool
	imports map[string]tsImport
	classes map[string]bool   // classes and interfaces declared in the module
	consts  map[string]string // module-level string constants
	vars    map[string]string // module-level variables with a known type: const service = new OrderService()
	res     *FileResult
	// prefixes maps router variables mounted with app.use("/api", router)
	// to their path prefix.
	prefixes map[string]string
}

// ParseTypeScript extracts the declarations, imports, call sites, routes and
// service contracts of a TypeScript or JavaScript file.
func ParseTypeScript(rel string, src []byte) (*FileResult, error) {
	lang := tsx.GetLanguage()
	if ext := path.Ext(rel); ext == ".ts" || ext == ".mts" || ext == ".cts" {
		lang = typescript.GetLanguage()
	}
	root, done, err := parseTree(lang, src)
	if err != nil {
		return nil, err
	}
	defer done()
	mod := tsModule(rel)
	tf := &tsFile{src: src, rel: rel, module: mod, isTest: isTSTest(rel), imports: map[string]tsImport{},
		classes: map[string]bool{}, consts: map[string]string{}, vars: map[string]string{}, prefixes: map[string]string{}}
	tf.res = &FileResult{File: store.File{Path: rel, Dir: path.Dir(rel), Lang: "typescript", Package: mod, Qual: mod,
		Doc: tsLeadingDoc(root, src)}}
	// Imports and module-level names first: calls anywhere refer to them.
	for _, n := range namedChildren(root) {
		tf.imports1(n)
		d := unexport(n)
		switch d.Type() {
		case "class_declaration", "abstract_class_declaration", "interface_declaration":
			tf.classes[text(field(d, "name"), src)] = true
		case "lexical_declaration", "variable_declaration":
			for _, v := range namedChildren(d) {
				if v.Type() != "variable_declarator" {
					continue
				}
				name, value := text(field(v, "name"), src), field(v, "value")
				if s := tf.template(value); s != "*" && !strings.Contains(s, "*") {
					tf.consts[name] = s
				}
				if value != nil && value.Type() == "new_expression" {
					if ctor := text(field(value, "constructor"), src); !strings.Contains(ctor, ".") {
						tf.vars[name] = tf.nameRef(ctor)
					}
				}
				if ref := tf.typeRef(field(v, "type")); ref != "" {
					tf.vars[name] = ref
				}
			}
		}
	}
	for _, n := range namedChildren(root) {
		tf.routerPrefix(n)
	}
	for _, n := range namedChildren(root) {
		tf.decl(n)
	}
	return tf.res, nil
}

// unexport returns the declaration of an export statement.
func unexport(n *sitter.Node) *sitter.Node {
	if n.Type() == "export_statement" {
		if d := field(n, "declaration"); d != nil {
			return d
		}
	}
	return n
}

// resolveModule turns an import specifier into a module name: a relative
// path is joined to the importing file's directory, extension dropped; a
// package is kept as written.
func (tf *tsFile) resolveModule(spec string) string {
	if !strings.HasPrefix(spec, ".") {
		return spec
	}
	p := path.Join(path.Dir(tf.rel), spec)
	switch path.Ext(p) {
	case ".js", ".jsx", ".ts", ".tsx", ".mjs", ".cjs", ".mts", ".cts":
		p = strings.TrimSuffix(p, path.Ext(p))
	}
	if path.Base(p) == "index" && path.Dir(p) != "." {
		p = path.Dir(p)
	}
	return p
}

// imports1 records the imports of a top-level statement: ES imports and
// const x = require("..."), const { a, b: c } = require("...").
func (tf *tsFile) imports1(n *sitter.Node) {
	add := func(local, module, member string) {
		tf.imports[local] = tsImport{module, member}
		tf.res.Imports = append(tf.res.Imports, store.Import{Path: module, Alias: local})
	}
	switch n.Type() {
	case "import_statement":
		module := tf.resolveModule(tsString(field(n, "source"), tf.src))
		for _, c := range namedChildren(n) {
			if c.Type() != "import_clause" {
				continue
			}
			for _, part := range namedChildren(c) {
				switch part.Type() {
				case "identifier": // default import
					add(text(part, tf.src), module, "default")
				case "namespace_import":
					if id := part.NamedChild(0); id != nil {
						add(text(id, tf.src), module, "")
					}
				case "named_imports":
					for _, spec := range namedChildren(part) {
						if spec.Type() != "import_specifier" {
							continue
						}
						name := text(field(spec, "name"), tf.src)
						local := name
						if a := field(spec, "alias"); a != nil {
							local = text(a, tf.src)
						}
						add(local, module, name)
					}
				}
			}
		}
	case "lexical_declaration", "variable_declaration":
		for _, v := range namedChildren(n) {
			value := field(v, "value")
			if v.Type() != "variable_declarator" || value == nil || value.Type() != "call_expression" ||
				text(field(value, "function"), tf.src) != "require" {
				continue
			}
			args := namedChildren(field(value, "arguments"))
			if len(args) != 1 {
				continue
			}
			module := tf.resolveModule(tsString(args[0], tf.src))
			switch name := field(v, "name"); name.Type() {
			case "identifier":
				add(text(name, tf.src), module, "")
			case "object_pattern":
				for _, p := range namedChildren(name) {
					switch p.Type() {
					case "shorthand_property_identifier_pattern":
						add(text(p, tf.src), module, text(p, tf.src))
					case "pair_pattern":
						add(text(field(p, "value"), tf.src), module, text(field(p, "key"), tf.src))
					}
				}
			}
		}
	}
}

// tsString returns the contents of a string literal.
func tsString(n *sitter.Node, src []byte) string {
	if n == nil {
		return ""
	}
	t := text(n, src)
	if len(t) >= 2 && (t[0] == '"' || t[0] == '\'' || t[0] == '`') {
		return t[1 : len(t)-1]
	}
	return t
}

// tsLeadingDoc is a file's leading comment, used as the module's doc.
func tsLeadingDoc(root *sitter.Node, src []byte) string {
	if c := root.NamedChild(0); c != nil && c.Type() == "comment" {
		return cleanComment(text(c, src))
	}
	return ""
}

var commentLine = regexp.MustCompile(`(?m)^\s*(/\*\*?|\*/|\*|//)\s?`)

// cleanComment strips comment markers: //, /* */, /** */ and leading *.
func cleanComment(c string) string {
	c = strings.TrimSuffix(strings.TrimSpace(c), "*/")
	return strings.TrimSpace(commentLine.ReplaceAllString(c, ""))
}

// docOf returns the comment directly above a declaration (or the export
// statement holding it).
func (tf *tsFile) docOf(n *sitter.Node) string {
	if n.Parent() != nil && n.Parent().Type() == "export_statement" {
		n = n.Parent()
	}
	if p := n.PrevNamedSibling(); p != nil && p.Type() == "comment" && int(n.StartPoint().Row)-int(p.EndPoint().Row) <= 1 {
		return cleanComment(text(p, tf.src))
	}
	return ""
}

func (tf *tsFile) add(n *sitter.Node, name, qualified, kind, sig, doc string, exported bool) *store.Symbol {
	tf.res.Symbols = append(tf.res.Symbols, store.Symbol{
		File: tf.rel, Lang: "typescript", Package: tf.module, Name: name, Qualified: qualified, Kind: kind,
		Signature: sig, Doc: doc, StartLine: line(n), EndLine: endLine(n), Exported: exported, Body: text(n, tf.src),
	})
	return &tf.res.Symbols[len(tf.res.Symbols)-1]
}

// signature is a declaration's text up to its body, on one line.
func (tf *tsFile) signature(n, body *sitter.Node) string {
	return header(n, body, tf.src)
}

// decl records a top-level statement: declarations, and the calls,
// routes and tests of top-level code.
func (tf *tsFile) decl(n *sitter.Node) {
	exported := n.Type() == "export_statement"
	d := unexport(n)
	switch d.Type() {
	case "function_declaration", "generator_function_declaration":
		name := text(field(d, "name"), tf.src)
		q := tf.module + "." + name
		sym := tf.add(d, name, q, "func", tf.signature(d, field(d, "body")), tf.docOf(d), exported)
		sym.Arity = tsArity(field(d, "parameters"))
		tf.body(d, field(d, "body"), q, "", tf.locals(field(d, "parameters")))
	case "class_declaration", "abstract_class_declaration":
		tf.class(d, n, exported)
	case "interface_declaration":
		name := text(field(d, "name"), tf.src)
		q := tf.module + "." + name
		tf.add(d, name, q, "interface", "interface "+name, tf.docOf(d), exported)
		for _, c := range namedChildren(d) {
			if c.Type() == "extends_type_clause" {
				for _, t := range namedChildren(c) {
					if ref := tf.typeRef(t); ref != "" {
						tf.res.Supertypes = append(tf.res.Supertypes, store.Supertype{Owner: q, Kind: store.SuperExtends, TypeRef: ref})
					}
				}
			}
		}
		for _, m := range namedChildren(field(d, "body")) {
			if m.Type() == "method_signature" {
				mn := text(field(m, "name"), tf.src)
				sym := tf.add(m, mn, q+"."+mn, "interface method", strings.TrimSuffix(text(m, tf.src), ";"), tf.docOf(m), true)
				sym.Arity = tsArity(field(m, "parameters"))
			}
		}
	case "type_alias_declaration":
		name := text(field(d, "name"), tf.src)
		tf.add(d, name, tf.module+"."+name, "type", strings.TrimSuffix(tf.signature(d, nil), ";"), tf.docOf(d), exported)
	case "enum_declaration":
		name := text(field(d, "name"), tf.src)
		tf.add(d, name, tf.module+"."+name, "enum", "enum "+name, tf.docOf(d), exported)
	case "lexical_declaration", "variable_declaration":
		for _, v := range namedChildren(d) {
			value := field(v, "value")
			if v.Type() != "variable_declarator" || value == nil {
				continue
			}
			if value.Type() == "arrow_function" || value.Type() == "function_expression" || value.Type() == "function" {
				name := text(field(v, "name"), tf.src)
				q := tf.module + "." + name
				sym := tf.add(v, name, q, "func", tf.signature(v, field(value, "body")), tf.docOf(d), exported)
				sym.Arity = tsArity(field(value, "parameters"))
				tf.body(v, field(value, "body"), q, "", tf.locals(field(value, "parameters")))
				continue
			}
			tf.body(v, value, tf.module+".<module>", "", map[string]string{})
		}
	case "expression_statement":
		if tf.assignedFunction(d) {
			return
		}
		tf.body(d, d, tf.module+".<module>", "", map[string]string{})
	}
}

// assignedFunction records a function assigned at the top level, as older
// JavaScript declares them: exports.parse = function () {},
// module.exports = function createApplication() {}, Router.prototype.route
// = function () {} (method route of Router), app.handle = function () {}.
// It reports whether the statement was one.
func (tf *tsFile) assignedFunction(stmt *sitter.Node) bool {
	as := stmt.NamedChild(0)
	if as == nil || as.Type() != "assignment_expression" {
		return false
	}
	left, right := field(as, "left"), field(as, "right")
	if right == nil || (right.Type() != "function_expression" && right.Type() != "function" && right.Type() != "arrow_function") {
		return false
	}
	name := text(left, tf.src)
	for _, p := range []string{"module.exports.", "exports."} {
		name = strings.TrimPrefix(name, p)
	}
	if name == "module.exports" {
		name = text(field(right, "name"), tf.src) // module.exports = function createApplication() {}
		if name == "" {
			return false
		}
	}
	name = strings.Replace(name, ".prototype.", ".", 1)
	if strings.ContainsAny(name, "[]()") || name == "" {
		return false
	}
	kind, short, class := "func", name, ""
	if i := strings.LastIndexByte(name, '.'); i >= 0 {
		kind, short, class = "method", name[i+1:], tf.module+"."+name[:i]
	}
	q := tf.module + "." + name
	sym := tf.add(stmt, short, q, kind, tf.signature(stmt, field(right, "body")), tf.docOf(stmt), true)
	sym.Arity = tsArity(field(right, "parameters"))
	tf.body(stmt, field(right, "body"), q, class, tf.locals(field(right, "parameters")))
	return true
}

// class records a class, its members, its supertypes and, for a NestJS
// controller, its routes.
func (tf *tsFile) class(d, outer *sitter.Node, exported bool) {
	name := text(field(d, "name"), tf.src)
	q := tf.module + "." + name
	body := field(d, "body")
	tf.add(d, name, q, "class", tf.signature(d, body), tf.docOf(d), exported)
	for _, c := range namedChildren(d) {
		if c.Type() != "class_heritage" {
			continue
		}
		for _, clause := range namedChildren(c) {
			kind := store.SuperExtends
			if clause.Type() == "implements_clause" {
				kind = store.SuperImplements
			}
			for _, t := range namedChildren(clause) {
				if ref := tf.typeRef(t); ref != "" {
					tf.res.Supertypes = append(tf.res.Supertypes, store.Supertype{Owner: q, Kind: kind, TypeRef: ref})
				}
			}
		}
	}
	// NestJS: @Controller("prefix") on the class, @Get(":id") on methods.
	prefix, controller := "", false
	for _, dec := range tsDecorators(outer, d) {
		if fn, args := tf.decoratorCall(dec); fn == "Controller" {
			controller = true
			if len(args) > 0 {
				prefix = tf.template(args[0])
				if args[0].Type() == "object" {
					prefix = tf.template(tsProperty(args[0], "path", tf.src))
				}
			}
		}
	}
	fields := map[string]string{} // field name -> type reference
	var pending []*sitter.Node    // decorators before the next member
	for _, m := range namedChildren(body) {
		switch m.Type() {
		case "decorator":
			pending = append(pending, m)
			continue
		case "public_field_definition", "field_definition":
			fname := text(field(m, "name"), tf.src)
			if fname == "" {
				fname = text(field(m, "property"), tf.src)
			}
			if t := field(m, "type"); t != nil {
				if ref := tf.typeRef(t); ref != "" {
					fields[fname] = ref
					tf.res.Fields = append(tf.res.Fields, store.Field{Owner: q, Name: fname, TypeRef: ref})
				}
			}
			if v := field(m, "value"); v != nil && (v.Type() == "arrow_function" || v.Type() == "function_expression") {
				mq := q + "." + fname
				sym := tf.add(m, fname, mq, "method", tf.signature(m, field(v, "body")), tf.docOf(m), !tsPrivate(m, tf.src))
				sym.Arity = tsArity(field(v, "parameters"))
				tf.body(m, field(v, "body"), mq, q, tf.locals(field(v, "parameters")))
			}
		case "method_definition":
			mname := text(field(m, "name"), tf.src)
			params := field(m, "parameters")
			kind, mq := "method", q+"."+mname
			if mname == "constructor" {
				kind, mname, mq = "constructor", name, q+"."+name
				// constructor(private repo: OrderRepository): a field too.
				for _, p := range namedChildren(params) {
					if hasChild(p, "accessibility_modifier") || hasChild(p, "readonly") {
						pn := text(field(p, "pattern"), tf.src)
						if ref := tf.typeRef(field(p, "type")); ref != "" && pn != "" {
							fields[pn] = ref
							tf.res.Fields = append(tf.res.Fields, store.Field{Owner: q, Name: pn, TypeRef: ref})
						}
					}
				}
			}
			mbody := field(m, "body")
			sym := tf.add(m, mname, mq, kind, tf.signature(m, mbody), tf.docOf(m), !tsPrivate(m, tf.src))
			sym.Arity = tsArity(params)
			sym.Result = tf.typeRef(field(m, "return_type"))
			if controller {
				for _, dec := range pending {
					fn, args := tf.decoratorCall(dec)
					if method := nestMethods[fn]; method != "" {
						route := ""
						if len(args) > 0 {
							route = tf.template(args[0])
						}
						tf.res.Endpoints = append(tf.res.Endpoints, store.Endpoint{Method: method,
							Path: joinRoute("/"+strings.Trim(prefix, "/"), route), Handler: mq, Framework: "nestjs", File: tf.rel, Line: line(m)})
					}
				}
			}
			if mbody != nil {
				locals := tf.locals(params)
				tf.bodyFields(m, mbody, mq, q, locals, fields)
			}
		}
		pending = nil
	}
}

var nestMethods = map[string]string{"Get": "GET", "Post": "POST", "Put": "PUT", "Patch": "PATCH", "Delete": "DELETE",
	"Head": "HEAD", "Options": "OPTIONS", "All": "ANY"}

// tsDecorators returns the decorators of a class declaration, which the
// grammar attaches to it or to the export statement around it.
func tsDecorators(outer, d *sitter.Node) []*sitter.Node {
	var out []*sitter.Node
	for _, n := range []*sitter.Node{outer, d} {
		for i := 0; i < int(n.NamedChildCount()); i++ {
			if c := n.NamedChild(i); c.Type() == "decorator" {
				out = append(out, c)
			}
		}
		if n == d {
			break
		}
	}
	return out
}

// decoratorCall returns a decorator's name and arguments: @Get(":id").
func (tf *tsFile) decoratorCall(dec *sitter.Node) (string, []*sitter.Node) {
	e := dec.NamedChild(0)
	if e == nil {
		return "", nil
	}
	if e.Type() == "call_expression" {
		name := text(field(e, "function"), tf.src)
		return name[strings.LastIndex(name, ".")+1:], namedChildren(field(e, "arguments"))
	}
	name := text(e, tf.src)
	return name[strings.LastIndex(name, ".")+1:], nil
}

func tsPrivate(m *sitter.Node, src []byte) bool {
	for _, c := range namedChildren(m) {
		if c.Type() == "accessibility_modifier" && text(c, src) == "private" {
			return true
		}
	}
	return strings.HasPrefix(text(field(m, "name"), src), "#")
}

// tsArity describes a parameter list as Java's does: "2", or "1+" with a
// rest parameter. Optional parameters make it unknown ("").
func tsArity(params *sitter.Node) string {
	n := 0
	for _, p := range namedChildren(params) {
		switch p.Type() {
		case "required_parameter":
			if hasChild(p, "rest_pattern") {
				return ""
			}
			n++
		case "optional_parameter":
			return ""
		case "identifier", "object_pattern", "array_pattern":
			n++
		default:
			return ""
		}
	}
	return itoa(n)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}

// typeRef turns a type into a reference the resolver understands: an
// imported class "@module.Name", a class of this module or a guess
// "~module.Name". Generic types are referred to by their base type;
// predefined types and unions give "".
func (tf *tsFile) typeRef(n *sitter.Node) string {
	if n == nil {
		return ""
	}
	switch n.Type() {
	case "type_annotation":
		if c := n.NamedChild(0); c != nil {
			return tf.typeRef(c)
		}
	case "generic_type":
		name := field(n, "name")
		if name == nil {
			name = n.NamedChild(0)
		}
		if t := text(name, tf.src); t == "Promise" || t == "Observable" || t == "Array" {
			if args := field(n, "type_arguments"); args != nil && args.NamedChildCount() == 1 && t == "Promise" {
				return tf.typeRef(args.NamedChild(0))
			}
		}
		return tf.typeRef(name)
	case "type_identifier", "identifier":
		return tf.nameRef(text(n, tf.src))
	case "nested_type_identifier", "member_expression":
		t := text(n, tf.src)
		first, rest, _ := strings.Cut(t, ".")
		if imp, ok := tf.imports[first]; ok && imp.member == "" {
			return "@" + imp.module + "." + rest
		}
		return ""
	}
	return ""
}

// nameRef is the reference of a type or class name used in this module.
func (tf *tsFile) nameRef(name string) string {
	if name == "" {
		return ""
	}
	if imp, ok := tf.imports[name]; ok {
		if imp.member == "" || imp.member == "default" {
			return "@" + imp.module + "." + name
		}
		return "@" + imp.module + "." + imp.member
	}
	if tf.classes[name] {
		return tf.module + "." + name
	}
	return "~" + tf.module + "." + name
}

// locals types the parameters of a function: name -> type reference.
func (tf *tsFile) locals(params *sitter.Node) map[string]string {
	locals := map[string]string{}
	for _, p := range namedChildren(params) {
		if p.Type() != "required_parameter" && p.Type() != "optional_parameter" {
			continue
		}
		if ref := tf.typeRef(field(p, "type")); ref != "" {
			locals[text(field(p, "pattern"), tf.src)] = ref
		}
	}
	return locals
}

func (tf *tsFile) body(decl, body *sitter.Node, caller, class string, locals map[string]string) {
	tf.bodyFields(decl, body, caller, class, locals, nil)
}

// bodyFields records the calls, HTTP requests, routes, broker calls and
// tests in a function body (or top-level code). class is the enclosing
// class, fields its typed fields.
func (tf *tsFile) bodyFields(decl, body *sitter.Node, caller, class string, locals, fields map[string]string) {
	if body == nil {
		return
	}
	clients := map[string]string{} // broker clients and producers held in variables
	walk(body, func(n *sitter.Node) bool {
		switch n.Type() {
		case "function_declaration", "class_declaration", "method_definition":
			return n == body
		case "variable_declarator":
			value, name := field(n, "value"), field(n, "name")
			if value == nil || name == nil || name.Type() != "identifier" {
				return true
			}
			v := text(name, tf.src)
			if value.Type() == "new_expression" {
				if ref := tf.nameRef(text(field(value, "constructor"), tf.src)); ref != "" {
					locals[v] = ref
				}
			}
			if ref := tf.typeRef(field(n, "type")); ref != "" {
				locals[v] = ref
			}
			if k := tf.brokerClient(value, clients); k != "" {
				clients[v] = k
			}
		case "call_expression":
			if tf.isTest && tf.testCall(n) {
				return false
			}
			tf.call(n, caller, class, locals, fields)
			if c, ok := tf.httpClientCall(n, caller); ok {
				tf.res.Contracts = append(tf.res.Contracts, c)
			}
			tf.route(n, caller)
			tf.res.Contracts = append(tf.res.Contracts, tf.messagingCall(n, caller, clients)...)
		case "new_expression":
			ctor := text(field(n, "constructor"), tf.src)
			if ref := tf.nameRef(ctor); ref != "" && !strings.Contains(ctor, ".") {
				tf.res.Calls = append(tf.res.Calls, store.RawCall{Caller: caller, Form: store.CallTyped, A: ref, B: ctor,
					Line: line(n), Args: argCount(n)})
			}
			tf.res.Contracts = append(tf.res.Contracts, tf.messagingNew(n, caller)...)
		}
		return true
	})
}

// testCall records it("...", fn) or test("...", fn) in a test file as a
// test function named by its title, with the calls of its callback, and
// describe("...", fn) blocks as grouping. It reports whether n was one.
func (tf *tsFile) testCall(n *sitter.Node) bool {
	fn := text(field(n, "function"), tf.src)
	base, _, _ := strings.Cut(fn, ".") // it.only, test.each, describe.skip
	args := namedChildren(field(n, "arguments"))
	if len(args) < 2 {
		return false
	}
	cb := args[len(args)-1]
	if cb.Type() != "arrow_function" && cb.Type() != "function_expression" && cb.Type() != "function" {
		return false
	}
	switch base {
	case "it", "test":
		title := tsString(args[0], tf.src)
		q := tf.module + ".test:" + title
		sym := tf.add(n, title, q, "func", base+"("+text(args[0], tf.src)+")", "", false)
		sym.Arity = "0"
		tf.body(n, field(cb, "body"), q, "", map[string]string{})
		return true
	case "describe", "context", "suite":
		tf.body(n, field(cb, "body"), tf.module+".<module>", "", map[string]string{})
		return true
	}
	return false
}

// call records a call site in the forms the resolver understands.
func (tf *tsFile) call(n *sitter.Node, caller, class string, locals, fields map[string]string) {
	fn := field(n, "function")
	rc := store.RawCall{Caller: caller, Line: line(n), Args: argCount(n)}
	switch fn.Type() {
	case "identifier":
		name := text(fn, tf.src)
		if name == "require" {
			return
		}
		if imp, ok := tf.imports[name]; ok {
			member := imp.member
			if member == "" || member == "default" {
				member = name
			}
			rc.Form, rc.A, rc.B = store.CallImport, imp.module, member
		} else {
			rc.Form, rc.A = store.CallQualified, tf.module+"."+name
		}
	case "member_expression":
		obj, prop := field(fn, "object"), text(field(fn, "property"), tf.src)
		switch {
		case obj == nil:
			return
		case obj.Type() == "this" && class != "":
			rc.Form, rc.A, rc.B = store.CallQualified, class+"."+prop, prop
		case obj.Type() == "super" && class != "":
			rc.Form, rc.A, rc.B = store.CallSuper, class, prop
		case obj.Type() == "identifier":
			x := text(obj, tf.src)
			switch imp, imported := tf.imports[x]; {
			case locals[x] != "":
				rc.Form, rc.A, rc.B = store.CallTyped, locals[x], prop
			case tf.vars[x] != "":
				rc.Form, rc.A, rc.B = store.CallTyped, tf.vars[x], prop
			case imported && (imp.member == "" || imp.member == "default"):
				rc.Form, rc.A, rc.B = store.CallImport, imp.module, prop // a namespace or module object
			case imported:
				rc.Form, rc.A, rc.B = store.CallTyped, "@"+imp.module+"."+imp.member, prop // a static call on an imported class
			case tf.classes[x]:
				rc.Form, rc.A, rc.B = store.CallQualified, tf.module+"."+x+"."+prop, prop
			default:
				rc.Form, rc.B = store.CallMethod, prop
			}
		case obj.Type() == "member_expression" && field(obj, "object") != nil && field(obj, "object").Type() == "this" && class != "":
			f := text(field(obj, "property"), tf.src)
			if fields[f] != "" {
				rc.Form, rc.A, rc.B = store.CallTyped, fields[f], prop
			} else {
				rc.Form, rc.A, rc.B, rc.C = store.CallField, class, f, prop
			}
		default:
			rc.Form, rc.B = store.CallMethod, prop
		}
	default:
		return
	}
	tf.res.Calls = append(tf.res.Calls, rc)
}

// template evaluates a string expression as far as the source shows it:
// strings, template literals, concatenation and constants; process.env.X
// and process.env["X"] become the placeholder ${X}, resolved from
// configuration files. Unknown parts are "*".
func (tf *tsFile) template(n *sitter.Node) string {
	if n == nil {
		return "*"
	}
	switch n.Type() {
	case "string":
		return tsString(n, tf.src)
	case "template_string":
		var b strings.Builder
		for _, c := range namedChildren(n) {
			switch c.Type() {
			case "string_fragment":
				b.WriteString(text(c, tf.src))
			case "template_substitution":
				if e := c.NamedChild(0); e != nil {
					b.WriteString(tf.template(e))
				} else {
					b.WriteString("*")
				}
			}
		}
		return b.String()
	case "binary_expression":
		if op := field(n, "operator"); op != nil && text(op, tf.src) == "+" {
			return tf.template(field(n, "left")) + tf.template(field(n, "right"))
		}
	case "parenthesized_expression":
		if c := n.NamedChild(0); c != nil {
			return tf.template(c)
		}
	case "identifier":
		if v, ok := tf.consts[text(n, tf.src)]; ok {
			return v
		}
	case "member_expression", "subscript_expression":
		if env := tf.envVar(n); env != "" {
			return env
		}
	}
	return "*"
}

// envVar returns ${X} for process.env.X or process.env["X"].
func (tf *tsFile) envVar(n *sitter.Node) string {
	obj := field(n, "object")
	if obj == nil || text(obj, tf.src) != "process.env" {
		return ""
	}
	key := ""
	if n.Type() == "member_expression" {
		key = text(field(n, "property"), tf.src)
	} else {
		key = tsString(field(n, "index"), tf.src)
	}
	if key == "" || strings.ContainsAny(key, "*$`'\"") {
		return ""
	}
	return "${" + key + "}"
}

// tsProperty returns the value of a property of an object literal.
func tsProperty(obj *sitter.Node, key string, src []byte) *sitter.Node {
	if obj == nil || obj.Type() != "object" {
		return nil
	}
	for _, p := range namedChildren(obj) {
		switch p.Type() {
		case "pair":
			if strings.Trim(text(field(p, "key"), src), `"'`) == key {
				return field(p, "value")
			}
		case "shorthand_property_identifier":
			if text(p, src) == key {
				return p
			}
		}
	}
	return nil
}

var httpVerbs = map[string]string{"get": "GET", "post": "POST", "put": "PUT", "patch": "PATCH", "delete": "DELETE",
	"head": "HEAD", "options": "OPTIONS", "all": "ANY"}

// routerPrefix records app.use("/api", router) at the top level, for the
// routes registered on router.
func (tf *tsFile) routerPrefix(n *sitter.Node) {
	if n.Type() != "expression_statement" {
		return
	}
	call := n.NamedChild(0)
	if call == nil || call.Type() != "call_expression" {
		return
	}
	fn := field(call, "function")
	if fn == nil || fn.Type() != "member_expression" || text(field(fn, "property"), tf.src) != "use" {
		return
	}
	args := namedChildren(field(call, "arguments"))
	if len(args) == 2 && args[0].Type() == "string" && args[1].Type() == "identifier" {
		tf.prefixes[text(args[1], tf.src)] = tsString(args[0], tf.src)
	}
}

// route records an Express, Fastify or Koa-style route:
// app.get("/orders/:id", handler), router.post(...).
func (tf *tsFile) route(n *sitter.Node, caller string) {
	fn := field(n, "function")
	if fn == nil || fn.Type() != "member_expression" {
		return
	}
	method := httpVerbs[text(field(fn, "property"), tf.src)]
	obj := field(fn, "object")
	args := namedChildren(field(n, "arguments"))
	if method == "" || obj == nil || obj.Type() != "identifier" || len(args) < 2 || args[0].Type() != "string" {
		return
	}
	route := tsString(args[0], tf.src)
	if !strings.HasPrefix(route, "/") {
		return
	}
	handler := tf.module + ".<inline handler>"
	switch h := args[len(args)-1]; h.Type() {
	case "identifier":
		if imp, ok := tf.imports[text(h, tf.src)]; ok && imp.member != "" && imp.member != "default" {
			handler = imp.module + "." + imp.member
		} else {
			handler = tf.module + "." + text(h, tf.src)
		}
	case "member_expression":
		handler = text(h, tf.src)
	}
	tf.res.Endpoints = append(tf.res.Endpoints, store.Endpoint{Method: method,
		Path: joinRoute(tf.prefixes[text(obj, tf.src)], route), Handler: handler, Framework: tf.framework(), File: tf.rel, Line: line(n)})
}

// framework names the HTTP framework the module imports.
func (tf *tsFile) framework() string {
	for _, imp := range tf.imports {
		switch imp.module {
		case "express", "fastify", "koa", "@koa/router", "koa-router", "hono":
			return strings.TrimPrefix(strings.TrimSuffix(imp.module, "/router"), "@")
		}
	}
	return "node"
}

// httpClientCall recognizes fetch(url, { method }) and axios.get(url),
// axios.post(url, ...), axios({ url, method }), axios.request({...}).
func (tf *tsFile) httpClientCall(n *sitter.Node, caller string) (store.Contract, bool) {
	fn := field(n, "function")
	args := namedChildren(field(n, "arguments"))
	if fn == nil || len(args) == 0 {
		return store.Contract{}, false
	}
	isAxios := func(name string) bool {
		imp, ok := tf.imports[name]
		return ok && imp.module == "axios"
	}
	switch {
	case fn.Type() == "identifier" && text(fn, tf.src) == "fetch":
		method := "GET"
		if len(args) > 1 {
			if m := tsProperty(args[1], "method", tf.src); m != nil {
				method = strings.ToUpper(tf.template(m))
			}
		}
		if strings.Contains(method, "*") {
			method = ""
		}
		return httpUse(method, tf.template(args[0]), caller, line(n))
	case fn.Type() == "identifier" && isAxios(text(fn, tf.src)) && args[0].Type() == "object":
		method := strings.ToUpper(tf.template(tsProperty(args[0], "method", tf.src)))
		if strings.Contains(method, "*") {
			method = "GET"
		}
		return httpUse(method, tf.template(tsProperty(args[0], "url", tf.src)), caller, line(n))
	case fn.Type() == "member_expression" && isAxios(text(field(fn, "object"), tf.src)):
		verb := text(field(fn, "property"), tf.src)
		if verb == "request" && args[0].Type() == "object" {
			method := strings.ToUpper(tf.template(tsProperty(args[0], "method", tf.src)))
			if strings.Contains(method, "*") {
				method = "GET"
			}
			return httpUse(method, tf.template(tsProperty(args[0], "url", tf.src)), caller, line(n))
		}
		if m := httpVerbs[verb]; m != "" && m != "ANY" {
			return httpUse(m, tf.template(args[0]), caller, line(n))
		}
	}
	return store.Contract{}, false
}
