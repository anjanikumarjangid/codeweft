package indexer

import (
	"strings"

	sitter "github.com/smacker/go-tree-sitter"

	"github.com/anjanikumarjangid/codeweft/internal/store"
)

// gRPC, Python (grpcio generated code).
//
// A service is provided by a class subclassing XServicer: its public
// methods are the service's methods, named as in the .proto. A method is
// used by a call on a stub made by XStub(channel), held in a local
// variable or a self attribute. The stub's class must come from a module
// whose name says gRPC (orders_pb2_grpc), so other classes named ...Stub
// are not taken for stubs.

// grpcServicer reports the service a class implements: the X of the
// XServicer it subclasses.
func (pf *pyFile) grpcServicer(def *sitter.Node) string {
	for _, b := range namedChildren(field(def, "superclasses")) {
		if b.Type() != "identifier" && b.Type() != "attribute" {
			continue
		}
		t := text(b, pf.src)
		t = t[strings.LastIndex(t, ".")+1:]
		if svc, ok := strings.CutSuffix(t, "Servicer"); ok && svc != "" {
			return svc
		}
	}
	return ""
}

// grpcStubCall reports the service of a stub created by call:
// orders_pb2_grpc.OrderServiceStub(channel), or OrderServiceStub(channel)
// imported from such a module.
func (pf *pyFile) grpcStubCall(call *sitter.Node) string {
	if call == nil || call.Type() != "call" {
		return ""
	}
	fn := text(field(call, "function"), pf.src)
	root, _, _ := strings.Cut(fn, ".")
	cls := fn[strings.LastIndex(fn, ".")+1:]
	svc, ok := strings.CutSuffix(cls, "Stub")
	if !ok || svc == "" || !strings.Contains(pf.imports[root], "grpc") {
		return ""
	}
	return svc
}

// grpcClassStubs collects the self attributes a class sets to a stub in
// any of its methods: self.orders = orders_pb2_grpc.OrderServiceStub(channel).
func (pf *pyFile) grpcClassStubs(body *sitter.Node) map[string]string {
	stubs := map[string]string{}
	walk(body, func(n *sitter.Node) bool {
		if n.Type() == "class_definition" && n != body {
			return false
		}
		if n.Type() != "assignment" {
			return true
		}
		left := field(n, "left")
		if left == nil || left.Type() != "attribute" || text(field(left, "object"), pf.src) != "self" {
			return true
		}
		if svc := pf.grpcStubCall(field(n, "right")); svc != "" {
			stubs[text(field(left, "attribute"), pf.src)] = svc
		}
		return true
	})
	return stubs
}

// grpcCall recognizes a call on a stub: stub.GetOrder(request) for a local
// stub, self.orders.GetOrder(request) for an attribute.
func (pf *pyFile) grpcCall(n *sitter.Node, caller string, locals map[string]string) (store.Contract, bool) {
	fn := field(n, "function")
	if fn == nil || fn.Type() != "attribute" {
		return store.Contract{}, false
	}
	obj, method := field(fn, "object"), text(field(fn, "attribute"), pf.src)
	svc := ""
	switch {
	case obj == nil:
	case obj.Type() == "identifier":
		svc = locals[text(obj, pf.src)]
	case obj.Type() == "attribute" && text(field(obj, "object"), pf.src) == "self":
		svc = pf.stubs[text(field(obj, "attribute"), pf.src)]
	}
	if svc == "" {
		return store.Contract{}, false
	}
	return store.Contract{Kind: store.ContractGRPC, Role: store.RoleUses, Name: svc + "/" + method,
		Symbol: caller, Line: line(n), Exact: true}, true
}
