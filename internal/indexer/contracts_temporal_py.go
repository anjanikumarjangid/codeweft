package indexer

import (
	"strings"

	sitter "github.com/smacker/go-tree-sitter"

	"github.com/anjanikumarjangid/codeweft/internal/store"
)

// Temporal, Python SDK (temporalio).
//
// Provides: a class decorated @workflow.defn (named after the class unless
// name= says otherwise), reported at its @workflow.run method, and a
// function or method decorated @activity.defn (named after the function
// unless name= says otherwise).
//
// Uses: client.execute_workflow and start_workflow, and
// workflow.execute_child_workflow and start_child_workflow, given a
// workflow's run method (OrderWorkflow.run) or name; workflow.execute_activity,
// start_activity, their _local_ and _method forms, given an activity
// function or name. The task_queue argument is kept.

// temporalModule returns the temporalio module an imported name refers to
// ("workflow" for `from temporalio import workflow`), or "".
func (pf *pyFile) temporalModule(name string) string {
	if m, ok := strings.CutPrefix(pf.imports[name], "temporalio."); ok {
		return m
	}
	return ""
}

// temporalDefn reads a @workflow.defn or @activity.defn decorator: the
// module (workflow or activity), and the name= it gives, if any.
func (pf *pyFile) temporalDefn(dec *sitter.Node) (module, name string, ok bool) {
	e := dec.NamedChild(0)
	if e == nil {
		return "", "", false
	}
	var args *sitter.Node
	if e.Type() == "call" {
		e, args = field(e, "function"), field(e, "arguments")
	}
	if e == nil || e.Type() != "attribute" || text(field(e, "attribute"), pf.src) != "defn" {
		return "", "", false
	}
	obj := field(e, "object")
	if obj == nil || obj.Type() != "identifier" {
		return "", "", false
	}
	module = pf.temporalModule(text(obj, pf.src))
	if module != "workflow" && module != "activity" {
		return "", "", false
	}
	for _, a := range namedChildren(args) {
		if a.Type() == "keyword_argument" && text(field(a, "name"), pf.src) == "name" {
			if v := pf.template(field(a, "value")); !strings.Contains(v, "*") {
				name = v
			}
		}
	}
	return module, name, true
}

// decorators returns the decorators of a decorated definition.
func decorators(outer *sitter.Node) []*sitter.Node {
	var out []*sitter.Node
	if outer != nil && outer.Type() == "decorated_definition" {
		for _, d := range namedChildren(outer) {
			if d.Type() == "decorator" {
				out = append(out, d)
			}
		}
	}
	return out
}

// temporalProvides returns what a decorated definition provides under the
// name Temporal gives it and, when that differs, under its name in the code,
// which a client refers to it by (OrderWorkflow.run, charge).
func temporalProvides(kind, custom, codeName, symbol string, ln int) []store.Contract {
	out := []store.Contract{}
	for _, n := range []string{custom, codeName} {
		if n != "" && (len(out) == 0 || out[0].Name != n) {
			out = append(out, store.Contract{Kind: kind, Role: store.RoleProvides, Name: n, Symbol: symbol, Line: ln, Exact: true})
		}
	}
	return out
}

// pyWorkflowClass returns the contracts of a @workflow.defn class, at its
// @workflow.run method when it has one.
func (pf *pyFile) pyWorkflowClass(def, outer *sitter.Node, name, qual string) []store.Contract {
	for _, dec := range decorators(outer) {
		module, custom, ok := pf.temporalDefn(dec)
		if !ok || module != "workflow" {
			continue
		}
		symbol, ln := qual, line(def)
		for _, m := range namedChildren(field(def, "body")) {
			for _, md := range decorators(m) {
				if e := md.NamedChild(0); e != nil && e.Type() == "attribute" && text(field(e, "attribute"), pf.src) == "run" &&
					pf.temporalModule(text(field(e, "object"), pf.src)) == "workflow" {
					d := unwrapDecorated(m)
					symbol, ln = qual+"."+text(field(d, "name"), pf.src), line(d)
				}
			}
		}
		return temporalProvides(store.ContractTemporalWorkflow, custom, name, symbol, ln)
	}
	return nil
}

// pyActivity returns the contracts of an @activity.defn function or method.
func (pf *pyFile) pyActivity(def, outer *sitter.Node, name, qual string) []store.Contract {
	for _, dec := range decorators(outer) {
		if module, custom, ok := pf.temporalDefn(dec); ok && module == "activity" {
			return temporalProvides(store.ContractTemporalActivity, custom, name, qual, line(def))
		}
	}
	return nil
}

