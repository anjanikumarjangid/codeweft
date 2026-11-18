package hosted

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/anjanikumarjangid/codeweft/internal/indexer"
	"github.com/anjanikumarjangid/codeweft/internal/server"
	"github.com/anjanikumarjangid/codeweft/internal/store"
)

// RepoStatus is what /status reports about a repository.
type RepoStatus struct {
	Name        string    `json:"name"`
	Source      string    `json:"source"`
	Branch      string    `json:"branch,omitempty"`
	Commit      string    `json:"commit,omitempty"`
	IndexedAt   time.Time `json:"indexed_at,omitempty"`
	Files       int       `json:"files"`
	Symbols     int       `json:"symbols"`
	CallEdges   int       `json:"call_edges"`
	Syncing     bool      `json:"syncing"`
	LastError   string    `json:"last_error,omitempty"`
	SyncSeconds float64   `json:"last_sync_seconds,omitempty"`
	Syncs       int       `json:"syncs"`
	Failures    int       `json:"failed_syncs"`
}

// repo is one served repository and its sync state.
type repo struct {
	spec    RepoSpec
	dir     string // working tree: the clone, or the local source
	trigger chan struct{}

	mu     sync.Mutex
	st     *store.Store
	status RepoStatus
}

// Manager keeps the repositories cloned and indexed, and the server's set
// of repositories current.
type Manager struct {
	cfg  Config
	set  *server.RepoSet
	log  *slog.Logger
	sem  chan struct{} // limits concurrent indexing
	ctx  context.Context
	wg   sync.WaitGroup
	mu   sync.Mutex
	byID map[string]*repo // by name

	metrics *metrics
}

// NewManager prepares the repositories of cfg; Start begins syncing them.
func NewManager(cfg Config, set *server.RepoSet, log *slog.Logger) *Manager {
	return &Manager{cfg: cfg, set: set, log: log, sem: make(chan struct{}, cfg.SyncConcurrency), byID: map[string]*repo{},
		metrics: newMetrics()}
}

// Start syncs every configured repository in the background, then keeps
// them current every SyncInterval, until ctx ends.
func (m *Manager) Start(ctx context.Context) {
	m.ctx = ctx
	for _, spec := range m.cfg.Repos {
		m.add(spec)
	}
	if m.cfg.GitHubOrg != "" {
		m.refreshOrg(ctx)
	}
	if m.cfg.SyncInterval > 0 {
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			t := time.NewTicker(m.cfg.SyncInterval)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					if m.cfg.GitHubOrg != "" {
						m.refreshOrg(ctx)
					}
					m.SyncAll()
				}
			}
		}()
	}
}

// Wait waits for the background work to stop after the context ends, and
// closes the indexes.
func (m *Manager) Wait() {
	m.wg.Wait()
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.byID {
		r.mu.Lock()
		if r.st != nil {
			r.st.Close()
		}
		r.mu.Unlock()
	}
}

// add starts serving a repository: it is synced now and whenever
// triggered. Adding a repository already served does nothing.
func (m *Manager) add(spec RepoSpec) *repo {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.byID[spec.Name]; ok {
		return r
	}
	r := &repo{spec: spec, trigger: make(chan struct{}, 1),
		status: RepoStatus{Name: spec.Name, Source: redact(spec.Source), Branch: spec.Branch}}
	r.dir = spec.Source
	if !spec.Local() {
		r.dir = filepath.Join(m.cfg.DataDir, "repos", spec.Name)
	}
	m.byID[spec.Name] = r
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		for {
			select {
			case <-m.ctx.Done():
				return
			case <-r.trigger:
				m.sync(r)
			}
		}
	}()
	r.trigger <- struct{}{}
	return r
}

// Sync asks for a repository to be synced. A sync already waiting covers
// the request, so a burst of pushes costs one sync after the running one.
func (m *Manager) Sync(name string) bool {
	m.mu.Lock()
	r, ok := m.byID[name]
	m.mu.Unlock()
	if !ok {
		return false
	}
	select {
	case r.trigger <- struct{}{}:
	default:
	}
	return true
}

// SyncAll asks for every repository to be synced.
func (m *Manager) SyncAll() {
	for _, s := range m.Statuses() {
		m.Sync(s.Name)
	}
}

