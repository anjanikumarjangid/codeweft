package indexer

import (
	"path"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/python"

	"github.com/anjanikumarjangid/codewiki/internal/store"
)

func isPythonTest(rel string) bool {
	base := path.Base(rel)
	return strings.HasPrefix(base, "test_") || strings.HasSuffix(base, "_test.py") ||
		base == "conftest.py" || strings.Contains("/"+rel, "/tests/")
}

// pythonModule maps a file path to its dotted module name:
// "src/inventory/stock.py" -> "inventory.stock", "pkg/__init__.py" -> "pkg".
func pythonModule(rel string) string {
	m := strings.TrimSuffix(rel, ".py")
	m = strings.TrimSuffix(m, "/__init__")
	m = strings.TrimPrefix(m, "src/")
	return strings.ReplaceAll(m, "/", ".")
}

type pyFile struct {
	src      []byte
	rel      string
	module   string
	pkg      string            // package the module lives in, for relative imports
	imports  map[string]string // local name -> dotted target
	prefixes map[string]string // router/blueprint variable -> URL prefix
	res      *FileResult
}

// ParsePython extracts classes, methods and functions, imports, attribute
// types assigned in methods, and call sites.
func ParsePython(rel string, src []byte) (*FileResult, error) {
	root, done, err := parseTree(python.GetLanguage(), src)
	if err != nil {
		return nil, err
	}
	defer done()

	mod := pythonModule(rel)
	pf := &pyFile{src: src, rel: rel, module: mod, imports: map[string]string{}, prefixes: map[string]string{}}
	pf.pkg = mod
	if path.Base(rel) != "__init__.py" {
		if i := strings.LastIndex(mod, "."); i >= 0 {
			pf.pkg = mod[:i]
		} else {
			pf.pkg = ""
		}
	}
	pf.res = &FileResult{File: store.File{
		Path: rel, Dir: path.Dir(rel), Lang: "python", Package: mod, Qual: mod,
		Doc: pyDocstring(root, src),
	}}

	for _, n := range namedChildren(root) {
		switch n.Type() {
		case "import_statement":
			pf.importStmt(n)
		case "import_from_statement":
			pf.importFrom(n)
		}
	}
	for _, n := range namedChildren(root) {
		pf.routerPrefix(n)
		pf.mainGuard(n)
	}
	for _, n := range namedChildren(root) {
		def := unwrapDecorated(n)
		switch def.Type() {
		case "class_definition":
			pf.class(def, n, mod)
		case "function_definition":
			pf.function(def, n, mod, "")
		}
	}
	return pf.res, nil
}

func unwrapDecorated(n *sitter.Node) *sitter.Node {
	if n.Type() == "decorated_definition" {
		if d := field(n, "definition"); d != nil {
			return d
		}
	}
	return n
}

// pyDocstring returns the docstring of a module, class or function body.
func pyDocstring(body *sitter.Node, src []byte) string {
	kids := namedChildren(body)
	if len(kids) == 0 || kids[0].Type() != "expression_statement" {
		return ""
	}
	s := kids[0].NamedChild(0)
	if s == nil || s.Type() != "string" {
		return ""
	}
	var b strings.Builder
	for _, c := range namedChildren(s) {
		if c.Type() == "string_content" {
			b.WriteString(text(c, src))
		}
	}
	return dedent(b.String())
}

func (pf *pyFile) importStmt(n *sitter.Node) {
	for _, c := range namedChildren(n) {
		switch c.Type() {
		case "dotted_name":
			full := text(c, pf.src)
			first := strings.SplitN(full, ".", 2)[0] // `import a.b` binds `a`
			pf.imports[first] = first
			pf.res.Imports = append(pf.res.Imports, store.Import{Path: full, Alias: first})
		case "aliased_import":
			full, alias := text(field(c, "name"), pf.src), text(field(c, "alias"), pf.src)
			pf.imports[alias] = full
			pf.res.Imports = append(pf.res.Imports, store.Import{Path: full, Alias: alias})
		}
	}
}

