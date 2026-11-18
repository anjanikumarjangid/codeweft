package store

import (
	"database/sql"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Contract kinds: the ways services talk to each other.
const (
	ContractHTTP             = "http"
	ContractGRPC             = "grpc"
	ContractKafka            = "kafka"
	ContractTemporalWorkflow = "temporal-workflow"
	ContractTemporalActivity = "temporal-activity"
)

// Contract roles. A link goes from a use to what provides it: an HTTP
// client call to the route serving it, a producer to the consumers of its
// topic, the code starting a workflow to the workflow.
const (
	RoleProvides = "provides"
	RoleUses     = "uses"
)

// Contract is a point where code meets another service, or another part
// of the same one: a route it serves, a URL it calls, a topic it consumes
// or produces, a workflow it defines or starts.
type Contract struct {
	Kind   string `json:"kind"`
	Role   string `json:"role"`
	Name   string `json:"name"`             // the key links match on: a path pattern, topic, workflow name
	Method string `json:"method,omitempty"` // HTTP method, "" when not known
	Symbol string `json:"symbol"`           // qualified name of the function providing or using it
	Line   int    `json:"line"`
	Detail string `json:"detail,omitempty"` // as written: the URL template, a task queue
	// Exact is true when Name was written out in full in the code; false
	// when parts of it come from values the index cannot see (a base URL
	// from configuration, a topic name from a variable).
	Exact bool `json:"exact"`
	// Target is, for a use that names the code it reaches (the workflow
	// function passed to ExecuteWorkflow, the interface of a Java stub),
	// that code: a reference while indexing, its qualified name once
	// resolved, "" otherwise. A provider with this symbol, or declared by
	// it, in the same repository is the one the use reaches, whatever other
	// code shares its name. For a provider implementing a Java interface
	// method, Target is that interface method.
	Target string `json:"target,omitempty"`
}

// ContractRow is a stored contract with its file.
type ContractRow struct {
	Contract
	File string `json:"file"`
}

// AddContract records a contract found in a file.
func (t *Tx) AddContract(fileID int64, c Contract) error {
	_, err := t.exec(`INSERT INTO contracts(file_id, kind, role, name, method, symbol, line, detail, exact, target)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		fileID, c.Kind, c.Role, c.Name, c.Method, c.Symbol, c.Line, c.Detail, c.Exact, c.Target)
	return err
}

// Contracts returns every contract in the index, optionally of one kind.
func (s *Store) Contracts(kind string) ([]ContractRow, error) {
	q := `SELECT c.kind, c.role, c.name, c.method, c.symbol, c.line, c.detail, c.exact, c.target, f.path
		FROM contracts c JOIN files f ON f.id = c.file_id`
	var args []any
	if kind != "" {
		q += ` WHERE c.kind = ?`
		args = append(args, kind)
	}
	rows, err := s.db.Query(q+` ORDER BY c.kind, c.name, f.path, c.line`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ContractRow{}
	for rows.Next() {
		var c ContractRow
		if err := rows.Scan(&c.Kind, &c.Role, &c.Name, &c.Method, &c.Symbol, &c.Line, &c.Detail, &c.Exact, &c.Target, &c.File); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return s.resolveContracts(out)
}

// Markers in a provider's Symbol for contracts the store expands once
// every type is indexed.
const (
	// StructMethods + a type reference: every exported method of a Go
	// struct, named with Name as prefix: a struct registered as Temporal
	// activities, or a gRPC server (Name "OrderService/").
	StructMethods = "struct:"
	// ClassMethods + a class's qualified name: the public methods of a
	// class and of every class extending it, named with Name as prefix (a
	// Java method's first letter upper-cased): a gRPC servicer whose
	// methods may be written in a subclass.
	ClassMethods = "class:"
	// ImplementationOf + a type reference: a Java class registered with a
	// Temporal worker; its workflow is named after the interfaces it
	// implements, its activities after its public methods.
	ImplementationOf = "impl:"
)

// resolveContracts reports each provider at its definition and expands the
// marked ones. A route, workflow or activity is recorded where the code
// names it (a route registration, a worker registering a function); it is
// reported at the function's definition, as list_apis does. A Java
// interface method marked as a workflow or activity is reported at its
// implementation, and dropped when the repository has none: a service
// that only starts a workflow often holds a copy of its interface.
// Providers found twice (a workflow function and its registration) are
// listed once, keeping the task queue when one of them names it.
func (s *Store) resolveContracts(in []ContractRow) ([]ContractRow, error) {
	out := []ContractRow{}
	index := map[string]int{}
	byName := map[string]int{} // providers by kind and name
	add := func(c ContractRow) {
		if c.Role == RoleProvides {
			// One provider per task queue: a function registered by two
			// workers is served on both queues.
			key := c.Kind + "|" + c.Method + "|" + c.Name + "|" + c.Symbol + "|" + c.Detail
			if _, ok := index[key]; ok {
				return
			}
			index[key] = len(out)
			if _, ok := byName[c.Kind+"|"+c.Name]; !ok {
				byName[c.Kind+"|"+c.Name] = len(out)
			}
		}
		out = append(out, c)
	}
	// Classes registered with a Temporal worker come last: they only add
	// workflows and activities not already declared by an annotated
	// interface, whose task queue they give.
	sort.SliceStable(in, func(i, j int) bool {
		return !strings.HasPrefix(in[i].Symbol, ImplementationOf) && strings.HasPrefix(in[j].Symbol, ImplementationOf)
	})
	registered := func(c ContractRow, class string) {
		// Already declared by the annotated interface this class
		// implements: give those providers the worker's task queue.
		declared := false
		for i := range out {
			if o := &out[i]; o.Role == RoleProvides && o.Kind == c.Kind &&
				(o.Symbol == c.Symbol || strings.HasPrefix(o.Symbol, class+".")) {
				declared = true
				if o.Detail == "" {
					o.Detail = c.Detail
				}
			}
		}
		if _, ok := byName[c.Kind+"|"+c.Name]; !declared && !ok {
			add(c)
		}
	}
	for _, c := range in {
		if c.Role != RoleProvides {
			if c.Target != "" {
				sym, ok, err := s.symbolByRef(c.Target)
				if err != nil {
					return nil, err
				}
				ref := c.Target
				c.Target = ""
				if ok {
					c.Target = sym.Qualified
				} else if t, ok := s.typeByRef(ref); ok {
					c.Target = t
				}
			}
			add(c)
			continue
		}
		switch {
		case strings.HasPrefix(c.Symbol, StructMethods):
			t, ok := s.typeByRef(strings.TrimPrefix(c.Symbol, StructMethods))
			if !ok {
				continue
			}
			ms, err := s.methodsOf(t, `s.exported = 1 AND s.kind = 'method'`)
			if err != nil {
				return nil, err
			}
			for _, m := range ms {
				d := c
				d.Name, d.Symbol, d.File, d.Line = c.Name+m.Name, m.Qualified, m.File, m.StartLine
				add(d)
			}
		case strings.HasPrefix(c.Symbol, ClassMethods):
			ms, err := s.classMethods(strings.TrimPrefix(c.Symbol, ClassMethods))
			if err != nil {
				return nil, err
			}
			for _, m := range ms {
				d := c
				d.Name, d.Symbol, d.File, d.Line = c.Name+m.Name, m.Qualified, m.File, m.StartLine
				add(d)
			}
		case strings.HasPrefix(c.Symbol, ImplementationOf):
			t, ok := s.typeByRef(strings.TrimPrefix(c.Symbol, ImplementationOf))
			if !ok {
				continue
			}
			if c.Kind == ContractTemporalActivity {
				ms, err := s.methodsOf(t, `s.exported = 1 AND s.kind = 'method'`)
				if err != nil {
					return nil, err
				}
				for _, m := range ms {
					d := c
					d.Name, d.Symbol, d.File, d.Line = capitalize(m.Name), m.Qualified, m.File, m.StartLine
					registered(d, t)
				}
				continue
			}
			var id int64
			var file string
			var line int
			if s.db.QueryRow(`SELECT s.id, f.path, s.start_line FROM symbols s JOIN files f ON f.id = s.file_id
				WHERE s.qualified = ? ORDER BY s.id LIMIT 1`, t).Scan(&id, &file, &line) != nil {
				continue
			}
			supers, err := s.Supertypes(id, 1)
			if err != nil {
				return nil, err
			}
			for _, sup := range supers {
				if sup.How != SuperImplements {
					continue
				}
				d := c
				d.Name, d.Symbol, d.File, d.Line = sup.Qualified[strings.LastIndexAny(sup.Qualified, ".$")+1:], t, file, line
				registered(d, t)
			}
		default:
			sym, ok, err := s.symbolByRef(c.Symbol)
			if err != nil {
				return nil, err
			}
			if !ok {
				add(c)
				continue
			}
			if sym.Kind == "interface method" && c.Kind != ContractHTTP {
				impls, err := s.MethodImplementations(sym, 5)
				if err != nil {
					return nil, err
				}
				for _, im := range impls {
					d := c
					d.Symbol, d.Target = im.Qualified, sym.Qualified
					d.File, d.Line = splitLocation(im.Location)
					add(d)
				}
				continue
			}
			c.Symbol, c.File, c.Line = sym.Qualified, sym.File, sym.StartLine
			add(c)
		}
	}
	// A workflow function is also named by the workers registering it,
	// which say on which task queues it runs: the definition without a
	// queue adds nothing then. Under another name (a workflow registered
	// with RegisterOptions.Name, an alias), it takes those queues.
	queues := map[string][]string{}
	for _, c := range out {
		if c.Role == RoleProvides && c.Kind != ContractHTTP && c.Detail != "" {
			queues[c.Kind+"|"+c.Symbol] = append(queues[c.Kind+"|"+c.Symbol], c.Detail)
		}
	}
	queued := map[string]bool{}
	for _, c := range out {
		if c.Role == RoleProvides && c.Detail != "" {
			queued[c.Kind+"|"+c.Name+"|"+c.Symbol] = true
		}
	}
	kept := out[:0]
	for _, c := range out {
		qs := queues[c.Kind+"|"+c.Symbol]
		if c.Role != RoleProvides || c.Kind == ContractHTTP || c.Detail != "" || len(qs) == 0 {
			kept = append(kept, c)
			continue
		}
		if queued[c.Kind+"|"+c.Name+"|"+c.Symbol] {
			continue
		}
		for _, q := range uniq(qs) {
			d := c
			d.Detail = q
			kept = append(kept, d)
		}
	}
	return s.checkGRPC(kept)
}

// checkGRPC names gRPC contracts after the services of the indexed .proto
// files. A service declared once gets its full name (shop.orders.v1.OrderService)
// as Detail, and its providers keep only the methods it declares (a server
// struct has helpers besides its RPC methods). A client call found without
// a grpc import is kept only when a .proto declares its service.
func (s *Store) checkGRPC(in []ContractRow) ([]ContractRow, error) {
	services := map[string][]string{} // short name -> qualified services
	rpcs := map[string]bool{}         // qualified service . method
	rows, err := s.db.Query(`SELECT s.qualified, s.name, s.kind FROM symbols s JOIN files f ON f.id = s.file_id
		WHERE f.lang = 'proto' AND s.kind IN ('service', 'rpc')`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var q, name, kind string
		if err := rows.Scan(&q, &name, &kind); err != nil {
			rows.Close()
			return nil, err
		}
		if kind == "service" {
			services[name] = append(services[name], q)
		} else {
			rpcs[q] = true
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := in[:0]
	for _, c := range in {
		if c.Kind != ContractGRPC {
			out = append(out, c)
			continue
		}
		svc, method, _ := strings.Cut(c.Name, "/")
		qs := uniq(services[svc])
		if len(qs) == 1 {
			c.Detail = qs[0]
			if c.Role == RoleProvides && !rpcs[qs[0]+"."+method] {
				continue // a helper method of the server, not an RPC
			}
		}
		if c.Role == RoleUses && !c.Exact {
			if len(qs) == 0 {
				continue // a NewXClient of something else than gRPC
			}
			c.Exact = true
		}
		out = append(out, c)
	}
	return out, nil
}

func uniq(ss []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range ss {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
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

func splitLocation(loc string) (string, int) {
	i := strings.LastIndexByte(loc, ':')
	if i < 0 {
		return loc, 0
	}
	n, _ := strconv.Atoi(loc[i+1:])
	return loc[:i], n
}

// symbolByRef finds the function, method or type a reference names: a
// qualified name, "@PATH.NAME" for a Go import path, or a name as written
// in the code ("h.getOrder"), found by its last part when exactly one
// function or method has it.
func (s *Store) symbolByRef(ref string) (Symbol, bool, error) {
	q := strings.TrimPrefix(ref, "@")
	if strings.HasPrefix(ref, "@") {
		if i := strings.LastIndexByte(q, '.'); i > 0 {
			if g, ok := s.goImported(q[:i], q[i+1:]); ok {
				q = g
			} else if j := strings.LastIndexByte(q[:i], '.'); j > 0 {
				// @PATH.T.M: a method of a type of an imported package.
				if g, ok := s.goImported(q[:j], q[j+1:i]); ok {
					q = g + q[i:]
				}
			}
		}
	}
	sym, ok, err := s.symbolNamed(q)
	if err != nil || ok {
		return sym, ok, err
	}
	if h, _, ok := s.handlerByName(ref); ok {
		return s.symbolNamed(h)
	}
	// A Python name imported from a package path that is not the
	// repository's (from inventory.notify import X for py/inventory/notify.py),
	// or a Go package whose name differs from its directory
	// (externalenvconf.Activity in external-env-conf/): the one symbol whose
	// qualified name ends with it.
	if strings.Count(q, ".") >= 1 {
		var n int
		var only string
		_ = s.db.QueryRow(`SELECT COUNT(DISTINCT qualified), MIN(qualified) FROM symbols WHERE name = ? AND
			(qualified = ? OR qualified LIKE ? OR qualified LIKE ?)`, q[strings.LastIndexByte(q, '.')+1:], q, "%."+q, "%/"+q).Scan(&n, &only)
		if n == 1 {
			return s.symbolNamed(only)
		}
	}
	return Symbol{}, false, nil
}

func (s *Store) symbolNamed(q string) (Symbol, bool, error) {
	var sym Symbol
	err := s.db.QueryRow(`SELECT s.qualified, s.name, s.kind, f.path, s.start_line FROM symbols s
		JOIN files f ON f.id = s.file_id WHERE s.qualified = ? ORDER BY s.id LIMIT 1`, q).
		Scan(&sym.Qualified, &sym.Name, &sym.Kind, &sym.File, &sym.StartLine)
	if err == sql.ErrNoRows {
		return sym, false, nil
	}
	return sym, err == nil, err
}

// goImported finds the qualified name of NAME declared at package level in
// the Go package imported as path: the package in the directory the import
// path ends with (example.com/shop/internal/orders -> internal/orders), or
// the repository root's package when no directory matches.
func (s *Store) goImported(path, name string) (string, bool) {
	rows, err := s.db.Query(`SELECT DISTINCT s.qualified, f.dir FROM symbols s JOIN files f ON f.id = s.file_id
		WHERE s.name = ? AND f.lang = 'go' AND s.qualified = f.qual || '.' || s.name`, name)
	if err != nil {
		return "", false
	}
	defer rows.Close()
	// The longest directory the path ends with: for .../standalone/helloworld,
	// standalone/helloworld rather than helloworld.
	var inDir, atRoot []string
	longest := 0
	for rows.Next() {
		var q, dir string
		if rows.Scan(&q, &dir) != nil {
			return "", false
		}
		switch {
		case dir == ".":
			atRoot = append(atRoot, q)
		case path == dir || strings.HasSuffix(path, "/"+dir):
			if len(dir) > longest {
				inDir, longest = nil, len(dir)
			}
			if len(dir) == longest {
				inDir = append(inDir, q)
			}
		}
	}
	switch {
	case len(inDir) == 1:
		return inDir[0], true
	case len(inDir) == 0 && len(atRoot) == 1:
		return atRoot[0], true
	}
	return "", false
}

// typeByRef finds the type a reference names: a qualified name, a Go
// "@PATH.T", or a Java or Python reference as the parsers write them
// ("~pkg.T", "^Scope|T", "@full.Name"), found by its short name when
// exactly one type has it.
func (s *Store) typeByRef(ref string) (string, bool) {
	if strings.HasPrefix(ref, "@") {
		if i := strings.LastIndexByte(ref, '.'); i > 0 {
			if q, ok := s.goImported(ref[1:i], ref[i+1:]); ok {
				return q, true
			}
		}
	}
	if scoped, ok := strings.CutPrefix(ref, "^"); ok {
		// A Java name looked up from a class: a nested type of the class or
		// of an enclosing one, or a type of its package.
		scope, name, _ := strings.Cut(scoped, "|")
		for p := scope; p != ""; p = p[:max(strings.LastIndexByte(p, '.'), 0)] {
			if sym, ok, _ := s.symbolNamed(p + "." + name); ok && sym.Kind != "method" && sym.Kind != "interface method" {
				return sym.Qualified, true
			}
			if !strings.Contains(p, ".") {
				break
			}
		}
	}
	q := strings.TrimLeft(ref, "@~?")
	if !strings.HasPrefix(q, "^") {
		if _, ok, _ := s.symbolNamed(q); ok {
			return q, true
		}
	}
	short := ref[strings.LastIndexAny(ref, ".@~?^|")+1:]
	var n int
	var only string
	_ = s.db.QueryRow(`SELECT COUNT(DISTINCT qualified), MIN(qualified) FROM symbols WHERE name = ?
		AND kind IN ('struct', 'type', 'class', 'record', 'enum', 'interface')`, short).Scan(&n, &only)
	return only, n == 1
}

// classMethods lists the public methods of a class and of the classes
// extending it, nearest first, with Java names upper-cased.
func (s *Store) classMethods(class string) ([]Symbol, error) {
	var id int64
	var lang string
	err := s.db.QueryRow(`SELECT s.id, f.lang FROM symbols s JOIN files f ON f.id = s.file_id WHERE s.qualified = ?
		ORDER BY s.id LIMIT 1`, class).Scan(&id, &lang)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	classes := []string{class}
	subs, err := s.Subtypes(id, 5)
	if err != nil {
		return nil, err
	}
	for _, sub := range subs {
		classes = append(classes, sub.Qualified)
	}
	cond := `s.kind = 'method' AND s.name NOT LIKE '\_%' ESCAPE '\'`
	if lang == "java" {
		cond = `s.kind = 'method' AND s.exported = 1`
	}
	var out []Symbol
	for _, c := range classes {
		ms, err := s.methodsOf(c, cond)
		if err != nil {
			return nil, err
		}
		for _, m := range ms {
			if lang == "java" {
				m.Name = capitalize(m.Name)
			}
			out = append(out, m)
		}
	}
	return out, nil
}

// methodsOf lists the methods declared on a type, filtered by cond.
func (s *Store) methodsOf(t, cond string) ([]Symbol, error) {
	rows, err := s.db.Query(`SELECT s.qualified, s.name, s.kind, f.path, s.start_line FROM symbols s
		JOIN files f ON f.id = s.file_id WHERE s.qualified = ? || '.' || s.name AND `+cond+`
		ORDER BY s.start_line`, t)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Symbol
	for rows.Next() {
		var m Symbol
		if err := rows.Scan(&m.Qualified, &m.Name, &m.Kind, &m.File, &m.StartLine); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// NormalizePath reduces an HTTP route or a client URL to the pattern links
// compare: scheme, host, query and fragment are dropped, and every segment
// that is a parameter ({id}, :id, <int:id>, *rest) or contains a value the
// index cannot see ("*") becomes "*". It returns "" when no path is known
// (a URL that is entirely a variable).
func NormalizePath(p string) string {
	p = strings.TrimSpace(p)
	if i := strings.Index(p, "://"); i >= 0 {
		rest := p[i+3:]
		j := strings.IndexByte(rest, '/')
		if j < 0 {
			return "/"
		}
		p = rest[j:]
	}
	if i := strings.IndexAny(p, "?#"); i >= 0 {
		p = p[:i]
	}
	if !strings.HasPrefix(p, "/") {
		// "*/orders/*": an unknown base URL, then a path.
		i := strings.IndexByte(p, '/')
		if i < 0 {
			return ""
		}
		p = p[i:]
	}
	var segs []string
	for _, s := range strings.Split(p, "/") {
		switch {
		case s == "":
			continue
		case strings.Contains(s, "*"), strings.HasPrefix(s, ":"), strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}"),
			strings.HasPrefix(s, "<") && strings.HasSuffix(s, ">"):
			segs = append(segs, "*")
		default:
			segs = append(segs, s)
		}
	}
	return "/" + strings.Join(segs, "/")
}

// PathsMatch reports whether a client path can reach a route pattern, both
// normalized: the same number of segments, each pair equal or one of them
// "*". It also returns how many segments matched literally, so callers can
// prefer the most specific route.
func PathsMatch(client, route string) (bool, int) {
	if client == "" || route == "" {
		return false, 0
	}
	a, b := strings.Split(client, "/"), strings.Split(route, "/")
	if len(a) != len(b) {
		return false, 0
	}
	literal := 0
	for i := range a {
		switch {
		case a[i] == b[i] && a[i] != "*":
			literal++
		case a[i] == "*" || b[i] == "*":
		default:
			return false, 0
		}
	}
	return true, literal
}

// String describes a contract for text output.
func (c Contract) String() string {
	name := c.Name
	if c.Method != "" {
		name = c.Method + " " + name
	}
	return fmt.Sprintf("%s %s %s", c.Kind, c.Role, name)
}
