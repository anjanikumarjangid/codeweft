package indexer

import (
	"go/ast"
	"go/token"
	"strings"

	"github.com/anjanikumarjangid/codewiki/internal/store"
)

// Temporal, Go SDK (go.temporal.io/sdk).
//
// A workflow is provided by a function whose first parameter is a
// workflow.Context, and by a worker's RegisterWorkflow call; an activity by
// RegisterActivity, of a function or of a struct, whose exported methods
// are then each an activity. Temporal names a workflow or activity after
// its function or method unless RegisterOptions.Name says otherwise (for a
// struct, Name is a prefix of its methods' names).
//
// Uses: client.ExecuteWorkflow and SignalWithStartWorkflow start a
// workflow, workflow.ExecuteChildWorkflow a child workflow, and
// workflow.ExecuteActivity and ExecuteLocalActivity an activity, each given
// the function or its registered name.

const (
	temporalSDK      = "go.temporal.io/sdk/"
	temporalWorkflow = "go.temporal.io/sdk/workflow"
	temporalWorker   = "go.temporal.io/sdk/worker"
)

func usesTemporal(aliases map[string]string) bool {
	for _, p := range aliases {
		if strings.HasPrefix(p, temporalSDK) {
			return true
		}
	}
	return false
}

// goWorkflowDef reports a workflow definition: a function or method whose
// first parameter is a workflow.Context.
func goWorkflowDef(fset *token.FileSet, d *ast.FuncDecl, qualified string, aliases map[string]string) (store.Contract, bool) {
	ps := d.Type.Params.List
	if len(ps) == 0 {
		return store.Contract{}, false
	}
	sel, ok := ps[0].Type.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Context" {
		return store.Contract{}, false
	}
	if pkg, ok := sel.X.(*ast.Ident); !ok || aliases[pkg.Name] != temporalWorkflow {
		return store.Contract{}, false
	}
	return store.Contract{Kind: store.ContractTemporalWorkflow, Role: store.RoleProvides, Name: d.Name.Name,
		Symbol: qualified, Line: fset.Position(d.Pos()).Line, Exact: true}, true
}

// goFuncRef names the function or method a function value refers to, as
// Temporal does, and gives a reference to its symbol: "QUAL.F" for a
// function of this package, "@PATH.F" for one of an imported package, the
// expression as written for a method value (the store finds the method by
// its name).
func goFuncRef(e ast.Expr, qual string, aliases map[string]string, enc *goEncoder) (name, ref string) {
	switch t := e.(type) {
	case *ast.ParenExpr:
		return goFuncRef(t.X, qual, aliases, enc)
	case *ast.IndexExpr: // an instantiated generic function
		return goFuncRef(t.X, qual, aliases, enc)
	case *ast.Ident:
		if l, ok := enc.lookup(t.Name, t.Pos()); ok && (l.val != nil || l.typ != nil) {
			return "", "" // a local variable holding a function value
		}
		return t.Name, qual + "." + t.Name
	case *ast.SelectorExpr:
		if pkg, ok := t.X.(*ast.Ident); ok && enc.isPackage(pkg.Name, pkg.Pos()) {
			if p := aliases[pkg.Name]; p != "" {
				return t.Sel.Name, "@" + p + "." + t.Sel.Name
			}
		}
		// A method value or expression: the method of the receiver's type,
		// when the type is written in the code ((*Activities).Reserve, or
		// a.Reserve for var a *Activities or a := &Activities{}).
		if p, ok := t.X.(*ast.ParenExpr); ok {
			if ref := goTypeRef(p.X, qual, aliases); ref != "" {
				return t.Sel.Name, ref + "." + t.Sel.Name
			}
		}
		if x, ok := t.X.(*ast.Ident); ok {
			if typ := goStructValue(x, enc, 0); typ != nil {
				if ref := goTypeRef(typ, qual, aliases); ref != "" {
					return t.Sel.Name, ref + "." + t.Sel.Name
				}
			}
		}
		return t.Sel.Name, printNode(enc.fset, t)
	}
	return "", ""
}

// goStructValue returns the type of a struct value given to
// RegisterActivity: &T{...}, T{...}, new(T), or a local variable holding
// one of these.
func goStructValue(e ast.Expr, enc *goEncoder, depth int) ast.Expr {
	switch t := e.(type) {
	case *ast.UnaryExpr:
		if t.Op == token.AND {
			return goStructValue(t.X, enc, depth)
		}
	case *ast.CompositeLit:
		return t.Type
	case *ast.CallExpr:
		if id, ok := t.Fun.(*ast.Ident); ok && id.Name == "new" && len(t.Args) == 1 {
			return t.Args[0]
		}
	case *ast.Ident:
		if l, ok := enc.lookup(t.Name, t.Pos()); ok && depth < 3 {
			if l.val != nil {
				return goStructValue(l.val, enc, depth+1)
			}
			return l.typ
		}
	}
	return nil
}

