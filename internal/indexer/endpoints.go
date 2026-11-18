package indexer

import (
	"go/ast"
	"go/token"
	"regexp"
	"strconv"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"

	"github.com/anjanikumarjangid/codeweft/internal/store"
)

var httpMethods = map[string]string{
	"GET": "GET", "POST": "POST", "PUT": "PUT", "PATCH": "PATCH", "DELETE": "DELETE",
	"HEAD": "HEAD", "OPTIONS": "OPTIONS", "Any": "ANY", "ANY": "ANY",
	"Get": "GET", "Post": "POST", "Put": "PUT", "Patch": "PATCH", "Delete": "DELETE",
	"Head": "HEAD", "Options": "OPTIONS",
}

func joinRoute(prefix, p string) string {
	if prefix == "" {
		return p
	}
	if p == "" || p == "/" {
		return prefix
	}
	return strings.TrimSuffix(prefix, "/") + "/" + strings.TrimPrefix(p, "/")
}

// goFramework names the HTTP router a file uses, from its imports.
func goFramework(aliases map[string]string) string {
	for _, p := range aliases {
		switch {
		case strings.Contains(p, "gin-gonic/gin"):
			return "gin"
		case strings.Contains(p, "labstack/echo"):
			return "echo"
		case strings.Contains(p, "go-chi/chi"):
			return "chi"
		case strings.Contains(p, "gorilla/mux"):
			return "gorilla/mux"
		case strings.Contains(p, "gofiber/fiber"):
			return "fiber"
		}
	}
	return "net/http"
}

func stringLit(e ast.Expr) (string, bool) {
	bl, ok := e.(*ast.BasicLit)
	if !ok || bl.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(bl.Value)
	return s, err == nil
}

// goEndpoints finds route registrations such as http.HandleFunc("GET /x", h),
// mux.Handle("/x", h), r.GET("/x", h) (gin, echo, fiber) and r.Get("/x", h)
// (chi), following simple group prefixes: api := r.Group("/api").
func goEndpoints(fset *token.FileSet, f *ast.File, qual string, aliases map[string]string) []store.Endpoint {
	fw := goFramework(aliases)
	var out []store.Endpoint
	for _, decl := range f.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			continue
		}
		prefixes := map[string]string{} // group variable -> path prefix
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			if as, ok := n.(*ast.AssignStmt); ok && len(as.Lhs) == 1 && len(as.Rhs) == 1 {
				if call, ok := as.Rhs[0].(*ast.CallExpr); ok {
					if sel, ok := call.Fun.(*ast.SelectorExpr); ok && (sel.Sel.Name == "Group" || sel.Sel.Name == "Route") && len(call.Args) > 0 {
						if p, ok := stringLit(call.Args[0]); ok {
							base := ""
							if x, ok := sel.X.(*ast.Ident); ok {
								base = prefixes[x.Name]
							}
							if id, ok := as.Lhs[0].(*ast.Ident); ok {
								prefixes[id.Name] = joinRoute(base, p)
							}
						}
					}
				}
			}
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) < 2 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pattern, ok := stringLit(call.Args[0])
			if !ok {
				return true
			}
			method := ""
			switch sel.Sel.Name {
			case "HandleFunc", "Handle":
				method = "ANY"
				if m, rest, found := strings.Cut(pattern, " "); found && httpMethods[m] != "" {
					method, pattern = httpMethods[m], strings.TrimSpace(rest)
				}
			default:
				method = httpMethods[sel.Sel.Name]
			}
			if method == "" || !strings.HasPrefix(pattern, "/") {
				return true
			}
			prefix := ""
			if x, ok := sel.X.(*ast.Ident); ok {
				prefix = prefixes[x.Name]
			}
			out = append(out, store.Endpoint{
				Method: method, Path: joinRoute(prefix, pattern),
				Handler:   handlerNameOf(call.Args[len(call.Args)-1], qual, aliases),
				Framework: fw, Line: fset.Position(call.Pos()).Line,
			})
			return true
		})
	}
	return out
}

