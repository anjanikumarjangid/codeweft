// Command implements type-checks the Go standard library and writes the
// ground truth for codewiki's Go type hierarchy:
//
//	OUTDIR/implements.txt  every pair "T I" where the named concrete type T
//	                       (or *T) implements the interface I
//	OUTDIR/interfaces.txt  the interfaces considered
//	OUTDIR/concrete.txt    the concrete types considered
//
// Both types are declared in the standard library and neither is generic;
// empty interfaces and constraint-only interfaces are left out. Names
// follow codewiki's qualification.
//
// Usage: go run ./implements OUTDIR
package main

import (
	"fmt"
	"go/importer"
	"go/token"
	"go/types"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

func qual(p *types.Package) string {
	if path.Base(p.Path()) == p.Name() {
		return p.Path()
	}
	return p.Path() + "/" + p.Name()
}

func typeName(t *types.Named) string { return qual(t.Obj().Pkg()) + "." + t.Obj().Name() }

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: implements OUTDIR")
		os.Exit(2)
	}
	out, err := exec.Command("go", "list", "std").Output()
	if err != nil {
		panic(err)
	}
	imp := importer.ForCompiler(token.NewFileSet(), "source", nil).(types.ImporterFrom)
	var ifaces, concrete []*types.Named
	for _, p := range strings.Fields(string(out)) {
		if strings.HasPrefix(p, "vendor/") {
			continue
		}
		pkg, err := imp.ImportFrom(p, "", 0)
		if err != nil {
			fmt.Fprintln(os.Stderr, "skip", p, err)
			continue
		}
		for _, n := range pkg.Scope().Names() {
			tn, ok := pkg.Scope().Lookup(n).(*types.TypeName)
			if !ok || tn.IsAlias() {
				continue
			}
			nt, ok := tn.Type().(*types.Named)
			if !ok || nt.TypeParams().Len() > 0 {
				continue
			}
			if it, ok := nt.Underlying().(*types.Interface); ok {
				if it.NumMethods() > 0 && it.IsMethodSet() {
					ifaces = append(ifaces, nt)
				}
			} else {
				concrete = append(concrete, nt)
			}
		}
	}
	pairs := map[string]bool{}
	for _, i := range ifaces {
		it := i.Underlying().(*types.Interface)
		for _, t := range concrete {
			if types.Implements(t, it) || types.Implements(types.NewPointer(t), it) {
				pairs[typeName(t)+" "+typeName(i)] = true
			}
		}
	}
	names := func(ts []*types.Named) map[string]bool {
		m := map[string]bool{}
		for _, t := range ts {
			m[typeName(t)] = true
		}
		return m
	}
	write(filepath.Join(os.Args[1], "implements.txt"), pairs)
	write(filepath.Join(os.Args[1], "interfaces.txt"), names(ifaces))
	write(filepath.Join(os.Args[1], "concrete.txt"), names(concrete))
	fmt.Fprintln(os.Stderr, len(ifaces), "interfaces,", len(concrete), "concrete types,", len(pairs), "pairs")
}

func write(file string, set map[string]bool) {
	lines := make([]string, 0, len(set))
	for l := range set {
		lines = append(lines, l)
	}
	sort.Strings(lines)
	if err := os.WriteFile(file, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		panic(err)
	}
}