func (pf *pyFile) importFrom(n *sitter.Node) {
	modNode := field(n, "module_name")
	module := text(modNode, pf.src)
	if modNode != nil && modNode.Type() == "relative_import" {
		dots := len(module) - len(strings.TrimLeft(module, "."))
		rest := strings.TrimLeft(module, ".")
		base := strings.Split(pf.pkg, ".")
		if pf.pkg == "" {
			base = nil
		}
		if up := dots - 1; up > 0 && up <= len(base) {
			base = base[:len(base)-up]
		}
		if rest != "" {
			base = append(base, rest)
		}
		module = strings.Join(base, ".")
	}
	for _, c := range namedChildren(n) {
		if c == modNode {
			continue
		}
		var name, alias string
		switch c.Type() {
		case "dotted_name":
			name = text(c, pf.src)
			alias = name
		case "aliased_import":
			name, alias = text(field(c, "name"), pf.src), text(field(c, "alias"), pf.src)
		default:
			continue
		}
		full := name
		if module != "" {
			full = module + "." + name
		}
		pf.imports[alias] = full
		pf.res.Imports = append(pf.res.Imports, store.Import{Path: full, Alias: alias})
	}
}

// nameRef turns a type or class name used in this module into a reference.
func (pf *pyFile) nameRef(name string) string {
	switch name {
	case "", "str", "int", "float", "bool", "bytes", "dict", "list", "set", "tuple", "None", "object", "Any":
		return ""
	}
	if full, ok := pf.imports[name]; ok {
		return "@" + full
	}
	if i := strings.IndexByte(name, '.'); i > 0 {
		if full, ok := pf.imports[name[:i]]; ok {
			return "@" + full + name[i:]
		}
	}
	return "~" + pf.module + "." + name
}

// annotationRef reads a type annotation, unwrapping Optional[T] and "T".
func (pf *pyFile) annotationRef(n *sitter.Node) string {
	if n == nil {
		return ""
	}
	t := strings.Trim(text(n, pf.src), `"' `)
	if i := strings.IndexByte(t, '['); i > 0 {
		inner := strings.TrimSuffix(t[i+1:], "]")
		switch t[:i] {
		case "Optional", "typing.Optional":
			t = inner
		default:
			t = t[:i]
		}
	}
	t = strings.TrimSpace(strings.Split(t, "|")[0])
	return pf.nameRef(t)
}

func (pf *pyFile) add(n *sitter.Node, name, qualified, kind, sig, doc string) {
	pf.res.Symbols = append(pf.res.Symbols, store.Symbol{
		File: pf.rel, Lang: "python", Package: pf.module, Name: name, Qualified: qualified, Kind: kind,
		Signature: sig, Doc: doc, StartLine: line(n), EndLine: endLine(n),
		Exported: !strings.HasPrefix(name, "_"), Body: text(n, pf.src),
	})
}

func (pf *pyFile) class(def, outer *sitter.Node, prefix string) {
	name := text(field(def, "name"), pf.src)
	qual := prefix + "." + name
	body := field(def, "body")
	pf.add(outer, name, qual, "class", strings.TrimSuffix(header(def, body, pf.src), ":"), pyDocstring(body, pf.src))
	// Base classes: `class A(B, mod.C, Generic[T], metaclass=M)` records B,
	// mod.C and Generic. Keyword arguments such as metaclass are not bases.
	for _, b := range namedChildren(field(def, "superclasses")) {
		switch b.Type() {
		case "subscript":
			b = field(b, "value")
		case "identifier", "attribute":
		default:
			continue
		}
		if ref := pf.nameRef(text(b, pf.src)); ref != "" {
			pf.res.Supertypes = append(pf.res.Supertypes, store.Supertype{Owner: qual, Kind: store.SuperExtends, TypeRef: ref})
		}
	}
	for _, m := range namedChildren(body) {
		d := unwrapDecorated(m)
		switch d.Type() {
		case "function_definition":
			pf.function(d, m, qual, qual)
		case "class_definition":
			pf.class(d, m, qual)
		}
	}
}

