package store

import (
	"fmt"
	"strings"
)

// TypeInput is what the resolver needs to build the type hierarchy: every
// declared supertype, and for Go the type and method declarations, with the
// package and imports of the file declaring each one, used to work out which
// types implement which interfaces.
type TypeInput struct {
	Supertypes []Supertype
	GoTypes    map[string]GoDecl            // qualified type -> declaration
	GoMethods  map[string]map[string]GoDecl // owner type -> method name -> declaration
	GoFuncs    map[string]GoDecl            // qualified function -> declaration
	GoFields   map[[2]string]GoDecl         // (owner type, field) -> type expression (in Signature)
	GoVars     map[string]GoDecl            // qualified package-level variable -> encoded expression (in Signature)
	GoFiles    map[int64]GoFile
	GoPaths    map[string]int64 // file path -> id
}

// GoDecl is a Go type or method declaration.
type GoDecl struct {
	Kind      string // struct, interface, type, method, interface method
	Signature string
	File      int64
}

// GoFile is the naming context of a Go file: its package qualifier and the
// local names of its imports.
type GoFile struct {
	Qual    string
	Imports map[string]string // local name -> import path
}

// LoadTypeInput reads declared supertypes and Go declarations for resolution.
func (t *Tx) LoadTypeInput() (*TypeInput, error) {
	in := &TypeInput{
		GoTypes:   map[string]GoDecl{},
		GoMethods: map[string]map[string]GoDecl{},
		GoFuncs:   map[string]GoDecl{},
		GoFields:  map[[2]string]GoDecl{},
		GoVars:    map[string]GoDecl{},
		GoFiles:   map[int64]GoFile{},
		GoPaths:   map[string]int64{},
	}
	rows, err := t.tx.Query(`SELECT owner, kind, type_ref FROM supertypes ORDER BY id`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var st Supertype
		if err := rows.Scan(&st.Owner, &st.Kind, &st.TypeRef); err != nil {
			rows.Close()
			return nil, err
		}
		in.Supertypes = append(in.Supertypes, st)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = t.tx.Query(`SELECT id, qual, path FROM files WHERE lang = 'go'`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id int64
		var q, p string
		if err := rows.Scan(&id, &q, &p); err != nil {
			rows.Close()
			return nil, err
		}
		in.GoFiles[id] = GoFile{Qual: q, Imports: map[string]string{}}
		in.GoPaths[p] = id
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = t.tx.Query(`SELECT i.file_id, i.path, i.alias FROM imports i
		JOIN files f ON f.id = i.file_id WHERE f.lang = 'go'`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id int64
		var p, alias string
		if err := rows.Scan(&id, &p, &alias); err != nil {
			rows.Close()
			return nil, err
		}
		in.GoFiles[id].Imports[alias] = p
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = t.tx.Query(`SELECT fl.owner, fl.name, fl.type_expr, fl.file_id FROM fields fl
		JOIN files f ON f.id = fl.file_id WHERE f.lang = 'go' AND fl.type_expr != ''`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var owner, name string
		var d GoDecl
		if err := rows.Scan(&owner, &name, &d.Signature, &d.File); err != nil {
			rows.Close()
			return nil, err
		}
		in.GoFields[[2]string{owner, name}] = d
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = t.tx.Query(`SELECT v.qualified, v.expr, v.file_id FROM vars v
		JOIN files f ON f.id = v.file_id WHERE f.test = 0`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var q string
		var d GoDecl
		if err := rows.Scan(&q, &d.Signature, &d.File); err != nil {
			rows.Close()
			return nil, err
		}
		in.GoVars[q] = d
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = t.tx.Query(`SELECT s.name, s.qualified, s.kind, s.signature, s.file_id
		FROM symbols s JOIN files f ON f.id = s.file_id
		WHERE f.lang = 'go' AND s.kind IN ('struct', 'interface', 'type', 'method', 'interface method', 'func')
		ORDER BY s.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var name, q string
		var d GoDecl
		if err := rows.Scan(&name, &q, &d.Kind, &d.Signature, &d.File); err != nil {
			return nil, err
		}
		switch d.Kind {
		case "method", "interface method":
			owner := strings.TrimSuffix(q, "."+name)
			if in.GoMethods[owner] == nil {
				in.GoMethods[owner] = map[string]GoDecl{}
			}
			in.GoMethods[owner][name] = d
		case "func":
			in.GoFuncs[q] = d
		default:
			in.GoTypes[q] = d
		}
	}
	return in, rows.Err()
}

// TypeEdge is a resolved (or external) supertype relation.
type TypeEdge struct {
	SubID    int64
	SuperID  int64 // 0 when the supertype is outside the index
	SuperRef string
	How      string // SuperExtends, SuperImplements, SuperEmbeds or SuperMethodSet
}

// ReplaceTypeEdges deletes all type edges and writes the given ones.
func (t *Tx) ReplaceTypeEdges(edges []TypeEdge) error {
	if _, err := t.tx.Exec(`DELETE FROM type_edges`); err != nil {
		return err
	}
	q := `INSERT INTO type_edges(sub_id, super_id, super_ref, how) VALUES(?, ?, ?, ?)`
	for _, e := range edges {
		var super any
		if e.SuperID != 0 {
			super = e.SuperID
		}
		if _, err := t.exec(q, e.SubID, super, e.SuperRef, e.How); err != nil {
			return fmt.Errorf("type edge %d -> %s: %w", e.SubID, e.SuperRef, err)
		}
	}
	return nil
}

// TypeLink is a type (or method) related to another through the type
// hierarchy.
type TypeLink struct {
	Qualified string `json:"qualified"`
	Kind      string `json:"kind,omitempty"`
	Location  string `json:"location,omitempty"`
	How       string `json:"how"`   // extends, implements, embeds, method set, overrides
	Depth     int    `json:"depth"` // 1 for a direct relation
	External  bool   `json:"external,omitempty"`
	Repo      string `json:"repo,omitempty"` // in a workspace, the repository of the type
}

// hierarchyLimit caps how many related types a single query returns.
const hierarchyLimit = 500

// Subtypes returns the types that extend or implement the type with this id,
// up to maxDepth levels down (implementations of implementations, subclasses
// of subclasses). Go struct embedding is not subtyping and is not followed.
func (s *Store) Subtypes(id int64, maxDepth int) ([]TypeLink, error) {
	return s.typeLinks(`WITH RECURSIVE h(id, depth, how) AS (
			SELECT sub_id, 1, how FROM type_edges WHERE super_id = ?1 AND how != 'embeds'
			UNION
			SELECT e.sub_id, h.depth + 1, e.how FROM type_edges e JOIN h ON e.super_id = h.id
			WHERE h.depth < ?2 AND e.how != 'embeds'
		)
		SELECT s.qualified, s.kind, f.path, s.start_line,
			(SELECT h2.how FROM h h2 WHERE h2.id = s.id ORDER BY h2.depth, h2.how LIMIT 1), MIN(h.depth)
		FROM h JOIN symbols s ON s.id = h.id JOIN files f ON f.id = s.file_id
		GROUP BY s.id, f.id ORDER BY MIN(h.depth), s.qualified LIMIT ?3`, id, maxDepth)
}

// Supertypes returns what the type with this id extends, implements or
// embeds, up to maxDepth levels up. Types outside the repository are listed
// by name. Embedded types are listed but not followed further.
func (s *Store) Supertypes(id int64, maxDepth int) ([]TypeLink, error) {
	links, err := s.typeLinks(`WITH RECURSIVE h(id, depth, how) AS (
			SELECT super_id, 1, how FROM type_edges WHERE sub_id = ?1 AND super_id IS NOT NULL
			UNION
			SELECT e.super_id, h.depth + 1, e.how FROM type_edges e JOIN h ON e.sub_id = h.id
			WHERE h.depth < ?2 AND e.super_id IS NOT NULL AND h.how != 'embeds'
		)
		SELECT s.qualified, s.kind, f.path, s.start_line,
			(SELECT h2.how FROM h h2 WHERE h2.id = s.id ORDER BY h2.depth, h2.how LIMIT 1), MIN(h.depth)
		FROM h JOIN symbols s ON s.id = h.id JOIN files f ON f.id = s.file_id
		GROUP BY s.id, f.id ORDER BY MIN(h.depth), s.qualified LIMIT ?3`, id, maxDepth)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Query(`SELECT super_ref, how FROM type_edges
		WHERE sub_id = ? AND super_id IS NULL ORDER BY super_ref`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		l := TypeLink{Depth: 1, External: true}
		if err := rows.Scan(&l.Qualified, &l.How); err != nil {
			return nil, err
		}
		links = append(links, l)
	}
	return links, rows.Err()
}

// ExternalSubtypes returns the repository types that extend or implement a
// type outside the repository, such as java.lang.Runnable, matched by full
// name or by a ".Name" suffix.
func (s *Store) ExternalSubtypes(ref string) ([]TypeLink, error) {
	return s.typeLinks(`SELECT s.qualified, s.kind, f.path, s.start_line, MIN(e.how), 1
		FROM type_edges e JOIN symbols s ON s.id = e.sub_id JOIN files f ON f.id = s.file_id
		WHERE e.super_id IS NULL AND (e.super_ref = ?1 OR e.super_ref LIKE ?2 ESCAPE '\')
		GROUP BY s.id, f.id ORDER BY s.qualified LIMIT ?3`, ref, "%."+escapeLike(ref))
}

func (s *Store) typeLinks(query string, a, b any) ([]TypeLink, error) {
	rows, err := s.db.Query(query, a, b, hierarchyLimit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TypeLink{}
	for rows.Next() {
		var l TypeLink
		var path string
		var line int
		if err := rows.Scan(&l.Qualified, &l.Kind, &path, &line, &l.How, &l.Depth); err != nil {
			return nil, err
		}
		l.Location = fmt.Sprintf("%s:%d", path, line)
		out = append(out, l)
	}
	return out, rows.Err()
}

// methodOwner returns the id of the type that declares method m, if the
// type is indexed.
func (s *Store) methodOwner(m Symbol) (int64, bool, error) {
	owner := strings.TrimSuffix(m.Qualified, "."+m.Name)
	if owner == m.Qualified {
		return 0, false, nil
	}
	var id int64
	err := s.db.QueryRow(`SELECT id FROM symbols WHERE qualified = ? AND kind NOT IN
		('func', 'method', 'interface method', 'constructor') ORDER BY id LIMIT 1`, owner).Scan(&id)
	if err != nil {
		return 0, false, nil //nolint:nilerr // no such type: not a method of an indexed type
	}
	return id, true, nil
}

// methodsNamed maps each type link to the method with this name declared on
// that type, keeping only types that declare one.
func (s *Store) methodsNamed(types []TypeLink, name, how string) ([]TypeLink, error) {
	out := []TypeLink{}
	for _, t := range types {
		if t.External {
			continue
		}
		var l TypeLink
		var path string
		var line int
		err := s.db.QueryRow(`SELECT s.qualified, s.kind, f.path, s.start_line FROM symbols s
			JOIN files f ON f.id = s.file_id WHERE s.qualified = ? ORDER BY s.id LIMIT 1`,
			t.Qualified+"."+name).Scan(&l.Qualified, &l.Kind, &path, &line)
		if err != nil {
			continue // the type inherits the method instead of declaring it
		}
		l.Location, l.Depth, l.How = fmt.Sprintf("%s:%d", path, line), t.Depth, how
		if how == "" { // going up: name the relation by what the method is
			l.How = "overrides"
			if l.Kind == "interface method" {
				l.How = "implements"
			}
		}
		out = append(out, l)
	}
	return out, nil
}

// MethodImplementations returns the methods that implement or override
// method m: the method with the same name declared on each subtype of the
// type that declares m.
func (s *Store) MethodImplementations(m Symbol, maxDepth int) ([]TypeLink, error) {
	owner, ok, err := s.methodOwner(m)
	if err != nil || !ok {
		return []TypeLink{}, err
	}
	subs, err := s.Subtypes(owner, maxDepth)
	if err != nil {
		return nil, err
	}
	how := "overrides"
	if m.Kind == "interface method" {
		how = "implements"
	}
	return s.methodsNamed(subs, m.Name, how)
}

// MethodSupers returns the methods that method m implements or overrides:
// the method with the same name declared on each supertype of its type.
func (s *Store) MethodSupers(m Symbol, maxDepth int) ([]TypeLink, error) {
	owner, ok, err := s.methodOwner(m)
	if err != nil || !ok {
		return []TypeLink{}, err
	}
	sups, err := s.Supertypes(owner, maxDepth)
	if err != nil {
		return nil, err
	}
	var keep []TypeLink
	for _, t := range sups {
		if t.How != SuperEmbeds {
			keep = append(keep, t)
		}
	}
	return s.methodsNamed(keep, m.Name, "")
}
