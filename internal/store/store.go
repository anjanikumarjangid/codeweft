// Package store persists the code index in a single SQLite file.
//
// Symbols live in a normal table. A contentless FTS5 table indexes their
// searchable text (names, split name words, signatures, docs) and joins back
// by rowid, so keyword search is ranked with BM25.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	_ "github.com/mattn/go-sqlite3" // requires the sqlite_fts5 build tag
)

const schema = `
CREATE TABLE IF NOT EXISTS meta (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS files (
	id   INTEGER PRIMARY KEY,
	path TEXT NOT NULL UNIQUE,
	lang TEXT NOT NULL,
	hash TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS symbols (
	id         INTEGER PRIMARY KEY,
	file_id    INTEGER NOT NULL REFERENCES files(id) ON DELETE CASCADE,
	package    TEXT NOT NULL,
	name       TEXT NOT NULL,
	qualified  TEXT NOT NULL,
	kind       TEXT NOT NULL,
	signature  TEXT NOT NULL,
	doc        TEXT NOT NULL,
	start_line INTEGER NOT NULL,
	end_line   INTEGER NOT NULL,
	body       TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS symbols_name ON symbols(name);
CREATE INDEX IF NOT EXISTS symbols_qualified ON symbols(qualified);
`

// The FTS table is contentless: it only holds the search index, and joins
// back to symbols by rowid. Resetting drops and recreates it.
const ftsSchema = `
CREATE VIRTUAL TABLE IF NOT EXISTS symbols_fts USING fts5(
	name, qualified, terms, signature, doc,
	content='',
	tokenize='porter unicode61 tokenchars ''_'''
);
`

