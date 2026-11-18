package hosted

import (
	"fmt"
	"io"
	"sort"
	"sync"
	"time"
)

// metrics counts what /metrics reports besides the repositories' state,
// in the Prometheus text format.
type metrics struct {
	mu       sync.Mutex
	webhooks map[string]int // by result
	requests map[string]int // MCP requests, by HTTP status class
}

func newMetrics() *metrics {
	return &metrics{webhooks: map[string]int{}, requests: map[string]int{}}
}

func (m *metrics) webhook(result string) {
	m.mu.Lock()
	m.webhooks[result]++
	m.mu.Unlock()
}

func (m *metrics) request(status int) {
	m.mu.Lock()
	m.requests[fmt.Sprintf("%dxx", status/100)]++
	m.mu.Unlock()
}

// write writes every metric: the counters, and each repository's index.
func (m *metrics) write(w io.Writer, repos []RepoStatus) {
	gauge := func(name, help string) { fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n", name, help, name) }
	counter := func(name, help string) { fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n", name, help, name) }
	indexed := 0
	for _, r := range repos {
		if !r.IndexedAt.IsZero() {
			indexed++
		}
	}
	gauge("codeweft_repositories", "Repositories served, and how many have an index.")
	fmt.Fprintf(w, "codeweft_repositories{state=\"configured\"} %d\ncodeweft_repositories{state=\"indexed\"} %d\n", len(repos), indexed)
	per := []struct {
		name, help, kind string
		value            func(RepoStatus) float64
	}{
		{"codeweft_repository_files", "Source files indexed.", "gauge", func(r RepoStatus) float64 { return float64(r.Files) }},
		{"codeweft_repository_symbols", "Symbols indexed.", "gauge", func(r RepoStatus) float64 { return float64(r.Symbols) }},
		{"codeweft_repository_call_edges", "Calls resolved within the repository.", "gauge", func(r RepoStatus) float64 { return float64(r.CallEdges) }},
		{"codeweft_repository_index_age_seconds", "Time since the index was last updated.", "gauge", func(r RepoStatus) float64 {
			if r.IndexedAt.IsZero() {
				return -1
			}
			return time.Since(r.IndexedAt).Seconds()
		}},
		{"codeweft_repository_last_sync_seconds", "Duration of the last sync.", "gauge", func(r RepoStatus) float64 { return r.SyncSeconds }},
		{"codeweft_repository_syncs_total", "Syncs run.", "counter", func(r RepoStatus) float64 { return float64(r.Syncs) }},
		{"codeweft_repository_sync_failures_total", "Syncs that failed.", "counter", func(r RepoStatus) float64 { return float64(r.Failures) }},
	}
	for _, p := range per {
		if p.kind == "gauge" {
			gauge(p.name, p.help)
		} else {
			counter(p.name, p.help)
		}
		for _, r := range repos {
			fmt.Fprintf(w, "%s{repo=%q} %g\n", p.name, r.Name, p.value(r))
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	counter("codeweft_webhook_deliveries_total", "GitHub webhook deliveries, by what was done.")
	for _, k := range sorted(m.webhooks) {
		fmt.Fprintf(w, "codeweft_webhook_deliveries_total{result=%q} %d\n", k, m.webhooks[k])
	}
	counter("codeweft_mcp_requests_total", "MCP HTTP requests, by status class.")
	for _, k := range sorted(m.requests) {
		fmt.Fprintf(w, "codeweft_mcp_requests_total{status=%q} %d\n", k, m.requests[k])
	}
}

func sorted(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
