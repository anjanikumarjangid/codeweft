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
	tx    *sql.Tx
	stmts map[string]*sql.Stmt
}

// exec runs a statement through a cached prepared statement, which matters
// when indexing hundreds of thousands of rows.
func (t *Tx) exec(query string, args ...any) (sql.Result, error) {
	st, ok := t.stmts[query]
	if !ok {
		var err error
		if st, err = t.tx.Prepare(query); err != nil {
			return nil, err
		}
		t.stmts[query] = st
	}
	return st.Exec(args...)
}

// Begin starts a write batch.
func (s *Store) Begin() (*Tx, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	return &Tx{tx: tx, stmts: map[string]*sql.Stmt{}}, nil
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

// DeleteFile removes a file and everything derived from it. Edges pointing
// at its symbols are removed by the cascade; edges are rebuilt afterwards.
func (t *Tx) DeleteFile(path string) error {
	if _, err := t.tx.Exec(`DELETE FROM symbols_fts WHERE rowid IN
		(SELECT s.id FROM symbols s JOIN files f ON f.id = s.file_id WHERE f.path = ?)`, path); err != nil {
		return err
	}
	_, err := t.tx.Exec(`DELETE FROM files WHERE path = ?`, path)
	return err
}

// AddFile inserts a file and returns its id.
func (t *Tx) AddFile(f File) (int64, error) {
	res, err := t.exec(`INSERT INTO files(path, dir, lang, package, qual, doc, hash, test, generated)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)`, f.Path, f.Dir, f.Lang, f.Package, f.Qual, f.Doc, f.Hash, f.Test, f.Generated)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// AddSymbol inserts a symbol and its full-text search row.
func (t *Tx) AddSymbol(fileID int64, sym Symbol) error {
	res, err := t.exec(`INSERT INTO symbols
		(file_id, package, name, qualified, kind, signature, doc, start_line, end_line, exported, body, result, arity)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		fileID, sym.Package, sym.Name, sym.Qualified, sym.Kind, sym.Signature, sym.Doc,
		sym.StartLine, sym.EndLine, sym.Exported, sym.Body, sym.Result, sym.Arity)
	if err != nil {
		return err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return err
	}
	_, err = t.exec(`INSERT INTO symbols_fts(rowid, name, qualified, terms, signature, doc)
		VALUES (?, ?, ?, ?, ?, ?)`, id, sym.Name, sym.Qualified, SplitWords(sym.Qualified), sym.Signature, sym.Doc)
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
func (t *Tx) LoadResolveInput(module string) (*ResolveInput, error) {
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

	rows, err = t.tx.Query(`SELECT f.path, r.caller, r.form, r.a, r.b, r.c, r.line, r.args
		FROM raw_calls r JOIN files f ON f.id = r.file_id ORDER BY r.rowid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var c RawCall
		var p string
		if err := rows.Scan(&p, &c.Caller, &c.Form, &c.A, &c.B, &c.C, &c.Line, &c.Args); err != nil {
			return nil, err
		}
		in.Calls = append(in.Calls, c)
		in.CallFiles = append(in.CallFiles, p)
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

// ReplaceTestEdges deletes all test edges and writes the given ones.
func (t *Tx) ReplaceTestEdges(edges []TestEdge) error {
	if _, err := t.tx.Exec(`DELETE FROM test_edges`); err != nil {
		return err
	}
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

// ReplaceEdges deletes all edges and writes the given ones.
func (t *Tx) ReplaceEdges(edges []Edge) error {
	if _, err := t.tx.Exec(`DELETE FROM edges`); err != nil {
		return err
	}
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