// goCompositeField returns the value of a field in a composite literal
// (Options{Name: "x"}, &Options{...}), or in the literal a local variable
// was set to.
func goCompositeField(e ast.Expr, name string, enc *goEncoder) ast.Expr {
	switch t := e.(type) {
	case *ast.UnaryExpr:
		return goCompositeField(t.X, name, enc)
	case *ast.Ident:
		if l, ok := enc.lookup(t.Name, t.Pos()); ok && l.val != nil {
			if _, isIdent := l.val.(*ast.Ident); !isIdent {
				return goCompositeField(l.val, name, enc)
			}
		}
	case *ast.CompositeLit:
		for _, el := range t.Elts {
			if kv, ok := el.(*ast.KeyValueExpr); ok {
				if k, ok := kv.Key.(*ast.Ident); ok && k.Name == name {
					return kv.Value
				}
			}
		}
	}
	return nil
}

// goTemporalCalls finds a function body's Temporal registrations and the
// workflows and activities it starts.
func goTemporalCalls(fset *token.FileSet, body *ast.BlockStmt, caller, qual string, aliases, consts map[string]string, enc *goEncoder) []store.Contract {
	if !usesTemporal(aliases) {
		return nil
	}
	known := func(e ast.Expr) string {
		if e == nil {
			return ""
		}
		if v := goTemplate(e, consts, aliases); !strings.Contains(v, "*") {
			return v
		}
		return ""
	}
	// Task queues of the workers created here: w := worker.New(c, "queue", opts).
	queues := map[string]string{}
	ast.Inspect(body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
			return true
		}
		call, ok := as.Rhs[0].(*ast.CallExpr)
		if !ok || len(call.Args) < 2 {
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "New" {
			if pkg, ok := sel.X.(*ast.Ident); ok && aliases[pkg.Name] == temporalWorker {
				if id, ok := as.Lhs[0].(*ast.Ident); ok {
					queues[id.Name] = known(call.Args[1])
				}
			}
		}
		return true
	})

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
		line := fset.Position(call.Pos()).Line
		pkgPath := ""
		if pkg, ok := sel.X.(*ast.Ident); ok && enc.isPackage(pkg.Name, pkg.Pos()) {
			pkgPath = aliases[pkg.Name]
		}
		args := call.Args
		// use records a workflow or activity started by name or function.
		use := func(kind string, fn ast.Expr, queue string) {
			name, target := known(fn), ""
			if name == "" {
				name, target = goFuncRef(fn, qual, aliases, enc)
			}
			if name != "" {
				out = append(out, store.Contract{Kind: kind, Role: store.RoleUses, Name: name, Symbol: caller,
					Line: line, Detail: queue, Exact: true, Target: target})
			}
		}
		switch name := sel.Sel.Name; {
		case pkgPath == "" && (name == "RegisterWorkflow" || name == "RegisterWorkflowWithOptions" ||
			name == "RegisterActivity" || name == "RegisterActivityWithOptions") && len(args) > 0:
			queue := ""
			if id, ok := sel.X.(*ast.Ident); ok {
				queue = queues[id.Name]
			}
			kind := store.ContractTemporalWorkflow
			if strings.HasPrefix(name, "RegisterActivity") {
				kind = store.ContractTemporalActivity
			}
			custom := ""
			if strings.HasSuffix(name, "WithOptions") && len(args) > 1 {
				custom = known(goCompositeField(args[1], "Name", enc))
			}
			if kind == store.ContractTemporalActivity {
				if t := goStructValue(args[0], enc, 0); t != nil {
					if ref := goTypeRef(t, qual, aliases); ref != "" {
						out = append(out, store.Contract{Kind: kind, Role: store.RoleProvides, Name: custom,
							Symbol: store.StructMethods + ref, Line: line, Detail: queue, Exact: true})
					}
					return true
				}
			}
			fname, ref := goFuncRef(args[0], qual, aliases, enc)
			if ref == "" {
				return true
			}
			if custom != "" {
				fname = custom
			}
			out = append(out, store.Contract{Kind: kind, Role: store.RoleProvides, Name: fname, Symbol: ref,
				Line: line, Detail: queue, Exact: true})
		case pkgPath == "" && name == "ExecuteWorkflow" && len(args) >= 3:
			use(store.ContractTemporalWorkflow, args[2], known(goCompositeField(args[1], "TaskQueue", enc)))
		case pkgPath == "" && name == "SignalWithStartWorkflow" && len(args) >= 6:
			use(store.ContractTemporalWorkflow, args[5], known(goCompositeField(args[4], "TaskQueue", enc)))
		case pkgPath == temporalWorkflow && name == "ExecuteChildWorkflow" && len(args) >= 2:
			use(store.ContractTemporalWorkflow, args[1], "")
		case pkgPath == temporalWorkflow && (name == "ExecuteActivity" || name == "ExecuteLocalActivity") && len(args) >= 2:
			use(store.ContractTemporalActivity, args[1], "")
		}
		return true
	})
	return out
}
