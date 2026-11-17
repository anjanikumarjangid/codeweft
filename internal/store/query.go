package store

import (
	"database/sql"
	"strconv"
	"strings"
	"unicode"
)

const selectSymbol = `SELECT s.id, f.path, f.lang, s.package, s.name, s.qualified, s.kind,
	s.signature, s.doc, s.start_line, s.end_line, s.exported, s.body
	FROM symbols s JOIN files f ON f.id = s.file_id`

func scanSymbols(rows *sql.Rows) ([]Symbol, error) {
	defer rows.Close()
	var out []Symbol
	for rows.Next() {
		var s Symbol
		if err := rows.Scan(&s.ID, &s.File, &s.Lang, &s.Package, &s.Name, &s.Qualified, &s.Kind,
			&s.Signature, &s.Doc, &s.StartLine, &s.EndLine, &s.Exported, &s.Body); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// SearchOptions narrows a search.
type SearchOptions struct {
	Limit int
	Kind  string // optional: func, method, struct, interface, class, ...
	Lang  string // optional: go, java, python
	Path  string // optional: only files under this path prefix
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
	if opts.Lang != "" {
		q += ` AND f.lang = ?`
		args = append(args, opts.Lang)
	}
	if opts.Path != "" {
		q += ` AND f.path LIKE ? ESCAPE '\'`
		args = append(args, escapeLike(strings.TrimPrefix(opts.Path, "./"))+"%")
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
		rows, err := s.db.Query(selectSymbol+` WHERE `+q.where+` ORDER BY s.qualified, s.id LIMIT 20`, q.arg)
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

// SameName reports whether all symbols share one qualified name, as Java
// overloads do. Callers can then treat them as a single symbol.
func SameName(syms []Symbol) bool {
	for _, s := range syms[1:] {
		if s.Qualified != syms[0].Qualified {
			return false
		}
	}
	return len(syms) > 0
}

// All returns every symbol ordered by file and line. Used by tests and export.
func (s *Store) All() ([]Symbol, error) {
	rows, err := s.db.Query(selectSymbol + ` ORDER BY f.path, s.start_line, s.qualified`)
	if err != nil {
		return nil, err
	}
	return scanSymbols(rows)
}

// Ref is one end of a call edge, as seen from a symbol.
type Ref struct {
	ID         int64  `json:"-"`
	Qualified  string `json:"qualified"`
	Kind       string `json:"kind,omitempty"`
	Location   string `json:"location,omitempty"` // file:line of the symbol
	CallLine   int    `json:"call_line"`          // line of the call site
	CallFile   string `json:"call_file"`          // file of the call site
	External   bool   `json:"external,omitempty"`
	Confidence string `json:"confidence"`
}

// Callees returns what the symbol calls, internal first.
func (s *Store) Callees(id int64, includeExternal bool) ([]Ref, error) {
	q := `SELECT COALESCE(t.id, 0), COALESCE(t.qualified, e.to_ref), COALESCE(t.kind, ''),
		COALESCE(tf.path || ':' || t.start_line, ''), e.line, ff.path, e.to_id IS NULL, e.confidence
		FROM edges e
		JOIN symbols fr ON fr.id = e.from_id JOIN files ff ON ff.id = fr.file_id
		LEFT JOIN symbols t ON t.id = e.to_id LEFT JOIN files tf ON tf.id = t.file_id
		WHERE e.from_id = ?`
	if !includeExternal {
		q += ` AND e.to_id IS NOT NULL`
	}
	q += ` ORDER BY e.to_id IS NULL, e.line`
	return s.refs(q, id)
}

// Callers returns the symbols that call the given symbol.
func (s *Store) Callers(id int64) ([]Ref, error) {
	return s.refs(`SELECT fr.id, fr.qualified, fr.kind, ff.path || ':' || fr.start_line,
		e.line, ff.path, 0, e.confidence
		FROM edges e JOIN symbols fr ON fr.id = e.from_id JOIN files ff ON ff.id = fr.file_id
		WHERE e.to_id = ? ORDER BY ff.path, e.line`, id)
}

// ExternalCallers returns internal symbols that call an external reference,
// matched by prefix (for example an import path like "database/sql").
func (s *Store) ExternalCallers(refPrefix string, limit int) ([]Ref, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.Query(`SELECT fr.id, fr.qualified, fr.kind, ff.path || ':' || fr.start_line,
		e.line, ff.path, 1, e.to_ref
		FROM edges e JOIN symbols fr ON fr.id = e.from_id JOIN files ff ON ff.id = fr.file_id
		WHERE e.to_id IS NULL AND (e.to_ref = ? OR e.to_ref LIKE ? ESCAPE '\')
		ORDER BY e.to_ref, ff.path, e.line LIMIT ?`, refPrefix, escapeLike(refPrefix)+"%", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Ref
	for rows.Next() {
		var r Ref
		var target string
		if err := rows.Scan(&r.ID, &r.Qualified, &r.Kind, &r.Location, &r.CallLine, &r.CallFile, &r.External, &target); err != nil {
			return nil, err
		}
		r.Confidence = "calls " + target
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) refs(q string, id int64) ([]Ref, error) {
	rows, err := s.db.Query(q, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Ref
	seen := map[string]bool{}
	for rows.Next() {
		var r Ref
		if err := rows.Scan(&r.ID, &r.Qualified, &r.Kind, &r.Location, &r.CallLine, &r.CallFile, &r.External, &r.Confidence); err != nil {
			return nil, err
		}
		// One entry per distinct target; the first call site is enough.
		if seen[r.Qualified] {
			continue
		}
		seen[r.Qualified] = true
		out = append(out, r)
	}
	return out, rows.Err()
}

// GraphNode is a node in a dependency tree.
type GraphNode struct {
	Ref
	Children []*GraphNode `json:"children,omitempty"`
	Repeated bool         `json:"repeated,omitempty"` // already expanded elsewhere
}

// Graph expands callers or callees of a symbol breadth-first up to depth.
// Each symbol is expanded once; later occurrences are marked Repeated, which
// also keeps recursion and cycles finite.
func (s *Store) Graph(root Symbol, direction string, depth int, includeExternal bool, maxNodes int) (*GraphNode, int, error) {
	if depth <= 0 {
		depth = 2
	}
	if maxNodes <= 0 {
		maxNodes = 200
	}
	top := &GraphNode{Ref: Ref{ID: root.ID, Qualified: root.Qualified, Kind: root.Kind,
		Location: root.File + ":" + strconv.Itoa(root.StartLine)}}
	expanded := map[int64]bool{root.ID: true}
	level := []*GraphNode{top}
	count := 0
	for d := 0; d < depth && len(level) > 0; d++ {
		var next []*GraphNode
		for _, n := range level {
			if n.External {
				continue
			}
			var refs []Ref
			var err error
			if direction == "callers" {
				refs, err = s.Callers(n.ID)
			} else {
				refs, err = s.Callees(n.ID, includeExternal)
			}
			if err != nil {
				return nil, 0, err
			}
			for _, r := range refs {
				if count >= maxNodes {
					return top, count, nil
				}
				child := &GraphNode{Ref: r}
				if r.ID != 0 && expanded[r.ID] {
					child.Repeated = true
				} else if r.ID != 0 {
					expanded[r.ID] = true
					next = append(next, child)
				}
				n.Children = append(n.Children, child)
				count++
			}
		}
		level = next
	}
	return top, count, nil
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

// FileSummary describes one file: what it declares, imports, and how it is
// connected to the rest of the repository.
type FileSummary struct {
	Path      string     `json:"path"`
	Lang      string     `json:"lang"`
	Package   string     `json:"package"`
	Doc       string     `json:"doc,omitempty"`
	Imports   []string   `json:"imports"`
	Symbols   []FileDecl `json:"symbols"`
	DependsOn []FileLink `json:"depends_on"` // other files this file calls into
	UsedBy    []FileLink `json:"used_by"`    // other files that call into this file
}

// FileDecl is a declaration listed in a file summary.
type FileDecl struct {
	Qualified string `json:"qualified"`
	Kind      string `json:"kind"`
	Signature string `json:"signature"`
	Doc       string `json:"doc,omitempty"`
	Line      int    `json:"line"`
	Exported  bool   `json:"exported"`
}

// FileLink is a file related through calls, with the number of call sites.
type FileLink struct {
	Path  string `json:"path"`
	Calls int    `json:"calls"`
}

// FindFiles returns indexed paths equal to p, or ending in "/"+p.
func (s *Store) FindFiles(p string) ([]string, error) {
	p = strings.TrimPrefix(strings.TrimSpace(p), "./")
	rows, err := s.db.Query(`SELECT path FROM files WHERE path = ? OR path LIKE ? ESCAPE '\' ORDER BY path LIMIT 20`,
		p, "%/"+escapeLike(p))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return nil, err
		}
		if path == p {
			return []string{p}, nil
		}
		out = append(out, path)
	}
	return out, rows.Err()
}

// FileSummary builds the summary of an indexed file by exact path.
func (s *Store) FileSummary(path string) (*FileSummary, error) {
	fs := &FileSummary{Path: path, Imports: []string{}, Symbols: []FileDecl{}, DependsOn: []FileLink{}, UsedBy: []FileLink{}}
	var id int64
	err := s.db.QueryRow(`SELECT id, lang, package, doc FROM files WHERE path = ?`, path).Scan(&id, &fs.Lang, &fs.Package, &fs.Doc)
	if err != nil {
		return nil, err
	}

	rows, err := s.db.Query(`SELECT path FROM imports WHERE file_id = ? ORDER BY path`, id)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			rows.Close()
			return nil, err
		}
		fs.Imports = append(fs.Imports, p)
	}
	rows.Close()

	rows, err = s.db.Query(`SELECT qualified, kind, signature, doc, start_line, exported
		FROM symbols WHERE file_id = ? ORDER BY start_line`, id)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var d FileDecl
		if err := rows.Scan(&d.Qualified, &d.Kind, &d.Signature, &d.Doc, &d.Line, &d.Exported); err != nil {
			rows.Close()
			return nil, err
		}
		if i := strings.IndexByte(d.Doc, '\n'); i >= 0 {
			d.Doc = d.Doc[:i]
		}
		fs.Symbols = append(fs.Symbols, d)
	}
	rows.Close()

	links := func(q string) ([]FileLink, error) {
		rows, err := s.db.Query(q, id, id)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		out := []FileLink{}
		for rows.Next() {
			var l FileLink
			if err := rows.Scan(&l.Path, &l.Calls); err != nil {
				return nil, err
			}
			out = append(out, l)
		}
		return out, rows.Err()
	}
	if fs.DependsOn, err = links(`SELECT tf.path, COUNT(*) FROM edges e
		JOIN symbols fr ON fr.id = e.from_id JOIN symbols t ON t.id = e.to_id JOIN files tf ON tf.id = t.file_id
		WHERE fr.file_id = ? AND t.file_id != ? GROUP BY tf.path ORDER BY COUNT(*) DESC, tf.path`); err != nil {
		return nil, err
	}
	if fs.UsedBy, err = links(`SELECT ff.path, COUNT(*) FROM edges e
		JOIN symbols t ON t.id = e.to_id JOIN symbols fr ON fr.id = e.from_id JOIN files ff ON ff.id = fr.file_id
		WHERE t.file_id = ? AND fr.file_id != ? GROUP BY ff.path ORDER BY COUNT(*) DESC, ff.path`); err != nil {
		return nil, err
	}
	return fs, nil
}
