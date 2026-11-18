package store

import "fmt"

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
	const reach = `WITH RECURSIVE
		up(id, d) AS (
			SELECT ?1, 0
			UNION
			SELECT e.from_id, up.d + 1 FROM edges e JOIN up ON e.to_id = up.id WHERE up.d < ?2
		),
		t(tid, via, d) AS (
			SELECT te.from_test, te.to_id, up.d + 1 FROM test_edges te JOIN up ON te.to_id = up.id
			UNION
			SELECT te.from_test, t.via, t.d + 1 FROM test_edges te JOIN t ON te.to_test = t.tid
			WHERE t.d < ?2 + ?3
		)`
	var total int
	if err := s.db.QueryRow(reach+`SELECT COUNT(DISTINCT t.tid) FROM t JOIN test_funcs tf ON tf.id = t.tid
		WHERE tf.entry = 1`, id, callerDepth, helperDepth).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.Query(reach+`SELECT tf.qualified, f.path, tf.line, s.qualified, MIN(t.d)
		FROM t JOIN test_funcs tf ON tf.id = t.tid JOIN files f ON f.id = tf.file_id
		JOIN symbols s ON s.id = t.via
		WHERE tf.entry = 1
		GROUP BY tf.id ORDER BY MIN(t.d), tf.qualified LIMIT ?4`, id, callerDepth, helperDepth, limit)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []TestRef{}
	for rows.Next() {
		var r TestRef
		var path string
		var line int
		if err := rows.Scan(&r.Qualified, &path, &line, &r.Via, &r.Distance); err != nil {
			return nil, 0, err
		}
		r.Location = fmt.Sprintf("%s:%d", path, line)
		out = append(out, r)
	}
	return out, total, rows.Err()
}

// TestCount returns the number of test files and test entry points indexed.
func (s *Store) TestCount() (files, tests int, err error) {
	if err = s.db.QueryRow(`SELECT COUNT(DISTINCT file_id) FROM test_funcs`).Scan(&files); err != nil {
		return
	}
	err = s.db.QueryRow(`SELECT COUNT(*) FROM test_funcs WHERE entry = 1`).Scan(&tests)
	return
}