// Symbol is one indexed declaration.
type Symbol struct {
	ID        int64  `json:"-"`
	File      string `json:"file"`
	Package   string `json:"package"`
	Name      string `json:"name"`
	Qualified string `json:"qualified"`
	Kind      string `json:"kind"`
	Signature string `json:"signature"`
	Doc       string `json:"doc,omitempty"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
	Body      string `json:"body,omitempty"`
}

// File is one indexed source file.
type File struct {
	Path string
	Lang string
	Hash string
}

// Store wraps the SQLite connection.
type Store struct {
	db *sql.DB
}

// Open opens (or creates) the index at path.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite3", path+"?_foreign_keys=on&_journal_mode=WAL")
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema + ftsSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("create schema: %w", err)
	}
	return &Store{db: db}, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// SetMeta stores a key/value pair, such as the indexed repository root.
func (s *Store) SetMeta(key, value string) error {
	_, err := s.db.Exec(`INSERT INTO meta(key, value) VALUES(?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// Meta returns a stored value, or "" if missing.
func (s *Store) Meta(key string) (string, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// Tx is a write batch used while indexing.
type Tx struct {
	tx *sql.Tx
}

// Begin starts a write batch. Indexing a whole repo in one transaction is
// much faster than committing per file.
func (s *Store) Begin() (*Tx, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	return &Tx{tx: tx}, nil
}

// Reset removes all indexed data, keeping the schema.
func (t *Tx) Reset() error {
	for _, q := range []string{
		`DELETE FROM symbols`,
		`DELETE FROM files`,
		`DROP TABLE IF EXISTS symbols_fts`,
		ftsSchema,
	} {
		if _, err := t.tx.Exec(q); err != nil {
			return err
		}
	}
	return nil
}

// AddFile inserts a file and returns its id.
func (t *Tx) AddFile(f File) (int64, error) {
	res, err := t.tx.Exec(`INSERT INTO files(path, lang, hash) VALUES(?, ?, ?)`, f.Path, f.Lang, f.Hash)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// AddSymbol inserts a symbol and its full-text search row.
func (t *Tx) AddSymbol(fileID int64, sym Symbol) error {
	res, err := t.tx.Exec(`INSERT INTO symbols
		(file_id, package, name, qualified, kind, signature, doc, start_line, end_line, body)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		fileID, sym.Package, sym.Name, sym.Qualified, sym.Kind, sym.Signature, sym.Doc,
		sym.StartLine, sym.EndLine, sym.Body)
	if err != nil {
		return err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return err
	}
	_, err = t.tx.Exec(`INSERT INTO symbols_fts(rowid, name, qualified, terms, signature, doc)
		VALUES (?, ?, ?, ?, ?, ?)`, id, sym.Name, sym.Qualified, SplitWords(sym.Qualified), sym.Signature, sym.Doc)
	return err
}

// Commit commits the batch.
func (t *Tx) Commit() error { return t.tx.Commit() }

// Rollback abandons the batch.
func (t *Tx) Rollback() error { return t.tx.Rollback() }

const selectSymbol = `SELECT s.id, f.path, s.package, s.name, s.qualified, s.kind,
	s.signature, s.doc, s.start_line, s.end_line, s.body
	FROM symbols s JOIN files f ON f.id = s.file_id`

func scanSymbols(rows *sql.Rows) ([]Symbol, error) {
	defer rows.Close()
	var out []Symbol
	for rows.Next() {
		var s Symbol
		if err := rows.Scan(&s.ID, &s.File, &s.Package, &s.Name, &s.Qualified, &s.Kind,
			&s.Signature, &s.Doc, &s.StartLine, &s.EndLine, &s.Body); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// SearchOptions narrows a search.
type SearchOptions struct {
	Limit int
	Kind  string // optional: func, method, struct, interface, type
}

// Search runs a ranked keyword search over symbol names, signatures and docs.
// Each word is matched as a stemmed prefix. Results matching all words come
// first; if nothing matches all of them, it falls back to matching any word.
func (s *Store) Search(query string, opts SearchOptions) ([]Symbol, error) {
	match := ftsQuery(query)
	if match == "" {
		return nil, nil
	}
	syms, err := s.search(match, opts)
	if err != nil || len(syms) > 0 || !strings.Contains(match, " ") {
		return syms, err
	}
	return s.search(strings.ReplaceAll(match, `* "`, `* OR "`), opts)
}

func (s *Store) search(match string, opts SearchOptions) ([]Symbol, error) {
	if opts.Limit <= 0 {
		opts.Limit = 10
	}
	q := selectSymbol + ` JOIN symbols_fts ON symbols_fts.rowid = s.id
		WHERE symbols_fts MATCH ?`
	args := []any{match}
	if opts.Kind != "" {
		q += ` AND s.kind = ?`
		args = append(args, opts.Kind)
	}
	// Weight names above split name words, then signatures, then doc text.
	q += ` ORDER BY bm25(symbols_fts, 10.0, 5.0, 4.0, 2.0, 1.0), s.qualified LIMIT ?`
	args = append(args, opts.Limit)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	return scanSymbols(rows)
}

// Lookup finds symbols by exact qualified name (pkg.Name or pkg.Type.Method),
// falling back to an exact short name, then a qualified-name suffix.
func (s *Store) Lookup(name string) ([]Symbol, error) {
	queries := []struct {
		where string
		arg   string
	}{
		{`s.qualified = ?`, name},
		{`s.name = ?`, name},
		{`s.qualified LIKE ? ESCAPE '\'`, "%." + escapeLike(name)},
	}
	for _, q := range queries {
		rows, err := s.db.Query(selectSymbol+` WHERE `+q.where+` ORDER BY s.qualified LIMIT 20`, q.arg)
		if err != nil {
			return nil, err
		}
		syms, err := scanSymbols(rows)
		if err != nil {
			return nil, err
		}
		if len(syms) > 0 {
			return syms, nil
		}
	}
	return nil, nil
}

// All returns every symbol ordered by file and line. Used by tests and export.
func (s *Store) All() ([]Symbol, error) {
	rows, err := s.db.Query(selectSymbol + ` ORDER BY f.path, s.start_line, s.qualified`)
	if err != nil {
		return nil, err
	}
	return scanSymbols(rows)
}

// Stats returns file and symbol counts.
func (s *Store) Stats() (files, symbols int, err error) {
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM files`).Scan(&files); err != nil {
		return
	}
	err = s.db.QueryRow(`SELECT COUNT(*) FROM symbols`).Scan(&symbols)
	return
}

// ftsQuery turns free text into a safe FTS5 query: each word becomes a quoted
// prefix term, so user input can never inject FTS5 syntax.
func ftsQuery(text string) string {
	words := strings.FieldsFunc(text, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_'
	})
	terms := make([]string, 0, len(words))
	for _, w := range words {
		terms = append(terms, `"`+w+`"*`)
	}
	return strings.Join(terms, " ")
}

// SplitWords breaks identifiers into lower-case words so that a search for
// "payment" finds ProcessPayment, payment_service and HTTPPaymentClient.
func SplitWords(ident string) string {
	var words []string
	for _, part := range strings.FieldsFunc(ident, func(r rune) bool {
		return r == '.' || r == '_' || r == '-' || r == '/' || r == ' '
	}) {
		rs := []rune(part)
		start := 0
		for i := 1; i < len(rs); i++ {
			prev, cur := rs[i-1], rs[i]
			nextLower := i+1 < len(rs) && unicode.IsLower(rs[i+1])
			if (unicode.IsLower(prev) && unicode.IsUpper(cur)) ||
				(unicode.IsUpper(prev) && unicode.IsUpper(cur) && nextLower) ||
				(unicode.IsLetter(prev) != unicode.IsLetter(cur)) {
				words = append(words, strings.ToLower(string(rs[start:i])))
				start = i
			}
		}
		words = append(words, strings.ToLower(string(rs[start:])))
	}
	return strings.Join(words, " ")
}

func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}
