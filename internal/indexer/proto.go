package indexer

import (
	"path"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/protobuf"

	"github.com/anjanikumarjangid/codewiki/internal/store"
)

// ParseProto extracts the messages, enums, services and RPC methods of a
// Protocol Buffers file. Names are qualified with the proto package, as
// gRPC names them (shop.orders.v1.OrderService.GetOrder). The services are
// what gRPC servers implement and clients call: the store uses the RPC
// methods to name and check the gRPC contracts found in code.
func ParseProto(rel string, src []byte) (*FileResult, error) {
	root, done, err := parseTree(protobuf.GetLanguage(), src)
	if err != nil {
		return nil, err
	}
	defer done()
	pkg := ""
	for _, n := range namedChildren(root) {
		if n.Type() == "package" {
			pkg = strings.TrimSpace(text(n.NamedChild(0), src))
		}
	}
	qual := pkg
	if qual == "" {
		qual = strings.TrimSuffix(path.Base(rel), ".proto")
	}
	res := &FileResult{File: store.File{Path: rel, Dir: path.Dir(rel), Lang: "proto", Package: pkg, Qual: qual,
		Doc: protoDoc(firstNamed(root, "package", "syntax"), src)}}
	add := func(n *sitter.Node, name, qualified, kind, sig string) {
		res.Symbols = append(res.Symbols, store.Symbol{
			File: rel, Lang: "proto", Package: pkg, Name: name, Qualified: qualified, Kind: kind,
			Signature: sig, Doc: protoDoc(n, src), StartLine: line(n), EndLine: endLine(n),
			Exported: true, Body: text(n, src),
		})
	}
	var decls func(nodes []*sitter.Node, prefix string)
	decls = func(nodes []*sitter.Node, prefix string) {
		for _, n := range nodes {
			switch n.Type() {
			case "message":
				name := text(firstNamed(n, "message_name"), src)
				add(n, name, prefix+"."+name, "message", "message "+name)
				decls(namedChildren(firstNamed(n, "message_body")), prefix+"."+name)
			case "enum":
				name := text(firstNamed(n, "enum_name"), src)
				add(n, name, prefix+"."+name, "enum", "enum "+name)
			case "service":
				name := text(firstNamed(n, "service_name"), src)
				svc := prefix + "." + name
				add(n, name, svc, "service", "service "+name)
				for _, r := range namedChildren(n) {
					if r.Type() != "rpc" {
						continue
					}
					m := text(firstNamed(r, "rpc_name"), src)
					add(r, m, svc+"."+m, "rpc", protoRPCSignature(r, src))
				}
			}
		}
	}
	decls(namedChildren(root), qual)
	return res, nil
}

// firstNamed returns the first named child of n of one of the types.
func firstNamed(n *sitter.Node, types ...string) *sitter.Node {
	for _, c := range namedChildren(n) {
		for _, t := range types {
			if c.Type() == t {
				return c
			}
		}
	}
	return nil
}

// protoRPCSignature is an RPC declaration without its options block:
// "rpc GetOrder(GetOrderRequest) returns (Order)".
func protoRPCSignature(r *sitter.Node, src []byte) string {
	s := text(r, src)
	if i := strings.IndexByte(s, '{'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSuffix(strings.Join(strings.Fields(s), " "), ";")
}

// protoDoc is the comment block directly above a declaration.
func protoDoc(n *sitter.Node, src []byte) string {
	if n == nil {
		return ""
	}
	var lines []string
	next := n
	for p := n.PrevSibling(); p != nil && p.Type() == "comment"; p = p.PrevSibling() {
		if int(next.StartPoint().Row)-int(p.EndPoint().Row) > 1 {
			break // a blank line: not this declaration's comment
		}
		c := strings.TrimSpace(text(p, src))
		c = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(c, "//"), "/*"), "*/"))
		lines = append([]string{c}, lines...)
		next = p
	}
	return strings.Join(lines, "\n")
}
