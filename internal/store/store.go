// Package store persists the code index in a single SQLite file.
//
// Symbols live in a normal table. A contentless FTS5 table indexes their
// searchable text (names, split name words, signatures, docs) and joins back
// by rowid, so keyword search is ranked with BM25.
//
// Call sites are stored raw (as the parser saw them) and resolved into edges
// in a separate pass, so the graph can be rebuilt after a partial re-index
// without re-parsing unchanged files.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	_ "github.com/mattn/go-sqlite3" // requires the sqlite_fts5 build tag
)

// SchemaVersion changes whenever the layout changes. An index built with a
// different version is discarded and rebuilt: it is a cache, not a source.
const SchemaVersion = "6"

const schema = `
CREATE TABLE IF NOT EXISTS meta (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS files (
	id      INTEGER PRIMARY KEY,
	path    TEXT NOT NULL UNIQUE,
	dir     TEXT NOT NULL,
	lang    TEXT NOT NULL,
	package TEXT NOT NULL,
	qual    TEXT NOT NULL,
	doc     TEXT NOT NULL,
	hash    TEXT NOT NULL,
	test    INTEGER NOT NULL DEFAULT 0 -- 1: test-only file, its functions are in test_funcs
);
CREATE INDEX IF NOT EXISTS files_dir ON files(dir);
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
	exported   INTEGER NOT NULL,
	body       TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS symbols_name ON symbols(name);
CREATE INDEX IF NOT EXISTS symbols_qualified ON symbols(qualified);
CREATE INDEX IF NOT EXISTS symbols_file ON symbols(file_id);
CREATE TABLE IF NOT EXISTS imports (
	file_id INTEGER NOT NULL REFERENCES files(id) ON DELETE CASCADE,
	path    TEXT NOT NULL,
	alias   TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS imports_file ON imports(file_id);
CREATE TABLE IF NOT EXISTS fields (
	file_id  INTEGER NOT NULL REFERENCES files(id) ON DELETE CASCADE,
	owner    TEXT NOT NULL,
	name     TEXT NOT NULL,
	type_ref TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS fields_owner ON fields(owner, name);
CREATE TABLE IF NOT EXISTS raw_calls (
	file_id INTEGER NOT NULL REFERENCES files(id) ON DELETE CASCADE,
	caller  TEXT NOT NULL,
	form    TEXT NOT NULL,
	a       TEXT NOT NULL,
	b       TEXT NOT NULL,
	c       TEXT NOT NULL,
	line    INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS raw_calls_file ON raw_calls(file_id);
CREATE TABLE IF NOT EXISTS endpoints (
	file_id   INTEGER NOT NULL REFERENCES files(id) ON DELETE CASCADE,
	method    TEXT NOT NULL,
	path      TEXT NOT NULL,
	handler   TEXT NOT NULL,
	framework TEXT NOT NULL,
	line      INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS endpoints_file ON endpoints(file_id);
CREATE TABLE IF NOT EXISTS supertypes (
	file_id  INTEGER NOT NULL REFERENCES files(id) ON DELETE CASCADE,
	owner    TEXT NOT NULL,
	kind     TEXT NOT NULL,
	type_ref TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS supertypes_file ON supertypes(file_id);
CREATE TABLE IF NOT EXISTS type_edges (
	sub_id    INTEGER NOT NULL REFERENCES symbols(id) ON DELETE CASCADE,
	super_id  INTEGER REFERENCES symbols(id) ON DELETE CASCADE,
	super_ref TEXT NOT NULL,
	how       TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS type_edges_sub ON type_edges(sub_id);
CREATE INDEX IF NOT EXISTS type_edges_super ON type_edges(super_id);
CREATE INDEX IF NOT EXISTS type_edges_ref ON type_edges(super_ref);
CREATE TABLE IF NOT EXISTS test_funcs (
	id        INTEGER PRIMARY KEY,
	file_id   INTEGER NOT NULL REFERENCES files(id) ON DELETE CASCADE,
	qualified TEXT NOT NULL,
	line      INTEGER NOT NULL,
	entry     INTEGER NOT NULL -- 1: a test the runner calls (TestXxx, @Test, test_xxx)
);
CREATE INDEX IF NOT EXISTS test_funcs_file ON test_funcs(file_id);
CREATE TABLE IF NOT EXISTS test_edges (
	from_test INTEGER NOT NULL REFERENCES test_funcs(id) ON DELETE CASCADE,
	to_id     INTEGER REFERENCES symbols(id) ON DELETE CASCADE,
	to_test   INTEGER REFERENCES test_funcs(id) ON DELETE CASCADE,
	line      INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS test_edges_to ON test_edges(to_id);
CREATE INDEX IF NOT EXISTS test_edges_to_test ON test_edges(to_test);
CREATE TABLE IF NOT EXISTS edges (
	from_id    INTEGER NOT NULL REFERENCES symbols(id) ON DELETE CASCADE,
	to_id      INTEGER REFERENCES symbols(id) ON DELETE CASCADE,
	to_ref     TEXT NOT NULL,
	line       INTEGER NOT NULL,
	confidence TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS edges_from ON edges(from_id);
CREATE INDEX IF NOT EXISTS edges_to ON edges(to_id);
CREATE INDEX IF NOT EXISTS edges_ref ON edges(to_ref);
`

