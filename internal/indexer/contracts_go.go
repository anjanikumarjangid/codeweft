package indexer

import (
	"go/ast"
	"go/token"
	"regexp"
	"strings"

	"github.com/anjanikumarjangid/codeweft/internal/store"
)

// goStringConsts collects a file's package-level string constants and
// variables whose value is written out (const base = "/api"), for
// evaluating URLs and names built from them.
func goStringConsts(f *ast.File) map[string]string {
	consts := map[string]string{}
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || (gd.Tok != token.CONST && gd.Tok != token.VAR) {
			continue
		}
		for _, spec := range gd.Specs {
			vs := spec.(*ast.ValueSpec)
			for i, n := range vs.Names {
				if i < len(vs.Values) {
					if v := goTemplate(vs.Values[i], consts, nil); v != "" && !strings.Contains(v, "*") {
						consts[n.Name] = v
					}
				}
			}
		}
	}
	return consts
}

// fmtVerb matches a fmt formatting verb such as %s, %d, %v or %08x.
var fmtVerb = regexp.MustCompile(`%[-+# 0-9.]*[a-zA-Z]`)

// goTemplate evaluates a string expression as far as the source allows,
// writing "*" for each part it cannot see: literals and their
// concatenation, named string constants, fmt.Sprintf (each verb a "*"),
// path.Join and url.JoinPath.
func goTemplate(e ast.Expr, consts, aliases map[string]string) string {
	switch t := e.(type) {
	case *ast.BasicLit:
		if s, ok := stringLit(t); ok {
			return s
		}
	case *ast.ParenExpr:
		return goTemplate(t.X, consts, aliases)
	case *ast.BinaryExpr:
		if t.Op == token.ADD {
			return goTemplate(t.X, consts, aliases) + goTemplate(t.Y, consts, aliases)
		}
	case *ast.Ident:
		if v, ok := consts[t.Name]; ok {
			return v
		}
	case *ast.CallExpr:
		sel, ok := t.Fun.(*ast.SelectorExpr)
		if !ok {
			break
		}
		pkg, _ := sel.X.(*ast.Ident)
		if pkg == nil {
			break
		}
		path := aliases[pkg.Name]
		switch {
		case path == "os" && sel.Sel.Name == "Getenv" && len(t.Args) == 1:
			// An environment variable: resolved from configuration files.
			if k := goTemplate(t.Args[0], consts, aliases); !strings.ContainsAny(k, "*$") {
				return "${" + k + "}"
			}
		case path == "fmt" && sel.Sel.Name == "Sprintf" && len(t.Args) > 0:
			return fmtVerb.ReplaceAllString(goTemplate(t.Args[0], consts, aliases), "*")
		case (path == "path" && sel.Sel.Name == "Join") || (path == "net/url" && sel.Sel.Name == "JoinPath"):
			var parts []string
			for _, a := range t.Args {
				parts = append(parts, strings.Trim(goTemplate(a, consts, aliases), "/"))
			}
			return strings.Join(parts, "/")
		}
	}
	return "*"
}

// goHTTPMethod reads an HTTP method argument: "POST" or http.MethodPost.
func goHTTPMethod(e ast.Expr, aliases map[string]string) string {
	if s, ok := stringLit(e); ok {
		return strings.ToUpper(s)
	}
	if sel, ok := e.(*ast.SelectorExpr); ok {
		if pkg, ok := sel.X.(*ast.Ident); ok && aliases[pkg.Name] == "net/http" {
			return strings.ToUpper(strings.TrimPrefix(sel.Sel.Name, "Method"))
		}
	}
	return ""
}

// httpUse builds the contract of an HTTP client call to a URL template.
func httpUse(method, url, caller string, line int) (store.Contract, bool) {
	name := store.NormalizePath(url)
	if name == "" || name == "/" && !strings.Contains(url, "/") {
		return store.Contract{}, false // nothing of the path is known
	}
	return store.Contract{Kind: store.ContractHTTP, Role: store.RoleUses, Name: name, Method: method,
		Symbol: caller, Line: line, Detail: url, Exact: !strings.Contains(url, "*") && !strings.Contains(url, "${")}, true
}

// goHTTPClientCalls finds HTTP requests made with net/http in a function
// body: http.Get/Head/Post/PostForm(url), http.NewRequest(method, url, body),
// http.NewRequestWithContext(ctx, method, url, body), and the Get, Head, Post
// and PostForm methods of an *http.Client.
func goHTTPClientCalls(fset *token.FileSet, body *ast.BlockStmt, caller string, aliases, consts map[string]string, enc *goEncoder) []store.Contract {
	var out []store.Contract
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		name := sel.Sel.Name
		onHTTP := false // a function of net/http, or a method of an http.Client
		if pkg, ok := sel.X.(*ast.Ident); ok && enc.isPackage(pkg.Name, pkg.Pos()) {
			onHTTP = aliases[pkg.Name] == "net/http"
		} else if x := enc.encode(sel.X); x != nil {
			if t, ok := x.([]any); ok && len(t) >= 2 && t[0] == "t" {
				ts, _ := t[1].(string)
				ts = strings.TrimPrefix(ts, "*")
				if dot := strings.IndexByte(ts, '.'); dot > 0 && aliases[ts[:dot]] == "net/http" && ts[dot+1:] == "Client" {
					onHTTP = name != "NewRequest" && name != "NewRequestWithContext"
				}
			}
		}
		if !onHTTP {
			return true
		}
		line := fset.Position(call.Pos()).Line
		var method, url string
		switch {
		case (name == "Get" || name == "Head" || name == "Post" || name == "PostForm") && len(call.Args) > 0:
			method, url = strings.ToUpper(strings.TrimSuffix(name, "Form")), goTemplate(call.Args[0], consts, aliases)
		case name == "NewRequest" && len(call.Args) >= 2:
			method, url = goHTTPMethod(call.Args[0], aliases), goTemplate(call.Args[1], consts, aliases)
		case name == "NewRequestWithContext" && len(call.Args) >= 3:
			method, url = goHTTPMethod(call.Args[1], aliases), goTemplate(call.Args[2], consts, aliases)
		default:
			return true
		}
		if c, ok := httpUse(method, url, caller, line); ok {
			out = append(out, c)
		}
		return true
	})
	return out
}
