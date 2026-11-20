package store

import (
	"database/sql"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// Tx is a write batch used while indexing. Indexing a whole repository in one
// transaction is much faster than committing per file, and readers never see
// a half-built index.
type Tx struct {
	tx    *txConn
	stmts map[string]*stmt
}

// exec runs a statement through a cached prepared statement, which matters
// when indexing hundreds of thousands of rows.
func (t *Tx) exec(query string, args ...any) (sql.Result, error) {
	st, err := t.prepared(query)
	if err != nil {
		return nil, err
	}
	return st.Exec(args...)
}

// insertID runs an INSERT ... RETURNING id through a cached prepared
// statement and returns the new row's id.
func (t *Tx) insertID(query string, args ...any) (int64, error) {
	st, err := t.prepared(query + ` RETURNING id`)
	if err != nil {
		return 0, err
	}
	var id int64
	err = st.QueryRow(args...).Scan(&id)
	return id, err
}

func (t *Tx) prepared(query string) (*stmt, error) {
	st, ok := t.stmts[query]
	if !ok {
		var err error
		if st, err = t.tx.Prepare(query); err != nil {
			return nil, err
		}
		t.stmts[query] = st
	}
	return st, nil
}

// Begin starts a write batch.
func (s *Store) Begin() (*Tx, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	return &Tx{tx: tx, stmts: map[string]*stmt{}}, nil
}

// Commit commits the batch.
func (t *Tx) Commit() error { return t.tx.Commit() }

// Rollback abandons the batch.
func (t *Tx) Rollback() error { return t.tx.Rollback() }

// Reset removes all indexed data, keeping the schema.
func (t *Tx) Reset() error {
	for _, q := range []string{
		`DELETE FROM contracts`, `DELETE FROM vars`, `DELETE FROM test_edges`, `DELETE FROM test_funcs`, `DELETE FROM type_edges`, `DELETE FROM supertypes`, `DELETE FROM edges`, `DELETE FROM endpoints`, `DELETE FROM raw_calls`, `DELETE FROM fields`,
		`DELETE FROM imports`, `DELETE FROM symbols`, `DELETE FROM files`,
		`DELETE FROM symbols_fts`,
	} {
		if _, err := t.tx.Exec(q); err != nil {
			return err
		}
	}
	return nil
}

// FileHashes returns path -> content hash for every indexed file. A
// test-only file's hash is prefixed with "test:", so a file indexed in the
// other mode (with or without --include-tests) never looks unchanged.
func (t *Tx) FileHashes() (map[string]string, error) {
	rows, err := t.tx.Query(`SELECT path, hash, test FROM files`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var p, h string
		var test bool
		if err := rows.Scan(&p, &h, &test); err != nil {
			return nil, err
		}
		if test {
			h = "test:" + h
		}
		out[p] = h
	}
	return out, rows.Err()
}

// FileShapes returns path -> shape for every indexed file.
func (t *Tx) FileShapes() (map[string]string, error) {
	rows, err := t.tx.Query(`SELECT path, shape FROM files`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var p, s string
		if err := rows.Scan(&p, &s); err != nil {
			return nil, err
		}
		out[p] = s
	}
	return out, rows.Err()
}

// UpdateFile replaces the contents of an indexed file whose shape did not
// change, in place: its symbols and test functions keep their ids, so the
// edges, test edges and type edges pointing at them stay valid. Its
// symbols' lines, bodies and docs are updated, its call sites, routes and
// contracts replaced, and the edges and test edges from its functions
// removed, to be resolved again from its new call sites. The caller adds
// those (AddRawCall, AddEndpoint, AddContract) and resolves them.
func (t *Tx) UpdateFile(f File, symbols []Symbol, tests []TestFunc) (int64, error) {
	var id int64
	if err := t.tx.QueryRow(`SELECT id FROM files WHERE path = ?`, f.Path).Scan(&id); err != nil {
		return 0, err
	}
	if _, err := t.exec(`UPDATE files SET doc = ?, hash = ?, generated = ? WHERE id = ?`, f.Doc, f.Hash, f.Generated, id); err != nil {
		return 0, err
	}
	ids, err := t.ids(`SELECT id FROM symbols WHERE file_id = ? ORDER BY id`, id)
	if err != nil {
		return 0, err
	}
	if len(ids) != len(symbols) {
		return 0, fmt.Errorf("%s: %d symbols stored, %d parsed for an unchanged shape", f.Path, len(ids), len(symbols))
	}
	for i, sym := range symbols {
		if _, err := t.exec(`UPDATE symbols SET doc = ?, start_line = ?, end_line = ?, body = ? WHERE id = ?`,
			sym.Doc, sym.StartLine, sym.EndLine, sym.Body, ids[i]); err != nil {
			return 0, err
		}
		if _, err := t.exec(`DELETE FROM symbols_fts WHERE `+t.tx.d.ftsKey()+` = ?`, ids[i]); err != nil {
			return 0, err
		}
		if _, err := t.exec(t.tx.d.ftsInsert(),
			ids[i], sym.Name, sym.Qualified, SplitWords(sym.Qualified), sym.Signature, sym.Doc); err != nil {
			return 0, err
		}
	}
	tids, err := t.ids(`SELECT id FROM test_funcs WHERE file_id = ? ORDER BY id`, id)
	if err != nil {
		return 0, err
	}
	if len(tids) != len(tests) {
		return 0, fmt.Errorf("%s: %d test functions stored, %d parsed for an unchanged shape", f.Path, len(tids), len(tests))
	}
	for i, tf := range tests {
		if _, err := t.exec(`UPDATE test_funcs SET line = ? WHERE id = ?`, tf.Line, tids[i]); err != nil {
			return 0, err
		}
	}
	for _, q := range []string{
		`DELETE FROM edges WHERE from_id IN (SELECT id FROM symbols WHERE file_id = ?)`,
		`DELETE FROM test_edges WHERE from_test IN (SELECT id FROM test_funcs WHERE file_id = ?)`,
		`DELETE FROM raw_calls WHERE file_id = ?`,
		`DELETE FROM endpoints WHERE file_id = ?`,
		`DELETE FROM contracts WHERE file_id = ?`,
		`DELETE FROM properties WHERE file_id = ?`,
	} {
		if _, err := t.exec(q, id); err != nil {
			return 0, err
		}
	}
	return id, nil
}

func (t *Tx) ids(q string, args ...any) ([]int64, error) {
	rows, err := t.tx.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// AddEdges inserts edges, without removing any.
func (t *Tx) AddEdges(edges []Edge) error {
	stmt, err := t.tx.Prepare(`INSERT INTO edges(from_id, to_id, to_ref, line, confidence) VALUES(?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, e := range edges {
		var to any
		if e.ToID != 0 {
			to = e.ToID
		}
		if _, err := stmt.Exec(e.FromID, to, e.ToRef, e.Line, e.Confidence); err != nil {
			return fmt.Errorf("edge %d -> %s: %w", e.FromID, e.ToRef, err)
		}
	}
	return nil
}

// AddTestEdges inserts test edges, without removing any.
func (t *Tx) AddTestEdges(edges []TestEdge) error {
	stmt, err := t.tx.Prepare(`INSERT INTO test_edges(from_test, to_id, to_test, line) VALUES(?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	nullable := func(id int64) any {
		if id == 0 {
			return nil
		}
		return id
	}
	for _, e := range edges {
		if _, err := stmt.Exec(e.FromTest, nullable(e.ToID), nullable(e.ToTest), e.Line); err != nil {
			return fmt.Errorf("test edge %d: %w", e.FromTest, err)
		}
	}
	return nil
}

// ResolvedEdges counts the edges into the repository's own symbols.
func (t *Tx) ResolvedEdges() (int, error) {
	var n int
	err := t.tx.QueryRow(`SELECT COUNT(*) FROM edges WHERE to_id IS NOT NULL`).Scan(&n)
	return n, err
}

// DeleteFile removes a file and everything derived from it. Edges pointing
// at its symbols are removed by the cascade; edges are rebuilt afterwards.
func (t *Tx) DeleteFile(path string) error {
	if _, err := t.tx.Exec(`DELETE FROM symbols_fts WHERE `+t.tx.d.ftsKey()+` IN
		(SELECT s.id FROM symbols s JOIN files f ON f.id = s.file_id WHERE f.path = ?)`, path); err != nil {
		return err
	}
	_, err := t.tx.Exec(`DELETE FROM files WHERE path = ?`, path)
	return err
}

// AddFile inserts a file and returns its id.
func (t *Tx) AddFile(f File) (int64, error) {
	return t.insertID(`INSERT INTO files(path, dir, lang, package, qual, doc, hash, test, generated, shape)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, f.Path, f.Dir, f.Lang, f.Package, f.Qual, f.Doc, f.Hash, f.Test, f.Generated, f.Shape)
}

// AddSymbol inserts a symbol and its full-text search row.
func (t *Tx) AddSymbol(fileID int64, sym Symbol) error {
	id, err := t.insertID(`INSERT INTO symbols
		(file_id, package, name, qualified, kind, signature, doc, start_line, end_line, exported, body, result, arity)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		fileID, sym.Package, sym.Name, sym.Qualified, sym.Kind, sym.Signature, sym.Doc,
		sym.StartLine, sym.EndLine, sym.Exported, sym.Body, sym.Result, sym.Arity)
	if err != nil {
		return err
	}
	_, err = t.exec(t.tx.d.ftsInsert(), id, sym.Name, sym.Qualified, SplitWords(sym.Qualified), sym.Signature, sym.Doc)
	return err
}

// AddImport records an import statement of a file.
func (t *Tx) AddImport(fileID int64, imp Import) error {
	_, err := t.exec(`INSERT INTO imports(file_id, path, alias) VALUES(?, ?, ?)`, fileID, imp.Path, imp.Alias)
	return err
}

// AddField records a struct or class field with its type.
func (t *Tx) AddField(fileID int64, f Field) error {
	_, err := t.exec(`INSERT INTO fields(file_id, owner, name, type_ref, type_expr) VALUES(?, ?, ?, ?, ?)`,
		fileID, f.Owner, f.Name, f.TypeRef, f.TypeExpr)
	return err
}

// AddVar records a package-level variable.
func (t *Tx) AddVar(fileID int64, v Var) error {
	_, err := t.exec(`INSERT INTO vars(file_id, qualified, expr) VALUES(?, ?, ?)`, fileID, v.Qualified, v.Expr)
	return err
}

// AddSupertype records a declared supertype of a type.
func (t *Tx) AddSupertype(fileID int64, st Supertype) error {
	_, err := t.exec(`INSERT INTO supertypes(file_id, owner, kind, type_ref) VALUES(?, ?, ?, ?)`,
		fileID, st.Owner, st.Kind, st.TypeRef)
	return err
}

// AddTestFunc records a function declared in a test file.
func (t *Tx) AddTestFunc(fileID int64, tf TestFunc) error {
	_, err := t.exec(`INSERT INTO test_funcs(file_id, qualified, line, entry) VALUES(?, ?, ?, ?)`,
		fileID, tf.Qualified, tf.Line, tf.Entry)
	return err
}

// AddEndpoint records an HTTP route.
func (t *Tx) AddEndpoint(fileID int64, e Endpoint) error {
	_, err := t.exec(`INSERT INTO endpoints(file_id, method, path, handler, framework, line) VALUES(?, ?, ?, ?, ?, ?)`,
		fileID, e.Method, e.Path, e.Handler, e.Framework, e.Line)
	return err
}

// AddProperty records a key and value of a configuration file.
func (t *Tx) AddProperty(fileID int64, p Property) error {
	_, err := t.exec(`INSERT INTO properties(file_id, key, value, profile, line) VALUES(?, ?, ?, ?, ?)`,
		fileID, p.Key, p.Value, p.Profile, p.Line)
	return err
}

// AddRawCall records an unresolved call site.
func (t *Tx) AddRawCall(fileID int64, c RawCall) error {
	_, err := t.exec(`INSERT INTO raw_calls(file_id, caller, form, a, b, c, line, args) VALUES(?, ?, ?, ?, ?, ?, ?, ?)`,
		fileID, c.Caller, c.Form, c.A, c.B, c.C, c.Line, c.Args)
	return err
}

// ResolveInput is everything the resolver needs, loaded in bulk.
type ResolveInput struct {
	Module    string                // Go module path, "" if none
	Symbols   map[string]int64      // qualified name -> id
	Qualified map[int64]string      // id -> qualified name
	Kinds     map[int64]string      // id -> kind
	Results   map[int64]string      // id -> declared result type reference (Java, Python)
	Overloads map[string][]Overload // qualified name -> every declaration with it
	Methods   map[string][]int64    // short method name -> ids
	Langs     map[int64]string      // id -> language of the file declaring it
	Types     map[string][]string   // short type name -> qualified names
	DirQual   map[string]string     // directory -> qualifier (Go packages)
	Fields    map[[2]string]string  // (owner, field) -> type ref
	Calls     []RawCall
	CallFiles []string         // file path per call, parallel to Calls
	TestFuncs map[string]int64 // qualified name -> test_funcs id
	// ReExports maps a Python module attribute bound by an import, such as
	// "requests.get" after `from .api import get` in requests/__init__.py,
	// to the imported name ("requests.api.get").
	ReExports map[string]string
}

// LoadResolveInput reads symbols, fields and raw calls for resolution.
//
// With files given, only the call sites of those files are loaded: the
// calls to resolve again after an edit that kept every file's shape.
func (t *Tx) LoadResolveInput(module string, files ...string) (*ResolveInput, error) {
	in := &ResolveInput{
		Module:    module,
		Symbols:   map[string]int64{},
		Kinds:     map[int64]string{},
		Qualified: map[int64]string{},
		Methods:   map[string][]int64{},
		Langs:     map[int64]string{},
		Types:     map[string][]string{},
		DirQual:   map[string]string{},
		Fields:    map[[2]string]string{},
		TestFuncs: map[string]int64{},
		ReExports: map[string]string{},
		Results:   map[int64]string{},
		Overloads: map[string][]Overload{},
	}
	rows, err := t.tx.Query(`SELECT s.id, s.name, s.qualified, s.kind, s.result, s.arity, f.lang
		FROM symbols s JOIN files f ON f.id = s.file_id ORDER BY s.id`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id int64
		var name, q, kind, result, arity, lang string
		if err := rows.Scan(&id, &name, &q, &kind, &result, &arity, &lang); err != nil {
			rows.Close()
			return nil, err
		}
		in.Overloads[q] = append(in.Overloads[q], Overload{ID: id, Arity: arity})
		if result != "" {
			in.Results[id] = result
		}
		if _, dup := in.Symbols[q]; !dup {
			in.Symbols[q] = id
		}
		in.Kinds[id] = kind
		in.Qualified[id] = q
		in.Langs[id] = lang
		switch kind {
		case "method", "interface method", "constructor":
			in.Methods[name] = append(in.Methods[name], id)
		case "struct", "interface", "type", "class", "enum", "record":
			// The same type can be declared twice under one name (Guava
			// keeps android/ and guava/ copies); that is not ambiguity.
			if !slices.Contains(in.Types[name], q) {
				in.Types[name] = append(in.Types[name], q)
			}
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = t.tx.Query(`SELECT DISTINCT dir, qual FROM files WHERE lang = 'go' AND test = 0`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var d, q string
		if err := rows.Scan(&d, &q); err != nil {
			rows.Close()
			return nil, err
		}
		in.DirQual[d] = q
	}
	rows.Close()

	rows, err = t.tx.Query(`SELECT owner, name, type_ref FROM fields`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var o, n, tr string
		if err := rows.Scan(&o, &n, &tr); err != nil {
			rows.Close()
			return nil, err
		}
		in.Fields[[2]string{o, n}] = tr
	}
	rows.Close()

	rows, err = t.tx.Query(`SELECT f.qual, i.alias, i.path FROM imports i JOIN files f ON f.id = i.file_id
		WHERE f.lang = 'python' AND f.test = 0 AND i.path != i.alias`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var mod, alias, target string
		if err := rows.Scan(&mod, &alias, &target); err != nil {
			rows.Close()
			return nil, err
		}
		in.ReExports[mod+"."+alias] = target
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = t.tx.Query(`SELECT id, qualified FROM test_funcs ORDER BY id`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id int64
		var q string
		if err := rows.Scan(&id, &q); err != nil {
			rows.Close()
			return nil, err
		}
		if _, dup := in.TestFuncs[q]; !dup {
			in.TestFuncs[q] = id
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// File paths by id, each string shared by all of the file's calls.
	paths := map[int64]string{}
	rows, err = t.tx.Query(`SELECT id, path FROM files`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id int64
		var p string
		if err := rows.Scan(&id, &p); err != nil {
			rows.Close()
			return nil, err
		}
		paths[id] = p
	}
	rows.Close()
	q := `SELECT r.file_id, r.caller, r.form, r.a, r.b, r.c, r.line, r.args FROM raw_calls r`
	var args []any
	if len(files) > 0 {
		q += ` JOIN files f ON f.id = r.file_id WHERE f.path IN (` + strings.TrimSuffix(strings.Repeat("?,", len(files)), ",") + `)`
		for _, f := range files {
			args = append(args, f)
		}
	}
	rows, err = t.tx.Query(q+` ORDER BY r.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var c RawCall
		var file int64
		if err := rows.Scan(&file, &c.Caller, &c.Form, &c.A, &c.B, &c.C, &c.Line, &c.Args); err != nil {
			return nil, err
		}
		in.Calls = append(in.Calls, c)
		in.CallFiles = append(in.CallFiles, paths[file])
	}
	return in, rows.Err()
}

// Overload is one declaration of a qualified name, with its arity.
type Overload struct {
	ID    int64
	Arity string
}

// Fits reports whether a call with args arguments (-1: unknown) can call
// this declaration.
func (o Overload) Fits(args int) bool {
	if args < 0 || o.Arity == "" {
		return true
	}
	if n, ok := strings.CutSuffix(o.Arity, "+"); ok {
		min, err := strconv.Atoi(n)
		return err != nil || args >= min
	}
	n, err := strconv.Atoi(o.Arity)
	return err != nil || args == n
}

// Edge is a resolved (or external) call.
type Edge struct {
	FromID     int64
	ToID       int64 // 0 when the callee is outside the index
	ToRef      string
	Line       int
	Confidence string // "exact" or "name"
}

// TestEdge is a call from a test function, into the repository's code
// (ToID) or into another test function such as a helper (ToTest).
type TestEdge struct {
	FromTest int64
	ToID     int64
	ToTest   int64
	Line     int
}

// ReplaceTestEdges makes the test edges table hold exactly the given
// edges, writing only the difference (see ReplaceEdges).
func (t *Tx) ReplaceTestEdges(edges []TestEdge) error {
	want := make(map[TestEdge]int, len(edges))
	for _, e := range edges {
		want[e]++
	}
	rows, err := t.tx.Query(`SELECT id, from_test, COALESCE(to_id, 0), COALESCE(to_test, 0), line FROM test_edges`)
	if err != nil {
		return err
	}
	var stale []int64
	for rows.Next() {
		var id int64
		var e TestEdge
		if err := rows.Scan(&id, &e.FromTest, &e.ToID, &e.ToTest, &e.Line); err != nil {
			rows.Close()
			return err
		}
		if want[e] > 0 {
			want[e]--
		} else {
			stale = append(stale, id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if err := t.deleteRows("test_edges", stale); err != nil {
		return err
	}
	var add []TestEdge
	for _, e := range edges {
		if want[e] > 0 {
			want[e]--
			add = append(add, e)
		}
	}
	return t.AddTestEdges(add)
}

// ReplaceEdges makes the edges table hold exactly the given edges. Only
// the difference is written: after an edit most edges are unchanged, and
// deleting and re-inserting hundreds of thousands of rows (and their
// indexes) would cost more than reading them.
func (t *Tx) ReplaceEdges(edges []Edge) error {
	type key struct {
		from, to int64
		ref      string
		line     int
		conf     string
	}
	want := make(map[key]int, len(edges)) // how many of each are wanted
	for _, e := range edges {
		want[key{e.FromID, e.ToID, e.ToRef, e.Line, e.Confidence}]++
	}
	rows, err := t.tx.Query(`SELECT id, from_id, COALESCE(to_id, 0), to_ref, line, confidence FROM edges`)
	if err != nil {
		return err
	}
	var stale []int64
	for rows.Next() {
		var id int64
		var k key
		if err := rows.Scan(&id, &k.from, &k.to, &k.ref, &k.line, &k.conf); err != nil {
			rows.Close()
			return err
		}
		if want[k] > 0 {
			want[k]-- // already stored
		} else {
			stale = append(stale, id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if err := t.deleteRows("edges", stale); err != nil {
		return err
	}
	var add []Edge
	for _, e := range edges {
		k := key{e.FromID, e.ToID, e.ToRef, e.Line, e.Confidence}
		if want[k] > 0 {
			want[k]--
			add = append(add, e)
		}
	}
	return t.AddEdges(add)
}

// deleteRows deletes rows of a table by id, a few hundred at a time.
func (t *Tx) deleteRows(table string, ids []int64) error {
	for len(ids) > 0 {
		n := min(len(ids), 500)
		args := make([]any, n)
		for i, id := range ids[:n] {
			args[i] = id
		}
		q := `DELETE FROM ` + table + ` WHERE id IN (` + strings.TrimSuffix(strings.Repeat("?,", n), ",") + `)`
		if _, err := t.tx.Exec(q, args...); err != nil {
			return err
		}
		ids = ids[n:]
	}
	return nil
}