// The FTS table is contentless: it only holds the search index and joins
// back to symbols by rowid. contentless_delete lets rows be removed when a
// single file is re-indexed.
const ftsSchema = `
CREATE VIRTUAL TABLE IF NOT EXISTS symbols_fts USING fts5(
	name, qualified, terms, signature, doc,
	content='', contentless_delete=1,
	tokenize='porter unicode61 tokenchars ''_'''
);
`

var allTables = []string{"symbols_fts", "test_edges", "test_funcs", "type_edges", "supertypes", "edges", "endpoints", "raw_calls", "fields", "imports", "symbols", "files", "meta"}

// Symbol is one indexed declaration.
type Symbol struct {
	ID        int64  `json:"-"`
	File      string `json:"file"`
	Lang      string `json:"lang"`
	Package   string `json:"package"`
	Name      string `json:"name"`
	Qualified string `json:"qualified"`
	Kind      string `json:"kind"`
	Signature string `json:"signature"`
	Doc       string `json:"doc,omitempty"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
	Exported  bool   `json:"exported"`
	Body      string `json:"body,omitempty"`
}

// File is one indexed source file.
type File struct {
	ID      int64
	Path    string // slash-separated, relative to the repository root
	Dir     string // slash-separated directory, "." for the root
	Lang    string
	Package string // package or module name as declared in the source
	Qual    string // prefix used for qualified symbol names in this file
	Doc     string // package or module doc comment
	Hash    string
	Test    bool // a test-only file: no symbols, its functions go to test_funcs
}

// TestFunc is a function or method declared in a test file.
type TestFunc struct {
	Qualified string
	Line      int
	Entry     bool // a test the test runner calls, not a helper
}

// Import is one import statement.
type Import struct {
	Path  string
	Alias string // local name the file uses for the import
}

// Field records that Owner (a qualified type) has a field Name of type
// TypeRef. TypeRef is either a qualified type name, or "@importpath.Type"
// for a type from another package, resolved later.
type Field struct {
	Owner   string
	Name    string
	TypeRef string
}

// Endpoint is an HTTP route found in the code.
type Endpoint struct {
	Method    string `json:"method"` // GET, POST, ... or ANY
	Path      string `json:"path"`
	Handler   string `json:"handler"` // qualified name of the handler, when known
	Framework string `json:"framework"`
	File      string `json:"file"`
	Line      int    `json:"line"`
}

// Supertype records that Owner (a qualified type) declares TypeRef as a
// supertype. TypeRef uses the same encoding as Field.TypeRef.
type Supertype struct {
	Owner   string
	Kind    string // SuperExtends, SuperImplements or SuperEmbeds
	TypeRef string
}

// How a type relates to its supertype.
const (
	SuperExtends    = "extends"    // class inheritance, or an interface extending (Go: embedding) another
	SuperImplements = "implements" // declared, as in Java
	SuperEmbeds     = "embeds"     // Go struct embedding: reuse, not subtyping
	SuperMethodSet  = "method set" // Go: has every method of the interface
)

// Call forms understood by the resolver.
const (
	CallQualified = "qualified" // A is the exact qualified callee
	CallImport    = "import"    // A is an import path, B the member
	CallField     = "field"     // A owner type ref, B field name, C method
	CallTypeName  = "typename"  // A short type name, B method (static calls)
	CallMethod    = "method"    // B method name on a value of unknown type
	CallTyped     = "typed"     // A type ref of the value, B method
)

// RawCall is a call site as the parser saw it, before resolution.
type RawCall struct {
	Caller string // qualified name of the calling symbol
	Form   string
	A, B   string
	C      string
	Line   int
}

// Store wraps the SQLite connection.
type Store struct {
	db *sql.DB
}

// Open opens (or creates) the index at path. An index written by a different
// schema version is wiped so it can be rebuilt.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite3", path+"?_foreign_keys=on&_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		return nil, err
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) migrate() error {
	var exists int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name = 'meta'`).Scan(&exists); err != nil {
		return err
	}
	if exists == 1 {
		v, err := s.Meta("schema_version")
		if err != nil || v != SchemaVersion {
			for _, t := range allTables {
				if _, err := s.db.Exec(`DROP TABLE IF EXISTS ` + t); err != nil {
					return err
				}
			}
		}
	}
	if _, err := s.db.Exec(schema + ftsSchema); err != nil {
		return fmt.Errorf("create schema: %w", err)
	}
	return s.SetMeta("schema_version", SchemaVersion)
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

// Stats returns file, symbol and resolved-edge counts.
func (s *Store) Stats() (files, symbols, edges int, err error) {
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM files WHERE test = 0`).Scan(&files); err != nil {
		return
	}
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM symbols`).Scan(&symbols); err != nil {
		return
	}
	err = s.db.QueryRow(`SELECT COUNT(*) FROM edges WHERE to_id IS NOT NULL`).Scan(&edges)
	return
}
