package store

import (
	"path"
	"sort"
	"strconv"
	"strings"
)

// EndpointFilter narrows ListEndpoints.
type EndpointFilter struct {
	Path   string // substring of the route
	Method string // GET, POST, ...
	Limit  int
}

// EndpointInfo is an endpoint with the handler's location when it resolves
// to an indexed symbol.
type EndpointInfo struct {
	Endpoint
	HandlerLocation string `json:"handler_location,omitempty"`
	Repo            string `json:"repo,omitempty"` // in a workspace, the repository serving the route
}

// ListEndpoints returns HTTP routes found in the code, ordered by path.
func (s *Store) ListEndpoints(f EndpointFilter) ([]EndpointInfo, error) {
	if f.Limit <= 0 {
		f.Limit = 200
	}
	q := `SELECT e.method, e.path, e.handler, e.framework, fl.path, e.line,
		COALESCE((SELECT hf.path || ':' || h.start_line FROM symbols h JOIN files hf ON hf.id = h.file_id
		          WHERE h.qualified = e.handler ORDER BY h.id LIMIT 1), '')
		FROM endpoints e JOIN files fl ON fl.id = e.file_id WHERE 1 = 1`
	var args []any
	if f.Path != "" {
		q += ` AND e.path LIKE ? ESCAPE '\'`
		args = append(args, "%"+escapeLike(f.Path)+"%")
	}
	if f.Method != "" {
		q += ` AND e.method = ?`
		args = append(args, strings.ToUpper(f.Method))
	}
	q += ` ORDER BY e.path, e.method LIMIT ?`
	args = append(args, f.Limit)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []EndpointInfo{}
	for rows.Next() {
		var e EndpointInfo
		if err := rows.Scan(&e.Method, &e.Path, &e.Handler, &e.Framework, &e.File, &e.Line, &e.HandlerLocation); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		if out[i].HandlerLocation == "" {
			if q, loc, ok := s.handlerByName(out[i].Handler, out[i].File); ok {
				out[i].Handler, out[i].HandlerLocation = q, loc
			}
		}
	}
	return out, nil
}

// handlerByName resolves a route handler written as a method on a local
// variable ("h.createUser"): the method or function with that name, with
// its location. Among several, the one in the directory of the file
// registering the route wins, then the one in a file of its language;
// otherwise the name must be unique.
func (s *Store) handlerByName(handler, from string) (q, loc string, ok bool) {
	if !strings.Contains(handler, ".") {
		return "", "", false
	}
	name := handler[strings.LastIndex(handler, ".")+1:]
	rows, err := s.db.Query(`SELECT DISTINCT s.qualified, f.path, s.start_line FROM symbols s JOIN files f ON f.id = s.file_id
		WHERE s.name = ? AND s.kind IN ('method', 'func') ORDER BY s.qualified`, name)
	if err != nil {
		return "", "", false
	}
	defer rows.Close()
	type cand struct {
		q, path string
		line    int
	}
	var all []cand
	for rows.Next() {
		var c cand
		if rows.Scan(&c.q, &c.path, &c.line) != nil {
			return "", "", false
		}
		all = append(all, c)
	}
	pick := func(keep func(cand) bool) (cand, bool) {
		var found []cand
		seen := map[string]bool{}
		for _, c := range all {
			if keep(c) && !seen[c.q] {
				seen[c.q] = true
				found = append(found, c)
			}
		}
		if len(found) == 1 {
			return found[0], true
		}
		return cand{}, false
	}
	for _, keep := range []func(cand) bool{
		func(c cand) bool { return from != "" && path.Dir(c.path) == path.Dir(from) },
		func(c cand) bool { return from != "" && path.Ext(c.path) == path.Ext(from) },
		func(cand) bool { return true },
	} {
		if c, ok := pick(keep); ok {
			return c.q, c.path + ":" + strconv.Itoa(c.line), true
		}
	}
	return "", "", false
}

// Overview is a project-level summary built from the index.
type Overview struct {
	Root         string        `json:"root"`
	Languages    []Count       `json:"languages"`
	Files        int           `json:"files"`
	Symbols      int           `json:"symbols"`
	Edges        int           `json:"call_edges"`
	Packages     []PackageInfo `json:"packages"`
	EntryPoints  []Ref         `json:"entry_points"`
	Endpoints    int           `json:"endpoints"`
	Frameworks   []Count       `json:"frameworks"`
	Hotspots     []Count       `json:"most_called"`
	Dependencies []DirEdge     `json:"package_dependencies"`
	External     []Count       `json:"external_dependencies"`
}

// Count is a name with a number.
type Count struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// PackageInfo summarises one directory.
type PackageInfo struct {
	Dir     string `json:"dir"`
	Lang    string `json:"lang"`
	Files   int    `json:"files"`
	Symbols int    `json:"symbols"`
	Doc     string `json:"doc,omitempty"`
}

// DirEdge is the number of calls from one directory into another.
type DirEdge struct {
	From  string `json:"from"`
	To    string `json:"to"`
	Calls int    `json:"calls"`
}

