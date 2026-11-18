package store

import (
	"fmt"
	"strings"
)

// Contract kinds: the ways services talk to each other.
const (
	ContractHTTP             = "http"
	ContractGRPC             = "grpc"
	ContractKafka            = "kafka"
	ContractTemporalWorkflow = "temporal-workflow"
	ContractTemporalActivity = "temporal-activity"
)

// Contract roles. A link goes from a use to what provides it: an HTTP
// client call to the route serving it, a producer to the consumers of its
// topic, the code starting a workflow to the workflow.
const (
	RoleProvides = "provides"
	RoleUses     = "uses"
)

// Contract is a point where code meets another service, or another part
// of the same one: a route it serves, a URL it calls, a topic it consumes
// or produces, a workflow it defines or starts.
type Contract struct {
	Kind   string `json:"kind"`
	Role   string `json:"role"`
	Name   string `json:"name"`             // the key links match on: a path pattern, topic, workflow name
	Method string `json:"method,omitempty"` // HTTP method, "" when not known
	Symbol string `json:"symbol"`           // qualified name of the function providing or using it
	Line   int    `json:"line"`
	Detail string `json:"detail,omitempty"` // as written: the URL template, a task queue
	// Exact is true when Name was written out in full in the code; false
	// when parts of it come from values the index cannot see (a base URL
	// from configuration, a topic name from a variable).
	Exact bool `json:"exact"`
}

// ContractRow is a stored contract with its file.
type ContractRow struct {
	Contract
	File string `json:"file"`
}

// AddContract records a contract found in a file.
func (t *Tx) AddContract(fileID int64, c Contract) error {
	_, err := t.exec(`INSERT INTO contracts(file_id, kind, role, name, method, symbol, line, detail, exact)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		fileID, c.Kind, c.Role, c.Name, c.Method, c.Symbol, c.Line, c.Detail, c.Exact)
	return err
}

// Contracts returns every contract in the index, optionally of one kind.
func (s *Store) Contracts(kind string) ([]ContractRow, error) {
	q := `SELECT c.kind, c.role, c.name, c.method, c.symbol, c.line, c.detail, c.exact, f.path
		FROM contracts c JOIN files f ON f.id = c.file_id`
	var args []any
	if kind != "" {
		q += ` WHERE c.kind = ?`
		args = append(args, kind)
	}
	rows, err := s.db.Query(q+` ORDER BY c.kind, c.name, f.path, c.line`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ContractRow{}
	for rows.Next() {
		var c ContractRow
		if err := rows.Scan(&c.Kind, &c.Role, &c.Name, &c.Method, &c.Symbol, &c.Line, &c.Detail, &c.Exact, &c.File); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// A route's contract is recorded where the route is registered; it is
	// reported at its handler's definition, as list_apis does, resolving a
	// handler written as a method on a local variable ("h.getOrder").
	for i := range out {
		if out[i].Role != RoleProvides || out[i].Kind != ContractHTTP {
			continue
		}
		var path string
		var line int
		err := s.db.QueryRow(`SELECT f.path, s.start_line FROM symbols s JOIN files f ON f.id = s.file_id
			WHERE s.qualified = ? ORDER BY s.id LIMIT 1`, out[i].Symbol).Scan(&path, &line)
		if err != nil {
			q, _, ok := s.handlerByName(out[i].Symbol)
			if !ok {
				continue
			}
			out[i].Symbol = q
			if s.db.QueryRow(`SELECT f.path, s.start_line FROM symbols s JOIN files f ON f.id = s.file_id
				WHERE s.qualified = ? ORDER BY s.id LIMIT 1`, q).Scan(&path, &line) != nil {
				continue
			}
		}
		out[i].File, out[i].Line = path, line
	}
	return out, nil
}

// NormalizePath reduces an HTTP route or a client URL to the pattern links
// compare: scheme, host, query and fragment are dropped, and every segment
// that is a parameter ({id}, :id, <int:id>, *rest) or contains a value the
// index cannot see ("*") becomes "*". It returns "" when no path is known
// (a URL that is entirely a variable).
func NormalizePath(p string) string {
	p = strings.TrimSpace(p)
	if i := strings.Index(p, "://"); i >= 0 {
		rest := p[i+3:]
		j := strings.IndexByte(rest, '/')
		if j < 0 {
			return "/"
		}
		p = rest[j:]
	}
	if i := strings.IndexAny(p, "?#"); i >= 0 {
		p = p[:i]
	}
	if !strings.HasPrefix(p, "/") {
		// "*/orders/*": an unknown base URL, then a path.
		i := strings.IndexByte(p, '/')
		if i < 0 {
			return ""
		}
		p = p[i:]
	}
	var segs []string
	for _, s := range strings.Split(p, "/") {
		switch {
		case s == "":
			continue
		case strings.Contains(s, "*"), strings.HasPrefix(s, ":"), strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}"),
			strings.HasPrefix(s, "<") && strings.HasSuffix(s, ">"):
			segs = append(segs, "*")
		default:
			segs = append(segs, s)
		}
	}
	return "/" + strings.Join(segs, "/")
}

// PathsMatch reports whether a client path can reach a route pattern, both
// normalized: the same number of segments, each pair equal or one of them
// "*". It also returns how many segments matched literally, so callers can
// prefer the most specific route.
func PathsMatch(client, route string) (bool, int) {
	if client == "" || route == "" {
		return false, 0
	}
	a, b := strings.Split(client, "/"), strings.Split(route, "/")
	if len(a) != len(b) {
		return false, 0
	}
	literal := 0
	for i := range a {
		switch {
		case a[i] == b[i] && a[i] != "*":
			literal++
		case a[i] == "*" || b[i] == "*":
		default:
			return false, 0
		}
	}
	return true, literal
}

// String describes a contract for text output.
func (c Contract) String() string {
	name := c.Name
	if c.Method != "" {
		name = c.Method + " " + name
	}
	return fmt.Sprintf("%s %s %s", c.Kind, c.Role, name)
}
