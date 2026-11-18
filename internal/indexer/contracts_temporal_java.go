package indexer

import (
	"strings"

	sitter "github.com/smacker/go-tree-sitter"

	"github.com/anjanikumarjangid/codeweft/internal/store"
)

// Temporal, Java SDK (io.temporal).
//
// Provides: the methods of an @WorkflowInterface marked @WorkflowMethod
// (the workflow is named after the interface unless the annotation names
// it), every method of an @ActivityInterface (named after the method with
// its first letter upper-cased, after the interface's namePrefix, unless
// @ActivityMethod names it), and the classes registered with a worker
// (registerWorkflowImplementationTypes, registerActivitiesImplementations),
// for interfaces from outside the repository. The store reports an
// interface method at its implementation.
//
// Uses: newWorkflowStub and newChildWorkflowStub of an interface, their
// untyped forms given a workflow name, and calls on an activity stub
// (Workflow.newActivityStub, newLocalActivityStub, or an untyped stub's
// execute("Name")).

// annotation returns the annotation named name among a declaration's
// modifiers.
func (jf *javaFile) annotation(mods *sitter.Node, name string) (*sitter.Node, bool) {
	for _, a := range namedChildren(mods) {
		if a.Type() != "annotation" && a.Type() != "marker_annotation" {
			continue
		}
		n := text(field(a, "name"), jf.src)
		if n[strings.LastIndex(n, ".")+1:] == name {
			return a, true
		}
	}
	return nil, false
}

// annotationArg returns the value of an annotation's element as far as the
// source shows it ("" when absent or not written out).
func (jf *javaFile) annotationArg(a *sitter.Node, key string) string {
	for _, p := range namedChildren(field(a, "arguments")) {
		if p.Type() == "element_value_pair" && text(field(p, "key"), jf.src) == key {
			if v := jf.template(field(p, "value")); !strings.Contains(v, "*") {
				return v
			}
		}
	}
	return ""
}

// temporalInterface reports whether a type is a Temporal workflow or
// activity interface, with an activity interface's name prefix.
func (jf *javaFile) temporalInterface(n *sitter.Node) (kind, prefix string) {
	if n.Type() != "interface_declaration" {
		return "", ""
	}
	mods := modifiersOf(n)
	if _, ok := jf.annotation(mods, "WorkflowInterface"); ok {
		return store.ContractTemporalWorkflow, ""
	}
	if a, ok := jf.annotation(mods, "ActivityInterface"); ok {
		return store.ContractTemporalActivity, jf.annotationArg(a, "namePrefix")
	}
	return "", ""
}