func (pf *pyFile) function(def, outer *sitter.Node, prefix, class string) {
	name := text(field(def, "name"), pf.src)
	qual := prefix + "." + name
	kind := "func"
	if class != "" {
		kind = "method"
	}
	body := field(def, "body")
	pf.add(outer, name, qual, kind, strings.TrimSuffix(header(def, body, pf.src), ":"), pyDocstring(body, pf.src))
	pf.calls(def, body, qual, class)
	if outer.Type() == "decorated_definition" {
		for _, dec := range namedChildren(outer) {
			if dec.Type() != "decorator" {
				continue
			}
			if methods, route, _, ok := pythonRoute(dec, pf.src, pf.prefixes); ok {
				for _, m := range methods {
					pf.res.Endpoints = append(pf.res.Endpoints, store.Endpoint{
						Method: m, Path: route, Handler: qual, Framework: pf.framework(), File: pf.rel, Line: line(dec),
					})
				}
			}
		}
	}
}

func (pf *pyFile) framework() string {
	for _, p := range pf.imports {
		root := strings.SplitN(p, ".", 2)[0]
		if root == "fastapi" || root == "flask" || root == "starlette" || root == "sanic" || root == "quart" {
			return root
		}
	}
	return "python"
}

// routerPrefix records `router = APIRouter(prefix="/items")` and
// `bp = Blueprint("x", __name__, url_prefix="/x")`.
func (pf *pyFile) routerPrefix(n *sitter.Node) {
	if n.Type() != "expression_statement" {
		return
	}
	as := n.NamedChild(0)
	if as == nil || as.Type() != "assignment" {
		return
	}
	left, right := field(as, "left"), field(as, "right")
	if left == nil || left.Type() != "identifier" || right == nil || right.Type() != "call" {
		return
	}
	for _, a := range namedChildren(field(right, "arguments")) {
		if a.Type() != "keyword_argument" {
			continue
		}
		k := text(field(a, "name"), pf.src)
		if v := field(a, "value"); (k == "prefix" || k == "url_prefix") && v != nil && v.Type() == "string" {
			pf.prefixes[text(left, pf.src)] = pyString(v, pf.src)
		}
	}
}

// mainGuard indexes `if __name__ == "__main__":` as the module's entry point.
func (pf *pyFile) mainGuard(n *sitter.Node) {
	if n.Type() != "if_statement" {
		return
	}
	cond := strings.Join(strings.Fields(text(field(n, "condition"), pf.src)), "")
	if cond != `__name__=="__main__"` && cond != `__name__=='__main__'` {
		return
	}
	pf.add(n, "__main__", pf.module+".__main__", "entry point", `if __name__ == "__main__"`, "Script entry point of "+pf.module+".")
	pf.calls(n, field(n, "consequence"), pf.module+".__main__", "")
}