func (s *Store) counts(q string, args ...any) ([]Count, error) {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Count{}
	for rows.Next() {
		var c Count
		if err := rows.Scan(&c.Name, &c.Count); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// BuildOverview summarises the whole index. limit caps each list.
func (s *Store) BuildOverview(limit int) (*Overview, error) {
	if limit <= 0 {
		limit = 15
	}
	o := &Overview{}
	var err error
	if o.Root, err = s.Meta("root"); err != nil {
		return nil, err
	}
	if o.Files, o.Symbols, o.Edges, err = s.Stats(); err != nil {
		return nil, err
	}
	if o.Languages, err = s.counts(`SELECT lang, COUNT(*) FROM files WHERE test = 0 AND lang != 'config'
		GROUP BY lang ORDER BY COUNT(*) DESC, lang`); err != nil {
		return nil, err
	}
	if o.Frameworks, err = s.counts(`SELECT framework, COUNT(*) FROM endpoints GROUP BY framework ORDER BY COUNT(*) DESC, framework`); err != nil {
		return nil, err
	}
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM endpoints`).Scan(&o.Endpoints); err != nil {
		return nil, err
	}

	rows, err := s.db.Query(`SELECT f.dir, MIN(f.lang), COUNT(DISTINCT f.id),
		(SELECT COUNT(*) FROM symbols s JOIN files f2 ON f2.id = s.file_id WHERE f2.dir = f.dir),
		COALESCE(MAX(CASE WHEN f.doc != '' THEN f.doc END), '')
		FROM files f WHERE f.test = 0 AND f.lang != 'config' GROUP BY f.dir ORDER BY 4 DESC, f.dir LIMIT ?`, limit*2)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var p PackageInfo
		if err := rows.Scan(&p.Dir, &p.Lang, &p.Files, &p.Symbols, &p.Doc); err != nil {
			rows.Close()
			return nil, err
		}
		if i := strings.IndexByte(p.Doc, '\n'); i >= 0 {
			p.Doc = p.Doc[:i]
		}
		o.Packages = append(o.Packages, p)
	}
	rows.Close()
	sort.Slice(o.Packages, func(i, j int) bool { return o.Packages[i].Dir < o.Packages[j].Dir })

	rows, err = s.db.Query(`SELECT s.id, s.qualified, s.kind, f.path || ':' || s.start_line
		FROM symbols s JOIN files f ON f.id = s.file_id
		WHERE (f.lang = 'go' AND s.kind = 'func' AND s.name = 'main' AND f.package = 'main')
		   OR (f.lang = 'java' AND s.name = 'main' AND s.signature LIKE '%static%void main%')
		   OR (s.kind = 'entry point')
		ORDER BY f.path LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var r Ref
		if err := rows.Scan(&r.ID, &r.Qualified, &r.Kind, &r.Location); err != nil {
			rows.Close()
			return nil, err
		}
		o.EntryPoints = append(o.EntryPoints, r)
	}
	rows.Close()
	if o.EntryPoints == nil {
		o.EntryPoints = []Ref{}
	}

	if o.Hotspots, err = s.counts(`SELECT t.qualified, COUNT(DISTINCT e.from_id) FROM edges e
		JOIN symbols t ON t.id = e.to_id WHERE e.from_id != e.to_id
		GROUP BY t.qualified ORDER BY 2 DESC, 1 LIMIT ?`, limit); err != nil {
		return nil, err
	}

	rows, err = s.db.Query(`SELECT ff.dir, tf.dir, COUNT(*) FROM edges e
		JOIN symbols fr ON fr.id = e.from_id JOIN files ff ON ff.id = fr.file_id
		JOIN symbols t ON t.id = e.to_id JOIN files tf ON tf.id = t.file_id
		WHERE ff.dir != tf.dir GROUP BY ff.dir, tf.dir ORDER BY 3 DESC, 1, 2 LIMIT ?`, limit*2)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var d DirEdge
		if err := rows.Scan(&d.From, &d.To, &d.Calls); err != nil {
			rows.Close()
			return nil, err
		}
		o.Dependencies = append(o.Dependencies, d)
	}
	rows.Close()
	if o.Dependencies == nil {
		o.Dependencies = []DirEdge{}
	}

	// External dependencies: unresolved call targets grouped by package.
	ext, err := s.counts(`SELECT to_ref, COUNT(*) FROM edges WHERE to_id IS NULL GROUP BY to_ref`)
	if err != nil {
		return nil, err
	}
	byPkg := map[string]int{}
	for _, c := range ext {
		byPkg[externalPackage(c.Name)] += c.Count
	}
	for k, v := range byPkg {
		o.External = append(o.External, Count{k, v})
	}
	sort.Slice(o.External, func(i, j int) bool {
		if o.External[i].Count != o.External[j].Count {
			return o.External[i].Count > o.External[j].Count
		}
		return o.External[i].Name < o.External[j].Name
	})
	if len(o.External) > limit {
		o.External = o.External[:limit]
	}
	if o.External == nil {
		o.External = []Count{}
	}
	return o, nil
}

// externalPackage strips the member from an external reference:
// "net/http.Client.Do" -> "net/http", "java.util.List.size" -> "java.util",
// "logging.info" -> "logging".
func externalPackage(ref string) string {
	if i := strings.LastIndex(ref, "/"); i >= 0 { // Go import path
		rest := ref[i+1:]
		if j := strings.IndexByte(rest, '.'); j >= 0 {
			return ref[:i+1+j]
		}
		return ref
	}
	parts := strings.Split(ref, ".")
	// Drop the member and any capitalised type names: a.b.Type.member -> a.b
	end := len(parts) - 1
	for end > 1 && parts[end-1] != "" && strings.ToUpper(parts[end-1][:1]) == parts[end-1][:1] {
		end--
	}
	if end < 1 {
		end = 1
	}
	return strings.Join(parts[:end], ".")
}
