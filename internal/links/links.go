// Package links matches the contracts of one or more repositories: an HTTP
// client call to the route serving it, a Kafka producer to the consumers of
// its topic, the code starting a Temporal workflow to the workflow.
package links

import (
	"sort"

	"github.com/anjanikumarjangid/codewiki/internal/store"
)

// End is one side of a link: a contract in a repository.
type End struct {
	Repo string `json:"repo,omitempty"`
	store.ContractRow
}

// Link connects a use to what provides it.
type Link struct {
	From End `json:"from"` // the use: client call, producer, workflow start
	To   End `json:"to"`   // the provider: route, consumer, workflow definition
	// Confidence is "exact" when the use's name was written out in full,
	// and "pattern" when parts of it come from values the index cannot see
	// (a base URL from configuration, a path segment from a variable).
	Confidence string `json:"confidence"`
}

// Result is every link between the given contracts, and the uses and
// providers left without a counterpart.
type Result struct {
	Links       []Link `json:"links"`
	Unmatched   []End  `json:"unmatched_uses"`      // calls whose provider is not indexed (or does not exist)
	Unreferring []End  `json:"unmatched_providers"` // routes, consumers, workflows nothing indexed uses
}

// Match links every use to the providers it can reach. An HTTP call links
// to the routes whose pattern its path matches, with a compatible method,
// keeping only the most specific ones (the most segments matched
// literally); other kinds link by equal name.
func Match(all []End) Result {
	var uses, provides []End
	for _, e := range all {
		switch e.Role {
		case store.RoleUses:
			uses = append(uses, e)
		case store.RoleProvides:
			provides = append(provides, e)
		}
	}
	res := Result{Links: []Link{}, Unmatched: []End{}, Unreferring: []End{}}
	used := make([]bool, len(provides))
	for _, u := range uses {
		best, matched := -1, []int(nil)
		for i, p := range provides {
			if p.Kind != u.Kind {
				continue
			}
			score := 0
			if u.Kind == store.ContractHTTP {
				if u.Method != "" && p.Method != "" && u.Method != p.Method {
					continue
				}
				ok, literal := store.PathsMatch(u.Name, p.Name)
				if !ok {
					continue
				}
				score = literal
			} else if u.Name != p.Name {
				continue
			}
			switch {
			case score > best:
				best, matched = score, []int{i}
			case score == best:
				matched = append(matched, i)
			}
		}
		if len(matched) == 0 {
			res.Unmatched = append(res.Unmatched, u)
			continue
		}
		conf := "exact"
		if !u.Exact {
			conf = "pattern"
		}
		for _, i := range matched {
			used[i] = true
			res.Links = append(res.Links, Link{From: u, To: provides[i], Confidence: conf})
		}
	}
	for i, p := range provides {
		if !used[i] {
			res.Unreferring = append(res.Unreferring, p)
		}
	}
	sort.SliceStable(res.Links, func(i, j int) bool {
		a, b := res.Links[i], res.Links[j]
		if a.To.Kind != b.To.Kind {
			return a.To.Kind < b.To.Kind
		}
		if a.To.Name != b.To.Name {
			return a.To.Name < b.To.Name
		}
		return a.From.Repo+a.From.Symbol < b.From.Repo+b.From.Symbol
	})
	return res
}