func (pf *pyFile) calls(def, body *sitter.Node, caller, class string) {
	locals := map[string]string{}
	for _, p := range namedChildren(field(def, "parameters")) {
		if p.Type() == "typed_parameter" || p.Type() == "typed_default_parameter" {
			var pname string
			if nn := field(p, "name"); nn != nil {
				pname = text(nn, pf.src)
			} else if first := p.NamedChild(0); first != nil {
				pname = text(first, pf.src)
			}
			if ref := pf.annotationRef(field(p, "type")); ref != "" && pname != "" {
				locals[pname] = ref
			}
		}
	}
	// classRef is the reference for a constructor call like Item(...).
	classRef := func(call *sitter.Node) string {
		if call == nil || call.Type() != "call" {
			return ""
		}
		fn := field(call, "function")
		if fn == nil || (fn.Type() != "identifier" && fn.Type() != "attribute") {
			return ""
		}
		name := text(fn, pf.src)
		if !isCapitalized(name[strings.LastIndex(name, ".")+1:]) {
			return ""
		}
		return pf.nameRef(name)
	}

	walk(body, func(n *sitter.Node) bool {
		switch n.Type() {
		case "function_definition", "class_definition":
			return n == body // nested definitions are not indexed separately
		case "assignment":
			left, right := field(n, "left"), field(n, "right")
			ref := pf.annotationRef(field(n, "type"))
			if ref == "" {
				ref = classRef(right)
			}
			if ref == "" && right != nil && right.Type() == "identifier" {
				ref = locals[text(right, pf.src)] // self.repo = repo (typed param)
			}
			if ref == "" || left == nil {
				return true
			}
			switch left.Type() {
			case "identifier":
				locals[text(left, pf.src)] = ref
			case "attribute":
				if obj := field(left, "object"); class != "" && obj != nil && text(obj, pf.src) == "self" {
					pf.res.Fields = append(pf.res.Fields, store.Field{
						Owner: class, Name: text(field(left, "attribute"), pf.src), TypeRef: ref,
					})
				}
			}
		case "call":
			pf.call(n, caller, class, locals)
		}
		return true
	})
}

// modulePath returns the full module path of a dotted attribute chain
// whose root is an imported name, such as "pkg.models" for pkg.models
// after `import pkg`, or "" if the chain is anything else (an attribute
// of a local value, a call result, an index).
func (pf *pyFile) modulePath(n *sitter.Node) string {
	var parts []string
	for n.Type() == "attribute" {
		parts = append([]string{text(field(n, "attribute"), pf.src)}, parts...)
		n = field(n, "object")
		if n == nil {
			return ""
		}
	}
	if n.Type() != "identifier" {
		return ""
	}
	root, ok := pf.imports[text(n, pf.src)]
	if !ok {
		return ""
	}
	return root + "." + strings.Join(parts, ".")
}

func (pf *pyFile) call(n *sitter.Node, caller, class string, locals map[string]string) {
	fn := field(n, "function")
	if fn == nil {
		return
	}
	rc := store.RawCall{Caller: caller, Line: line(n)}
	switch fn.Type() {
	case "identifier":
		name := text(fn, pf.src)
		if full, ok := pf.imports[name]; ok {
			i := strings.LastIndex(full, ".")
			if i < 0 {
				return
			}
			rc.Form, rc.A, rc.B = store.CallImport, full[:i], full[i+1:]
		} else {
			rc.Form, rc.A = store.CallQualified, pf.module+"."+name
		}
	case "attribute":
		attr := text(field(fn, "attribute"), pf.src)
		obj := field(fn, "object")
		objText := text(obj, pf.src)
		switch {
		case obj.Type() == "identifier" && (objText == "self" || objText == "cls") && class != "":
			rc.Form, rc.A, rc.B = store.CallQualified, class+"."+attr, attr
		case obj.Type() == "identifier" && locals[objText] != "":
			rc.Form, rc.A, rc.B = store.CallTyped, locals[objText], attr
		case obj.Type() == "identifier" && pf.imports[objText] != "":
			rc.Form, rc.A, rc.B = store.CallImport, pf.imports[objText], attr
		case obj.Type() == "attribute" && class != "" && text(field(obj, "object"), pf.src) == "self":
			rc.Form, rc.A, rc.B, rc.C = store.CallField, class, text(field(obj, "attribute"), pf.src), attr
		case obj.Type() == "call" && class != "" && text(field(obj, "function"), pf.src) == "super":
			// super().m() and super(Cls, self).m(): m on a base class.
			rc.Form, rc.A, rc.B = store.CallSuper, class, attr
		case obj.Type() == "attribute" && pf.modulePath(obj) != "":
			// pkg.models.Response(): a module path rooted at an import.
			rc.Form, rc.A, rc.B = store.CallImport, pf.modulePath(obj), attr
		default:
			rc.Form, rc.B = store.CallMethod, attr
		}
	default:
		return
	}
	pf.res.Calls = append(pf.res.Calls, rc)
}