// Statuses reports every repository, by name.
func (m *Manager) Statuses() []RepoStatus {
	m.mu.Lock()
	repos := make([]*repo, 0, len(m.byID))
	for _, r := range m.byID {
		repos = append(repos, r)
	}
	m.mu.Unlock()
	out := make([]RepoStatus, 0, len(repos))
	for _, r := range repos {
		r.mu.Lock()
		out = append(out, r.status)
		r.mu.Unlock()
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// find returns the repository a GitHub push to fullName's branch is for.
func (m *Manager) find(fullName, branch string) *repo {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.byID {
		if !strings.EqualFold(r.spec.FullName, fullName) {
			continue
		}
		r.mu.Lock()
		b := r.status.Branch
		r.mu.Unlock()
		if b == "" || b == branch {
			return r
		}
	}
	return nil
}

// sync brings a repository's working tree to the latest commit of its
// branch and its index up to date.
func (m *Manager) sync(r *repo) {
	select {
	case m.sem <- struct{}{}:
	case <-m.ctx.Done():
		return
	}
	defer func() { <-m.sem }()
	r.mu.Lock()
	r.status.Syncing = true
	r.mu.Unlock()
	start := time.Now()
	err := m.syncRepo(r)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status.Syncing = false
	r.status.Syncs++
	r.status.SyncSeconds = time.Since(start).Seconds()
	if err != nil {
		r.status.Failures++
		r.status.LastError = err.Error()
		m.log.Error("sync failed", "repo", r.spec.Name, "err", err)
		return
	}
	r.status.LastError = ""
}

func (m *Manager) syncRepo(r *repo) error {
	if !r.spec.Local() {
		if err := m.fetch(r); err != nil {
			return err
		}
	}
	commit := ""
	if _, err := os.Stat(filepath.Join(r.dir, ".git")); err == nil {
		out, err := m.git(r.dir, "rev-parse", "HEAD")
		if err != nil {
			return err
		}
		commit = strings.TrimSpace(out)
	}
	r.mu.Lock()
	st, indexed := r.st, r.status.Commit
	r.mu.Unlock()
	if st != nil && commit != "" && commit == indexed {
		return nil // already indexed at this commit
	}
	first := st == nil
	if first {
		db := filepath.Join(m.cfg.DataDir, "index", r.spec.Name+".db")
		if err := os.MkdirAll(filepath.Dir(db), 0o755); err != nil {
			return err
		}
		var err error
		if st, err = store.Open(db); err != nil {
			return err
		}
	}
	opts, err := indexer.StoredOptions(st)
	if err != nil {
		return err
	}
	res, err := indexer.Reindex(r.dir, st, opts)
	if err != nil {
		if first {
			st.Close()
		}
		return fmt.Errorf("index: %w", err)
	}
	files, symbols, _, err := st.Stats()
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.st = st
	r.status.Commit, r.status.IndexedAt = commit, time.Now().UTC()
	r.status.Files, r.status.Symbols, r.status.CallEdges = files, symbols, res.Edges
	r.mu.Unlock()
	if first {
		m.set.Put(server.Repo{Name: r.spec.Name, Store: st})
	}
	m.log.Info("indexed", "repo", r.spec.Name, "commit", short(commit), "files", files, "partial", res.Partial)
	return nil
}

// fetch clones the repository, or brings the clone to the latest commit
// of its branch. Clones are shallow: only the files are indexed.
func (m *Manager) fetch(r *repo) error {
	if _, err := os.Stat(filepath.Join(r.dir, ".git")); err != nil {
		if err := os.MkdirAll(filepath.Dir(r.dir), 0o755); err != nil {
			return err
		}
		os.RemoveAll(r.dir) // a clone interrupted before .git was written
		args := []string{"clone", "--quiet", "--depth", "1", "--single-branch", "--no-tags"}
		if r.spec.Branch != "" {
			args = append(args, "--branch", r.spec.Branch)
		}
		if _, err := m.git("", append(args, "--", r.spec.Source, r.dir)...); err != nil {
			os.RemoveAll(r.dir)
			return err
		}
	} else {
		branch := r.spec.Branch
		if branch == "" {
			r.mu.Lock()
			branch = r.status.Branch
			r.mu.Unlock()
		}
		if _, err := m.git(r.dir, "fetch", "--quiet", "--depth", "1", "--no-tags", "origin", branch); err != nil {
			return err
		}
		if _, err := m.git(r.dir, "reset", "--quiet", "--hard", "FETCH_HEAD"); err != nil {
			return err
		}
	}
	if r.spec.Branch == "" {
		out, err := m.git(r.dir, "rev-parse", "--abbrev-ref", "HEAD")
		if err != nil {
			return err
		}
		r.mu.Lock()
		r.status.Branch = strings.TrimSpace(out)
		r.mu.Unlock()
	}
	return nil
}

// git runs a git command. It never prompts, and a token for GitHub is
// passed as an HTTP header through the environment, so it appears neither
// in the command line nor in the clone's configuration.
func (m *Manager) git(dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(m.ctx, 15*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=true", "GCM_INTERACTIVE=never")
	if m.cfg.GitHubToken != "" {
		header := "AUTHORIZATION: basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+m.cfg.GitHubToken))
		cmd.Env = append(cmd.Env, "GIT_CONFIG_COUNT=1",
			"GIT_CONFIG_KEY_0=http."+m.cfg.GitHubURL+"/.extraheader", "GIT_CONFIG_VALUE_0="+header)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if m.cfg.GitHubToken != "" {
			msg = strings.ReplaceAll(msg, m.cfg.GitHubToken, "***")
		}
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, redact(msg))
	}
	return stdout.String(), nil
}

// userinfo matches credentials written into a URL.
var userinfo = regexp.MustCompile(`://[^/@\s]+@`)

// redact removes credentials from URLs in text.
func redact(s string) string {
	return userinfo.ReplaceAllString(s, "://***@")
}

func short(commit string) string {
	if len(commit) > 12 {
		return commit[:12]
	}
	return commit
}
