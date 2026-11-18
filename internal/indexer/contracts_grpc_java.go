package indexer

import (
	"regexp"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"

	"github.com/anjanikumarjangid/codewiki/internal/store"
)

// gRPC, Java (grpc-java generated code).
//
// A service is provided by a class extending XGrpc.XImplBase: its public
// methods are the service's methods, named with their first letter
// upper-cased as in the .proto (getOrder -> GetOrder). A method is used by
// a call on a stub: one made by XGrpc.newBlockingStub, newStub or
// newFutureStub, or a field, local or parameter of a stub type
// (XGrpc.XBlockingStub).

var (
	grpcImplBase = regexp.MustCompile(`(\w+)ImplBase$`)
	grpcStubType = regexp.MustCompile(`(\w+)Grpc\.\w*Stub$`)
)

// grpcService reports the service a class implements: the X of the
// XImplBase it extends.
func (jf *javaFile) grpcService(n *sitter.Node) string {
	sc := field(n, "superclass")
	if sc == nil {
		return ""
	}
	t := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(text(sc, jf.src)), "extends"))
	if i := strings.IndexByte(t, '<'); i >= 0 {
		t = t[:i]
	}
	if m := grpcImplBase.FindStringSubmatch(t); m != nil {
		return m[1]
	}
	return ""
}

// grpcStub returns "G" and the service when a type is a gRPC stub type, or
// a value creates a stub.
func (jf *javaFile) grpcStub(typ, value *sitter.Node) string {
	if typ != nil {
		if m := grpcStubType.FindStringSubmatch(strings.TrimSpace(text(typ, jf.src))); m != nil {
			return "G" + m[1]
		}
	}
	if value != nil && value.Type() == "method_invocation" {
		switch text(field(value, "name"), jf.src) {
		case "newBlockingStub", "newStub", "newFutureStub", "newBlockingV2Stub":
			if o := text(field(value, "object"), jf.src); strings.HasSuffix(o, "Grpc") {
				o = o[strings.LastIndex(o, ".")+1:]
				return "G" + strings.TrimSuffix(o, "Grpc")
			}
		}
	}
	return ""
}

// grpcCall recognizes a call on a gRPC stub (stub.getOrder(request),
// this.stub.getOrder(request)).
func (jf *javaFile) grpcCall(n *sitter.Node, stubs map[string]string, caller string) (store.Contract, bool) {
	obj := field(n, "object")
	name := text(field(n, "name"), jf.src)
	x := ""
	switch {
	case obj == nil:
		return store.Contract{}, false
	case obj.Type() == "identifier":
		x = text(obj, jf.src)
	case obj.Type() == "field_access" && field(obj, "object") != nil && field(obj, "object").Type() == "this":
		x = text(field(obj, "field"), jf.src)
	}
	svc, ok := strings.CutPrefix(stubs[x], "G")
	if !ok || strings.HasPrefix(name, "with") || name == "getChannel" || name == "getCallOptions" {
		return store.Contract{}, false // stub configuration: withDeadlineAfter, withCallCredentials
	}
	return store.Contract{Kind: store.ContractGRPC, Role: store.RoleUses, Name: svc + "/" + capitalize(name),
		Symbol: caller, Line: line(n), Exact: true}, true
}
