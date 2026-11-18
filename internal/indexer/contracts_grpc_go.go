package indexer

import (
	"go/ast"
	"go/token"
	"regexp"
	"strings"

	"github.com/anjanikumarjangid/codeweft/internal/store"
)

// gRPC, Go (google.golang.org/grpc and protoc-gen-go-grpc code).
//
// A service is provided by the struct registered with RegisterXServer(s,
// impl), or a struct embedding UnimplementedXServer (or UnsafeXServer):
// its exported methods are the service's methods. A method is used by a
// call on a client made by NewXClient(conn), or on a variable, parameter
// or struct field of type XClient. Contracts are named "Service/Method";
// the store qualifies them with the proto package and keeps only the
// methods the service declares, when its .proto file is indexed.

var (
	grpcRegister = regexp.MustCompile(`^Register(\w+)Server$`)
	grpcNew      = regexp.MustCompile(`^New(\w+)Client$`)
	grpcServer   = regexp.MustCompile(`^(?:Unimplemented|Unsafe)(\w+)Server$`)
	grpcClient   = regexp.MustCompile(`^(\w+)Client$`)
)

// usesGRPC reports whether a file imports a google.golang.org/grpc package.
func usesGRPC(aliases map[string]string) bool {
	for _, p := range aliases {
		if p == "google.golang.org/grpc" || strings.HasPrefix(p, "google.golang.org/grpc/") {
			return true
		}
	}
	return false
}

// typeName is the name of a type expression without pointer, package and
// type arguments: *pb.OrderServiceClient -> OrderServiceClient.
func typeName(e ast.Expr) string {
	for {
		switch t := e.(type) {
		case *ast.StarExpr:
			e = t.X
		case *ast.ParenExpr:
			e = t.X
		case *ast.IndexExpr:
			e = t.X
		case *ast.SelectorExpr:
			return t.Sel.Name
		case *ast.Ident:
			return t.Name
		default:
			return ""
		}
	}
}

// goGRPCServer reports the service a struct implements by embedding its
// generated UnimplementedXServer.
func goGRPCServer(st *ast.StructType) string {
	for _, f := range st.Fields.List {
		if len(f.Names) == 0 {
			if m := grpcServer.FindStringSubmatch(typeName(f.Type)); m != nil {
				return m[1]
			}
		}
	}
	return ""
}

// goGRPCClientFields maps the struct fields of a file declared with a
// gRPC client type (orders pb.OrderServiceClient) to the service. A field
// name used with two services is left out.
func goGRPCClientFields(f *ast.File) map[string]string {
	fields := map[string]string{}
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.TYPE {
			continue
		}
		for _, spec := range gd.Specs {
			st, ok := spec.(*ast.TypeSpec).Type.(*ast.StructType)
			if !ok {
				continue
			}
			for _, fl := range st.Fields.List {
				m := grpcClient.FindStringSubmatch(typeName(fl.Type))
				if m == nil || m[1] == "" {
					continue
				}
				if _, ok := fl.Type.(*ast.SelectorExpr); !ok && !isPtrSelector(fl.Type) {
					continue // a client type of another package: generated code
				}
				for _, n := range fl.Names {
					if old, seen := fields[n.Name]; seen && old != m[1] {
						fields[n.Name] = ""
					} else {
						fields[n.Name] = m[1]
					}
				}
			}
		}
	}
	return fields
}

func isPtrSelector(e ast.Expr) bool {
	s, ok := e.(*ast.StarExpr)
	if !ok {
		return false
	}
	_, ok = s.X.(*ast.SelectorExpr)
	return ok
}

// goGRPCCalls finds a function body's gRPC server registrations and client
// calls. clientFields comes from goGRPCClientFields. Calls on a client
// made in a file that does not import grpc are recorded as not exact: the
// store keeps them only when an indexed .proto declares the service.
func goGRPCCalls(fset *token.FileSet, body *ast.BlockStmt, caller, qual string, aliases map[string]string,
	enc *goEncoder, clientFields map[string]string) []store.Contract {
	grpc := usesGRPC(aliases)
	// service is the gRPC service of the client an expression holds.
	var service func(e ast.Expr, depth int) string
	service = func(e ast.Expr, depth int) string {
		switch t := e.(type) {
		case *ast.ParenExpr:
			return service(t.X, depth)
		case *ast.CallExpr:
			name := ""
			switch f := t.Fun.(type) {
			case *ast.SelectorExpr:
				name = f.Sel.Name
			case *ast.Ident:
				name = f.Name
			}
			if m := grpcNew.FindStringSubmatch(name); m != nil {
				return m[1]
			}
		case *ast.Ident:
			l, ok := enc.lookup(t.Name, t.Pos())
			if !ok || depth > 2 {
				return ""
			}
			if l.val != nil {
				return service(l.val, depth+1)
			}
			if l.typ != nil {
				if _, isSel := l.typ.(*ast.SelectorExpr); isSel || isPtrSelector(l.typ) {
					if m := grpcClient.FindStringSubmatch(typeName(l.typ)); m != nil {
						return m[1]
					}
				}
			}
		case *ast.SelectorExpr:
			// s.orders.GetOrder: a field of a struct of this file.
			if _, ok := t.X.(*ast.Ident); ok {
				return clientFields[t.Sel.Name]
			}
		}
		return ""
	}

	var out []store.Contract
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		line := fset.Position(call.Pos()).Line
		name := ""
		var recv ast.Expr
		switch f := call.Fun.(type) {
		case *ast.SelectorExpr:
			name, recv = f.Sel.Name, f.X
		case *ast.Ident:
			name = f.Name
		}
		if m := grpcRegister.FindStringSubmatch(name); m != nil && len(call.Args) == 2 {
			if t := goStructValue(call.Args[1], enc, 0); t != nil {
				if ref := goTypeRef(t, qual, aliases); ref != "" {
					out = append(out, store.Contract{Kind: store.ContractGRPC, Role: store.RoleProvides, Name: m[1] + "/",
						Symbol: store.StructMethods + ref, Line: line, Exact: true})
				}
			}
			return true
		}
		if recv == nil || !ast.IsExported(name) {
			return true
		}
		if pkg, ok := recv.(*ast.Ident); ok && enc.isPackage(pkg.Name, pkg.Pos()) {
			return true
		}
		if svc := service(recv, 0); svc != "" {
			out = append(out, store.Contract{Kind: store.ContractGRPC, Role: store.RoleUses, Name: svc + "/" + name,
				Symbol: caller, Line: line, Exact: grpc})
		}
		return true
	})
	return out
}
