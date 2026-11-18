package server

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/anjanikumarjangid/codeweft/internal/store"
)

// Repo is one repository a server answers about: its name, which labels
// results when several repositories are served together, and its index.
type Repo struct {
	Name  string
	Store *store.Store
}

// handlers implements the tools. st is the repository a tool works in once
// it has chosen one, and repo its name ("" when one repository is served);
// repos is every repository served when the call started, taken from set.
// labelled says results are labelled with their repository, as they are
// whenever several repositories are, or may be, served.
type handlers struct {
	st       *store.Store
	repo     string
	repos    []Repo
	set      *RepoSet
	labelled bool
}

// snapshot fixes the repositories a tool call works with.
func (h handlers) snapshot() handlers {
	h.repos = h.set.Repos()
	if len(h.repos) > 0 {
		h.st = h.repos[0].Store
	}
	return h
}

// workspace reports whether results are labelled with their repository.
func (h handlers) workspace() bool { return h.labelled || len(h.repos) > 1 }

// RepoSet is the repositories a server answers about. It is safe for
// concurrent use: a hosted server adds repositories while tools run.
type RepoSet struct {
	mu    sync.RWMutex
	repos []Repo
}

// NewRepoSet returns a set of the given repositories.
func NewRepoSet(repos ...Repo) *RepoSet {
	return &RepoSet{repos: append([]Repo(nil), repos...)}
}

// Repos returns the repositories, in the order they were added.
func (s *RepoSet) Repos() []Repo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]Repo(nil), s.repos...)
}

// Put adds a repository, or replaces the one of the same name.
func (s *RepoSet) Put(r Repo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.repos {
		if s.repos[i].Name == r.Name {
			s.repos[i] = r
			return
		}
	}
	s.repos = append(s.repos, r)
}

// in returns the handlers working in one repository.
func (h handlers) in(r Repo) handlers {
	h.st, h.repo = r.Store, r.Name
	if !h.workspace() {
		h.repo = ""
	}
	return h
}