// pyTemporalStarts maps the methods starting workflows and activities to
// the kind they start; only the workflow-starting client methods may be
// called on any object (a Client).
var pyTemporalStarts = map[string]string{
	"execute_workflow": store.ContractTemporalWorkflow, "start_workflow": store.ContractTemporalWorkflow,
	"execute_child_workflow": store.ContractTemporalWorkflow, "start_child_workflow": store.ContractTemporalWorkflow,
	"execute_activity": store.ContractTemporalActivity, "start_activity": store.ContractTemporalActivity,
	"execute_local_activity": store.ContractTemporalActivity, "start_local_activity": store.ContractTemporalActivity,
	"execute_activity_method": store.ContractTemporalActivity, "start_activity_method": store.ContractTemporalActivity,
	"execute_local_activity_method": store.ContractTemporalActivity, "start_local_activity_method": store.ContractTemporalActivity,
}

// temporalCall recognizes a call starting a workflow or an activity.
func (pf *pyFile) temporalCall(n *sitter.Node, caller string, bound map[string]bool) (store.Contract, bool) {
	fn := field(n, "function")
	if fn == nil || fn.Type() != "attribute" {
		return store.Contract{}, false
	}
	method := text(field(fn, "attribute"), pf.src)
	kind, ok := pyTemporalStarts[method]
	if !ok {
		return store.Contract{}, false
	}
	obj := field(fn, "object")
	onModule := obj != nil && obj.Type() == "identifier" && pf.temporalModule(text(obj, pf.src)) == "workflow"
	if !onModule {
		// Client methods: the file must use Temporal.
		usesTemporal := false
		for _, p := range pf.imports {
			usesTemporal = usesTemporal || strings.HasPrefix(p, "temporalio")
		}
		if !usesTemporal || (method != "execute_workflow" && method != "start_workflow") {
			return store.Contract{}, false
		}
	}
	var target *sitter.Node
	queue := ""
	for _, a := range namedChildren(field(n, "arguments")) {
		switch {
		case a.Type() == "keyword_argument":
			switch text(field(a, "name"), pf.src) {
			case "task_queue":
				if v := pf.template(field(a, "value")); !strings.Contains(v, "*") {
					queue = v
				}
			case "workflow", "activity":
				target = field(a, "value")
			}
		case target == nil:
			target = a
		}
	}
	name, ref := "", ""
	switch {
	case target == nil:
	case target.Type() == "string":
		if v := pf.template(target); !strings.Contains(v, "*") {
			name = v
		}
	case target.Type() == "identifier":
		if bound[text(target, pf.src)] {
			return store.Contract{}, false // a parameter or local variable holding the workflow or activity
		}
		name = text(target, pf.src)
		ref = pf.codeRef(name)
	case target.Type() == "attribute":
		attr := text(field(target, "attribute"), pf.src)
		o := text(field(target, "object"), pf.src)
		if kind == store.ContractTemporalWorkflow {
			// OrderWorkflow.run: the workflow is the class.
			name = o[strings.LastIndex(o, ".")+1:]
		} else {
			name = attr
		}
		if root, _, _ := strings.Cut(o, "."); root != "self" && !bound[root] {
			ref = pf.codeRef(o + "." + attr)
		}
	}
	if name == "" {
		return store.Contract{}, false
	}
	return store.Contract{Kind: kind, Role: store.RoleUses, Name: name, Symbol: caller, Line: line(n), Detail: queue,
		Exact: true, Target: ref}, true
}

// codeRef is the reference to a name used in this module: through the
// import binding its first part, or a name of the module itself.
func (pf *pyFile) codeRef(name string) string {
	first, rest, _ := strings.Cut(name, ".")
	if full, ok := pf.imports[first]; ok {
		if rest != "" {
			return full + "." + rest
		}
		return full
	}
	return pf.module + "." + name
}

// pyBound collects a function's parameter names and the names assigned in
// its body: variables, which never name a module-level function or class.
func pyBound(def, body *sitter.Node, src []byte) map[string]bool {
	bound := map[string]bool{}
	for _, p := range namedChildren(field(def, "parameters")) {
		switch p.Type() {
		case "identifier":
			bound[text(p, src)] = true
		default:
			if nn := field(p, "name"); nn != nil {
				bound[text(nn, src)] = true
			} else if first := p.NamedChild(0); first != nil && first.Type() == "identifier" {
				bound[text(first, src)] = true
			}
		}
	}
	walk(body, func(n *sitter.Node) bool {
		switch n.Type() {
		case "function_definition", "class_definition":
			return n == body
		case "assignment", "for_statement":
			if l := field(n, "left"); l != nil && l.Type() == "identifier" {
				bound[text(l, src)] = true
			}
		}
		return true
	})
	return bound
}