// temporalMethod returns the contracts a method of a Temporal interface
// provides: under the name Temporal gives it and, when that differs, under
// the name a Java client's stub of the interface refers to it by.
func (jf *javaFile) temporalMethod(m *sitter.Node, kind, prefix, iface, mname, mq string) []store.Contract {
	mods := modifiersOf(m)
	var name, alias string
	switch kind {
	case store.ContractTemporalWorkflow:
		a, ok := jf.annotation(mods, "WorkflowMethod")
		if !ok {
			return nil // a signal, query or update method
		}
		name, alias = jf.annotationArg(a, "name"), iface
	case store.ContractTemporalActivity:
		if hasModifier(m, jf.src, "static") {
			return nil
		}
		alias = capitalize(mname)
		name = prefix + alias
		if a, ok := jf.annotation(mods, "ActivityMethod"); ok {
			if n := jf.annotationArg(a, "name"); n != "" {
				name = n
			}
		}
	}
	if name == "" {
		name = alias
	}
	var out []store.Contract
	for _, n := range []string{name, alias} {
		if n != "" && (len(out) == 0 || out[0].Name != n) {
			out = append(out, store.Contract{Kind: kind, Role: store.RoleProvides, Name: n, Symbol: mq, Line: line(m), Exact: true})
		}
	}
	return out
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// classLiteral returns the simple name of the type in T.class.
func (jf *javaFile) classLiteral(n *sitter.Node) (string, bool) {
	if n == nil || n.Type() != "class_literal" {
		return "", false
	}
	t := strings.TrimSuffix(strings.TrimSpace(text(n, jf.src)), ".class")
	return t[strings.LastIndex(t, ".")+1:], true
}

// classRef is the type reference of the type in T.class.
func (jf *javaFile) classRef(n *sitter.Node) string {
	if cs := namedChildren(n); len(cs) > 0 {
		return jf.typeRef(cs[0])
	}
	return ""
}

// temporalStub records what a variable initialized by value holds: a typed
// activity stub ("A", the interface name, "|" and its type reference) or an
// untyped one ("U"), a worker ("W" and its task queue).
func (jf *javaFile) temporalStub(value *sitter.Node) string {
	if value == nil || value.Type() != "method_invocation" {
		return ""
	}
	args := namedChildren(field(value, "arguments"))
	switch text(field(value, "name"), jf.src) {
	case "newActivityStub", "newLocalActivityStub":
		if len(args) > 0 {
			if t, ok := jf.classLiteral(args[0]); ok {
				return "A" + t + "|" + jf.classRef(args[0])
			}
		}
	case "newUntypedActivityStub", "newUntypedLocalActivityStub":
		return "U"
	case "newWorker":
		if len(args) > 0 {
			if q := jf.template(args[0]); !strings.Contains(q, "*") {
				return "W" + q
			}
			return "W"
		}
	}
	return ""
}

// taskQueue finds a task queue set in an options expression:
// WorkflowOptions.newBuilder().setTaskQueue("orders").build().
func (jf *javaFile) taskQueue(n *sitter.Node) string {
	q := ""
	walk(n, func(c *sitter.Node) bool {
		if c.Type() == "method_invocation" && text(field(c, "name"), jf.src) == "setTaskQueue" {
			if args := namedChildren(field(c, "arguments")); len(args) == 1 {
				if v := jf.template(args[0]); !strings.Contains(v, "*") {
					q = v
				}
			}
		}
		return q == ""
	})
	return q
}

// temporalCall recognizes a Temporal registration, workflow stub or
// activity call. stubs maps variables to what temporalStub found; locals
// and fields give the types of variables passed to registrations.
func (jf *javaFile) temporalCall(n *sitter.Node, stubs, locals, fields map[string]string, caller string) []store.Contract {
	name := text(field(n, "name"), jf.src)
	args := namedChildren(field(n, "arguments"))
	obj := field(n, "object")
	objName := ""
	if obj != nil && obj.Type() == "identifier" {
		objName = text(obj, jf.src)
	}
	l := line(n)
	use := func(kind, target, queue, ref string) []store.Contract {
		return []store.Contract{{Kind: kind, Role: store.RoleUses, Name: target, Symbol: caller, Line: l, Detail: queue,
			Exact: true, Target: ref}}
	}
	switch name {
	case "newWorkflowStub", "newChildWorkflowStub":
		if len(args) > 0 {
			if t, ok := jf.classLiteral(args[0]); ok {
				queue := ""
				if len(args) > 1 {
					queue = jf.taskQueue(args[1])
				}
				return use(store.ContractTemporalWorkflow, t, queue, jf.classRef(args[0]))
			}
		}
	case "newUntypedWorkflowStub", "newUntypedChildWorkflowStub":
		// newUntypedWorkflowStub(workflowType, options) starts a workflow;
		// (workflowId) and (workflowId, runId, type) attach to a running one.
		if len(args) == 0 || args[0].Type() != "string_literal" || (name == "newUntypedWorkflowStub" && len(args) != 2) {
			return nil
		}
		if t := jf.template(args[0]); t != "" && !strings.Contains(t, "*") {
			queue := ""
			if len(args) > 1 {
				queue = jf.taskQueue(args[1])
			}
			return use(store.ContractTemporalWorkflow, t, queue, "")
		}
	case "registerWorkflowImplementationTypes", "registerActivitiesImplementations":
		kind := store.ContractTemporalWorkflow
		if name == "registerActivitiesImplementations" {
			kind = store.ContractTemporalActivity
		}
		queue := strings.TrimPrefix(stubs[objName], "W")
		var out []store.Contract
		for _, a := range args {
			ref := ""
			switch a.Type() {
			case "class_literal":
				ref = jf.classRef(a)
			case "object_creation_expression":
				ref = jf.typeRef(field(a, "type"))
			case "identifier":
				ref = locals[text(a, jf.src)]
				if ref == "" {
					ref = fields[text(a, jf.src)]
				}
			}
			if ref != "" {
				out = append(out, store.Contract{Kind: kind, Role: store.RoleProvides, Symbol: store.ImplementationOf + ref,
					Line: l, Detail: queue, Exact: true})
			}
		}
		return out
	}
	switch stub := stubs[objName]; {
	case strings.HasPrefix(stub, "A"):
		_, ref, _ := strings.Cut(stub, "|")
		return use(store.ContractTemporalActivity, capitalize(name), "", ref)
	case stub == "U" && (name == "execute" || name == "executeAsync") && len(args) > 0:
		if t := jf.template(args[0]); args[0].Type() == "string_literal" && !strings.Contains(t, "*") {
			return use(store.ContractTemporalActivity, t, "", "")
		}
	}
	return nil
}
