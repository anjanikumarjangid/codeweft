package store

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
)

// TestRef is a test that exercises a symbol.
type TestRef struct {
	Qualified string `json:"qualified"`
	Location  string `json:"location"`
	// Via is the repository symbol the test (or the helper it uses) calls
	// directly on the way to the symbol asked about: the symbol itself for
	// a direct test.
	Via string `json:"via"`
	// Distance counts calls from the test to the symbol: 1 when the test
	// calls it directly.
	Distance int `json:"distance"`
}

// TestsFor returns the tests that exercise the symbol with this id: test
// functions that call it, or call something that calls it (up to
// callerDepth calls through the repository's own code), directly or
// through helper functions in test files (up to helperDepth calls).
// Nearest tests come first. Only test entry points are returned, not the
// helpers they go through.
func (s *Store) TestsFor(id int64, callerDepth, helperDepth, limit int) ([]TestRef, int, error) {
	// Breadth-first, each function and test visited once at its nearest
	// distance: a recursive query would follow every call path to a
	// function called from thousands of places.
	depth := map[int64]int{id: 0} // repository functions reaching id, by calls away
	level := []int64{id}
	for d := 1; d <= callerDepth && len(level) > 0; d++ {
		var next []int64
		err := s.inChunks(level, `SELECT DISTINCT from_id FROM edges WHERE to_id IN (%s)`, func(rows *sql.Rows) error {
			var from int64
			if err := rows.Scan(&from); err != nil {
				return err
			}
			if _, seen := depth[from]; !seen {
				depth[from] = d
				next = append(next, from)
			}
			return nil
		})
		if err != nil {
			return nil, 0, err
		}
		level = next
	}
	type reach struct {
		d   int
		via int64
	}
	tests := map[int64]reach{} // test functions (entry points and helpers)
	reached := make([]int64, 0, len(depth))
	for f := range depth {
		reached = append(reached, f)
	}
	var frontier []int64
	err := s.inChunks(reached, `SELECT from_test, to_id FROM test_edges WHERE to_id IN (%s)`, func(rows *sql.Rows) error {
		var t, to int64
		if err := rows.Scan(&t, &to); err != nil {
			return err
		}
		r := reach{depth[to] + 1, to}
		if old, seen := tests[t]; !seen || r.d < old.d || (r.d == old.d && r.via < old.via) {
			if !seen {
				frontier = append(frontier, t)
			}
			tests[t] = r
		}
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	// Tests calling a helper that reaches the symbol, nearest first.
	for len(frontier) > 0 {
		var expand []int64
		for _, t := range frontier {
			if tests[t].d < callerDepth+helperDepth {
				expand = append(expand, t)
			}
		}
		var next []int64
		err := s.inChunks(expand, `SELECT from_test, to_test FROM test_edges WHERE to_test IN (%s)`, func(rows *sql.Rows) error {
			var t, helper int64
			if err := rows.Scan(&t, &helper); err != nil {
				return err
			}
			r := reach{tests[helper].d + 1, tests[helper].via}
			if old, seen := tests[t]; !seen || r.d < old.d {
				next = append(next, t)
				tests[t] = r
			}
			return nil
		})
		if err != nil {
			return nil, 0, err
		}
		frontier = next
	}
	// Only entry points: tests the runner calls, not the helpers.
	type found struct {
		TestRef
		via int64
	}
	var entries []found
	ids := make([]int64, 0, len(tests))
	for t := range tests {
		ids = append(ids, t)
	}
	err = s.inChunks(ids, `SELECT tf.id, tf.qualified, f.path, tf.line FROM test_funcs tf JOIN files f ON f.id = tf.file_id
		WHERE tf.entry = 1 AND tf.id IN (%s)`, func(rows *sql.Rows) error {
		var t int64
		var r found
		var path string
		var line int
		if err := rows.Scan(&t, &r.Qualified, &path, &line); err != nil {
			return err
		}
		r.Location = fmt.Sprintf("%s:%d", path, line)
		r.Distance, r.via = tests[t].d, tests[t].via
		entries = append(entries, r)
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Distance != entries[j].Distance {
			return entries[i].Distance < entries[j].Distance
		}
		return entries[i].Qualified < entries[j].Qualified
	})
	total := len(entries)
	if limit > 0 && len(entries) > limit {
		entries = entries[:limit]
	}
	out := make([]TestRef, 0, len(entries))
	for _, e := range entries {
		if err := s.db.QueryRow(`SELECT qualified FROM symbols WHERE id = ?`, e.via).Scan(&e.Via); err != nil {
			return nil, 0, err
		}
		out = append(out, e.TestRef)
	}
	return out, total, nil
}

// inChunks runs a query with an IN list (%s in q) over ids, a few hundred
// at a time, calling scan for each row.
func (s *Store) inChunks(ids []int64, q string, scan func(*sql.Rows) error) error {
	const chunk = 500
	for len(ids) > 0 {
		n := min(len(ids), chunk)
		args := make([]any, n)
		for i, id := range ids[:n] {
			args[i] = id
		}
		rows, err := s.db.Query(fmt.Sprintf(q, strings.TrimSuffix(strings.Repeat("?,", n), ",")), args...)
		if err != nil {
			return err
		}
		for rows.Next() {
			if err := scan(rows); err != nil {
				rows.Close()
				return err
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		ids = ids[n:]
	}
	return nil
}

// TestCount returns the number of test files and test entry points indexed.
func (s *Store) TestCount() (files, tests int, err error) {
	if err = s.db.QueryRow(`SELECT COUNT(DISTINCT file_id) FROM test_funcs`).Scan(&files); err != nil {
		return
	}
	err = s.db.QueryRow(`SELECT COUNT(*) FROM test_funcs WHERE entry = 1`).Scan(&tests)
	return
}