// handlerNameOf names a route handler expression: a function, a method
// value (h.createUser), or the last argument of a wrapper (http.HandlerFunc(h)).
func handlerNameOf(e ast.Expr, qual string, aliases map[string]string) string {
	switch t := e.(type) {
	case *ast.CallExpr:
		if len(t.Args) > 0 {
			return handlerNameOf(t.Args[len(t.Args)-1], qual, aliases)
		}
	case *ast.Ident:
		return qual + "." + t.Name
	case *ast.SelectorExpr:
		if x, ok := t.X.(*ast.Ident); ok {
			if p, ok := aliases[x.Name]; ok {
				return p + "." + t.Sel.Name
			}
			return x.Name + "." + t.Sel.Name
		}
	}
	return ""
}

var requestMethod = regexp.MustCompile(`RequestMethod\.([A-Z]+)`)

// javaRoute reads Spring (@GetMapping, @RequestMapping, ...) and JAX-RS
// (@Path with @GET, ...) annotations from a modifiers node.
func javaRoute(mods *sitter.Node, src []byte) (method, path, framework string, ok bool) {
	for _, a := range namedChildren(mods) {
		if a.Type() != "annotation" && a.Type() != "marker_annotation" {
			continue
		}
		name := text(field(a, "name"), src)
		name = name[strings.LastIndex(name, ".")+1:]
		args := field(a, "arguments")
		first := ""
		walk(args, func(n *sitter.Node) bool {
			if first == "" && n.Type() == "string_literal" {
				first = strings.Trim(text(n, src), `"`)
			}
			return first == ""
		})
		switch name {
		case "GetMapping", "PostMapping", "PutMapping", "PatchMapping", "DeleteMapping":
			method, path, framework, ok = strings.ToUpper(strings.TrimSuffix(name, "Mapping")), first, "spring", true
		case "RequestMapping":
			method, path, framework, ok = "ANY", first, "spring", true
			if m := requestMethod.FindStringSubmatch(text(args, src)); m != nil {
				method = m[1]
			}
		case "Path":
			path, framework, ok = first, "jax-rs", true
			if method == "" {
				method = "ANY"
			}
		case "GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS":
			method, framework, ok = name, "jax-rs", true
		}
	}
	return
}

// pythonRoute reads Flask/FastAPI decorators: @app.route("/x", methods=[...]),
// @app.get("/x"), @router.post("/x").
func pythonRoute(dec *sitter.Node, src []byte, prefixes map[string]string) (methods []string, path, obj string, ok bool) {
	call := dec.NamedChild(0)
	if call == nil || call.Type() != "call" {
		return nil, "", "", false
	}
	fn := field(call, "function")
	if fn == nil || fn.Type() != "attribute" {
		return nil, "", "", false
	}
	obj = text(field(fn, "object"), src)
	attr := text(field(fn, "attribute"), src)
	args := field(call, "arguments")
	for _, a := range namedChildren(args) {
		if a.Type() == "string" && path == "" {
			path = pyString(a, src)
		}
		if a.Type() == "keyword_argument" && text(field(a, "name"), src) == "methods" {
			walk(field(a, "value"), func(n *sitter.Node) bool {
				if n.Type() == "string" {
					methods = append(methods, strings.ToUpper(pyString(n, src)))
				}
				return true
			})
		}
	}
	if path == "" || !strings.HasPrefix(path, "/") {
		return nil, "", "", false
	}
	switch attr {
	case "route", "api_route":
		if len(methods) == 0 {
			methods = []string{"GET"}
		}
	case "get", "post", "put", "patch", "delete", "head", "options":
		methods = []string{strings.ToUpper(attr)}
	case "websocket":
		methods = []string{"WEBSOCKET"}
	default:
		return nil, "", "", false
	}
	return methods, joinRoute(prefixes[obj], path), obj, true
}

func pyString(n *sitter.Node, src []byte) string {
	var b strings.Builder
	for _, c := range namedChildren(n) {
		if c.Type() == "string_content" {
			b.WriteString(text(c, src))
		}
	}
	return b.String()
}
