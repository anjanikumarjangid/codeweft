package store

import (
	"context"
	"database/sql"
	"regexp"
	"strconv"
	"strings"
)

// dialect is the database an index lives in. Queries are written once,
// with ? placeholders; the connection wrappers below adapt them to
// PostgreSQL ($1, $2 ... placeholders, booleans stored as integers). The
// few differences that are more than that (full-text search, the schema's
// types) are written per dialect.
type dialect int

const (
	sqliteDialect dialect = iota
	postgresDialect
)

// rebind rewrites ? placeholders to $1, $2 ... for PostgreSQL, leaving
// question marks inside quoted strings alone.
func (d dialect) rebind(q string) string {
	if d != postgresDialect || !strings.Contains(q, "?") {
		return q
	}
	var b strings.Builder
	b.Grow(len(q) + 8)
	n, quoted := 0, false
	for i := 0; i < len(q); i++ {
		c := q[i]
		switch {
		case c == '\'':
			quoted = !quoted
		case c == '?' && !quoted:
			// ?NNN keeps its number; a bare ? takes the next one.
			j := i + 1
			for j < len(q) && q[j] >= '0' && q[j] <= '9' {
				j++
			}
			b.WriteByte('$')
			if j > i+1 {
				b.WriteString(q[i+1 : j])
				i = j - 1
			} else {
				n++
				b.WriteString(strconv.Itoa(n))
			}
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

// args adapts arguments: PostgreSQL's integer columns do not take Go
// booleans, as SQLite's do.
func (d dialect) args(args []any) []any {
	if d != postgresDialect {
		return args
	}
	for i, a := range args {
		if b, ok := a.(bool); ok {
			if b {
				args[i] = 1
			} else {
				args[i] = 0
			}
		}
	}
	return args
}

// conn is a database handle that adapts queries to its dialect.
type conn struct {
	db *sql.DB
	d  dialect
}

func (c *conn) Query(q string, args ...any) (*sql.Rows, error) {
	return c.db.Query(c.d.rebind(q), c.d.args(args)...)
}

func (c *conn) QueryRow(q string, args ...any) *sql.Row {
	return c.db.QueryRow(c.d.rebind(q), c.d.args(args)...)
}

func (c *conn) Exec(q string, args ...any) (sql.Result, error) {
	return c.db.Exec(c.d.rebind(q), c.d.args(args)...)
}

func (c *conn) Close() error { return c.db.Close() }

func (c *conn) Begin() (*txConn, error) {
	tx, err := c.db.Begin()
	if err != nil {
		return nil, err
	}
	return &txConn{tx: tx, d: c.d, shapes: map[string]*batch{}}, nil
}

// txConn is a transaction that adapts queries to its dialect.
//
// With PostgreSQL, where every statement is a network round trip, plain
// INSERT ... VALUES statements are queued and sent as multi-row inserts.
// Any other statement, and Commit, first sends what is queued, so reads in
// the transaction see every row written before them.
type txConn struct {
	tx *sql.Tx
	d  dialect

	shapes  map[string]*batch // by query; nil for a query that cannot be batched
	batches []*batch          // in the order first used, which respects foreign keys
	err     error             // a failed flush, reported at Commit
}

// batch is the queued rows of one INSERT statement.
type batch struct {
	prefix, tuple string // "INSERT INTO t(a, b) VALUES", "(?, ?)"
	args          []any
	rows, width   int
}

// Rows per multi-row insert: enough to amortise the round trip, and within
// PostgreSQL's limit of 65535 parameters per statement.
const batchRows, maxParams = 500, 65535

// queue adds a row to its statement's batch, reporting false when the
// statement is not a batchable insert.
func (t *txConn) queue(q string, args []any) bool {
	if t.d != postgresDialect {
		return false
	}
	b, seen := t.shapes[q]
	if !seen {
		b = parseInsert(q, len(args))
		t.shapes[q] = b
		if b != nil {
			t.batches = append(t.batches, b)
		}
	}
	if b == nil {
		return false
	}
	b.args = append(b.args, t.d.args(args)...)
	b.rows++
	if b.rows >= batchRows || (b.rows+1)*b.width > maxParams {
		t.flush()
	}
	return true
}

// parseInsert splits "INSERT INTO t(a, b) VALUES (?, ?)" into its prefix
// and row tuple, or returns nil for any other statement.
func parseInsert(q string, width int) *batch {
	i := strings.Index(q, "VALUES")
	if !strings.HasPrefix(q, "INSERT INTO") || i < 0 || width == 0 {
		return nil
	}
	tuple := strings.TrimSpace(q[i+len("VALUES"):])
	if !strings.HasPrefix(tuple, "(") || !strings.HasSuffix(tuple, ")") ||
		strings.Contains(tuple, "RETURNING") || strings.Contains(tuple, "ON CONFLICT") {
		return nil
	}
	return &batch{prefix: q[:i+len("VALUES")], tuple: tuple, width: width}
}

// flush sends every queued row, oldest batch first.
func (t *txConn) flush() {
	for _, b := range t.batches {
		if b.rows == 0 || t.err != nil {
			continue
		}
		var q strings.Builder
		q.WriteString(b.prefix)
		for i := 0; i < b.rows; i++ {
			if i > 0 {
				q.WriteByte(',')
			}
			q.WriteByte(' ')
			q.WriteString(b.tuple)
		}
		if _, err := t.tx.Exec(t.d.rebind(q.String()), b.args...); err != nil {
			t.err = err
		}
		b.args, b.rows = b.args[:0], 0
	}
}

func (t *txConn) Query(q string, args ...any) (*sql.Rows, error) {
	if t.flush(); t.err != nil {
		return nil, t.err
	}
	return t.tx.Query(t.d.rebind(q), t.d.args(args)...)
}

func (t *txConn) QueryRow(q string, args ...any) *sql.Row {
	t.flush() // an error is reported at Commit
	return t.tx.QueryRow(t.d.rebind(q), t.d.args(args)...)
}

func (t *txConn) Exec(q string, args ...any) (sql.Result, error) {
	if t.flush(); t.err != nil {
		return nil, t.err
	}
	return t.tx.Exec(t.d.rebind(q), t.d.args(args)...)
}

func (t *txConn) Prepare(q string) (*stmt, error) {
	st, err := t.tx.Prepare(t.d.rebind(q))
	if err != nil {
		return nil, err
	}
	return &stmt{st: st, d: t.d}, nil
}

func (t *txConn) Commit() error {
	if t.flush(); t.err != nil {
		t.tx.Rollback() //nolint:errcheck
		return t.err
	}
	return t.tx.Commit()
}

func (t *txConn) Rollback() error { return t.tx.Rollback() }

// stmt is a prepared statement that adapts its arguments to its dialect.
type stmt struct {
	st *sql.Stmt
	d  dialect
}

func (s *stmt) Exec(args ...any) (sql.Result, error) { return s.st.Exec(s.d.args(args)...) }
func (s *stmt) QueryRow(args ...any) *sql.Row        { return s.st.QueryRow(s.d.args(args)...) }
func (s *stmt) Close() error                         { return s.st.Close() }

// schemaFor adapts the schema, written for SQLite, to the dialect: in
// PostgreSQL, 64-bit integers and identity columns.
var textColumn = regexp.MustCompile(`\bTEXT\b`)

func (d dialect) schemaFor(s string) string {
	if d != postgresDialect {
		return s
	}
	s = strings.ReplaceAll(s, "INTEGER PRIMARY KEY", "BIGINT GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY")
	s = strings.ReplaceAll(s, "INTEGER", "BIGINT")
	// Byte order, as SQLite compares text: results then come in the same
	// order whatever the database's locale, and prefix LIKE can use indexes.
	return textColumn.ReplaceAllString(s, `TEXT COLLATE "C"`)
}

// ftsSchemaFor is the full-text search table: an FTS5 table in SQLite, a
// tsvector with a GIN index in PostgreSQL.
func (d dialect) ftsSchemaFor() string {
	if d != postgresDialect {
		return ftsSchema
	}
	return `
CREATE TABLE IF NOT EXISTS symbols_fts (
	id  BIGINT PRIMARY KEY REFERENCES symbols(id) ON DELETE CASCADE,
	doc tsvector NOT NULL
);
CREATE INDEX IF NOT EXISTS symbols_fts_doc ON symbols_fts USING GIN (doc);
`
}

// ftsKey is the column of the full-text search table holding the symbol id.
func (d dialect) ftsKey() string {
	if d == postgresDialect {
		return "id"
	}
	return "rowid"
}

// ftsInsert is the statement adding a symbol to full-text search, with its
// name, qualified name, name words, signature and doc as arguments. In
// PostgreSQL they are weighted as the SQLite ranking weights them: names
// (A), qualified names and their words (B), signatures (C), docs (D).
func (d dialect) ftsInsert() string {
	if d != postgresDialect {
		return `INSERT INTO symbols_fts(rowid, name, qualified, terms, signature, doc) VALUES (?, ?, ?, ?, ?, ?)`
	}
	return `INSERT INTO symbols_fts(id, doc) VALUES (?,
		setweight(to_tsvector('simple', ?), 'A') ||
		setweight(to_tsvector('simple', replace(?, '.', ' ')), 'B') ||
		setweight(to_tsvector('english', ?), 'B') ||
		setweight(to_tsvector('english', ?), 'C') ||
		setweight(to_tsvector('english', ?), 'D'))`
}

// tableExists reports whether the current schema has a table.
func (c *conn) tableExists(name string) (bool, error) {
	var n int
	q := `SELECT COUNT(*) FROM sqlite_master WHERE name = ?`
	if c.d == postgresDialect {
		q = `SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = current_schema() AND table_name = ?`
	}
	err := c.QueryRow(q, name).Scan(&n)
	return n > 0, err
}

// dropTable removes a table and what depends on it.
func (c *conn) dropTable(name string) error {
	q := `DROP TABLE IF EXISTS ` + name
	if c.d == postgresDialect {
		q += ` CASCADE`
	}
	_, err := c.Exec(q)
	return err
}

// withLock runs fn holding a lock on the index shared by every process
// using it: a PostgreSQL advisory lock named after the schema, so that of
// several server replicas only one indexes a repository at a time. SQLite
// is used by one process and needs none.
func (s *Store) withLock(ctx context.Context, fn func() error) error {
	if s.db.d != postgresDialect {
		return fn()
	}
	c, err := s.db.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	if _, err := c.ExecContext(ctx, `SELECT pg_advisory_lock(hashtext(current_schema()))`); err != nil {
		return err
	}
	defer c.ExecContext(context.Background(), `SELECT pg_advisory_unlock(hashtext(current_schema()))`) //nolint:errcheck
	return fn()
}

// WithIndexLock runs fn while no other process indexes this index: with
// PostgreSQL, other server replicas wait; with SQLite, it just runs fn.
func (s *Store) WithIndexLock(ctx context.Context, fn func() error) error {
	return s.withLock(ctx, fn)
}