// selected returns the repositories a tool call covers: the one named by
// its repo argument, or all of them. With one repository served the
// argument is ignored.
func (h handlers) selected(repo string) ([]handlers, error) {
	repo = strings.TrimSpace(repo)
	var out []handlers
	for _, r := range h.repos {
		if repo == "" || !h.workspace() || r.Name == repo {
			out = append(out, h.in(r))
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no repository named %q in the workspace; it has: %s", repo, strings.Join(h.repoNames(), ", "))
	}
	return out, nil
}

func (h handlers) repoNames() []string {
	names := make([]string, len(h.repos))
	for i, r := range h.repos {
		names[i] = r.Name
	}
	return names
}

// label prefixes text with the repository name in a workspace.
func label(repo, s string) string {
	if repo == "" {
		return s
	}
	return "[" + repo + "] " + s
}

// found is the symbols matching a name in one repository.
type found struct {
	h     handlers
	syms  []store.Symbol
	total int
}

// lookup finds the symbols matching name in the repositories a call covers,
// keeping the repositories where something matched.
func (h handlers) lookup(name, repo string) ([]found, error) {
	hs, err := h.selected(repo)
	if err != nil {
		return nil, err
	}
	var out []found
	for _, x := range hs {
		syms, total, err := x.st.LookupAll(name, maxCandidates)
		if err != nil {
			return nil, err
		}
		if len(syms) > 0 {
			out = append(out, found{x, syms, total})
		}
	}
	return out, nil
}

// candidates lists the symbols matching a name across repositories, for an
// ambiguous name.
func candidates(fs []found) ([]SymbolSummary, int) {
	var out []SymbolSummary
	total := 0
	for _, f := range fs {
		total += f.total
		for _, s := range f.syms {
			out = append(out, f.h.summarize(s))
		}
	}
	if len(out) > maxCandidates {
		out = out[:maxCandidates]
	}
	return out, total
}

// ambiguous reports whether a lookup matched more than one symbol: in
// several repositories, or several symbols of different names in one
// (overloads of one method are one answer).
func ambiguous(fs []found) bool {
	if len(fs) > 1 {
		return true
	}
	f := fs[0]
	return f.total > len(f.syms) || (len(f.syms) > 1 && !store.SameName(f.syms))
}

// mergeOverviews combines the overviews of several repositories into one:
// counts are summed, and packages, entry points, call hotspots and package
// dependencies are labelled with their repository and cut to the limit,
// largest first. External libraries are merged by name, so libraries the
// services share add up.
func mergeOverviews(names []string, os []*store.Overview, limit int) *store.Overview {
	if limit <= 0 {
		limit = 15
	}
	m := &store.Overview{Root: "workspace: " + strings.Join(names, ", "), Languages: []store.Count{},
		Packages: []store.PackageInfo{}, EntryPoints: []store.Ref{}, Frameworks: []store.Count{},
		Hotspots: []store.Count{}, Dependencies: []store.DirEdge{}, External: []store.Count{}}
	langs, fws, ext := map[string]int{}, map[string]int{}, map[string]int{}
	for i, o := range os {
		repo := names[i]
		m.Files += o.Files
		m.Symbols += o.Symbols
		m.Edges += o.Edges
		m.Endpoints += o.Endpoints
		for _, c := range o.Languages {
			langs[c.Name] += c.Count
		}
		for _, c := range o.Frameworks {
			fws[c.Name] += c.Count
		}
		for _, c := range o.External {
			ext[c.Name] += c.Count
		}
		for _, p := range o.Packages {
			p.Dir = label(repo, p.Dir)
			m.Packages = append(m.Packages, p)
		}
		for _, e := range o.EntryPoints {
			e.Repo = repo
			m.EntryPoints = append(m.EntryPoints, e)
		}
		for _, c := range o.Hotspots {
			c.Name = label(repo, c.Name)
			m.Hotspots = append(m.Hotspots, c)
		}
		for _, d := range o.Dependencies {
			d.From, d.To = label(repo, d.From), label(repo, d.To)
			m.Dependencies = append(m.Dependencies, d)
		}
	}
	counts := func(byName map[string]int) []store.Count {
		out := []store.Count{}
		for n, c := range byName {
			out = append(out, store.Count{Name: n, Count: c})
		}
		sortCounts(out)
		return out
	}
	m.Languages, m.Frameworks, m.External = counts(langs), counts(fws), counts(ext)
	sort.SliceStable(m.Packages, func(i, j int) bool { return m.Packages[i].Symbols > m.Packages[j].Symbols })
	sortCounts(m.Hotspots)
	sort.SliceStable(m.Dependencies, func(i, j int) bool { return m.Dependencies[i].Calls > m.Dependencies[j].Calls })
	m.Packages = m.Packages[:min(len(m.Packages), limit*2)]
	sort.SliceStable(m.Packages, func(i, j int) bool { return m.Packages[i].Dir < m.Packages[j].Dir })
	m.EntryPoints = m.EntryPoints[:min(len(m.EntryPoints), limit)]
	m.Hotspots = m.Hotspots[:min(len(m.Hotspots), limit)]
	m.Dependencies = m.Dependencies[:min(len(m.Dependencies), limit*2)]
	m.External = m.External[:min(len(m.External), limit)]
	return m
}

// sortCounts orders counts largest first, then by name.
func sortCounts(cs []store.Count) {
	sort.SliceStable(cs, func(i, j int) bool {
		if cs[i].Count != cs[j].Count {
			return cs[i].Count > cs[j].Count
		}
		return cs[i].Name < cs[j].Name
	})
}

// selectedAll returns the handlers of every repository.
func (h handlers) selectedAll() []handlers {
	out := make([]handlers, len(h.repos))
	for i, r := range h.repos {
		out[i] = h.in(r)
	}
	return out
}
