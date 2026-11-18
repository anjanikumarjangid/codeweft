package hosted

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ghRepo is the part of GitHub's repository object codeweft reads.
type ghRepo struct {
	Name          string `json:"name"`
	FullName      string `json:"full_name"`
	CloneURL      string `json:"clone_url"`
	DefaultBranch string `json:"default_branch"`
	Archived      bool   `json:"archived"`
	Disabled      bool   `json:"disabled"`
	Fork          bool   `json:"fork"`
}

// listOrg lists the repositories of a GitHub organization, or of a user
// when no organization has the name.
func (m *Manager) listOrg(ctx context.Context) ([]ghRepo, error) {
	var all []ghRepo
	for _, kind := range []string{"orgs", "users"} {
		all = all[:0]
		for page := 1; ; page++ {
			u := fmt.Sprintf("%s/%s/%s/repos?per_page=100&page=%d", m.cfg.GitHubAPIURL, kind, m.cfg.GitHubOrg, page)
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
			if err != nil {
				return nil, err
			}
			req.Header.Set("Accept", "application/vnd.github+json")
			req.Header.Set("Authorization", "Bearer "+m.cfg.GitHubToken)
			req.Header.Set("User-Agent", "codeweft")
			client := &http.Client{Timeout: 30 * time.Second}
			resp, err := client.Do(req)
			if err != nil {
				return nil, err
			}
			body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
			resp.Body.Close()
			if err != nil {
				return nil, err
			}
			if resp.StatusCode == http.StatusNotFound && kind == "orgs" && page == 1 {
				break // not an organization: try a user
			}
			if resp.StatusCode != http.StatusOK {
				return nil, fmt.Errorf("listing %s: GitHub answered %s", m.cfg.GitHubOrg, resp.Status)
			}
			var repos []ghRepo
			if err := json.Unmarshal(body, &repos); err != nil {
				return nil, fmt.Errorf("listing %s: %w", m.cfg.GitHubOrg, err)
			}
			all = append(all, repos...)
			if len(repos) < 100 {
				return all, nil
			}
		}
	}
	return nil, fmt.Errorf("no GitHub organization or user named %q", m.cfg.GitHubOrg)
}

// refreshOrg starts serving the organization's repositories not yet
// served: all of them at start, new ones later.
func (m *Manager) refreshOrg(ctx context.Context) {
	repos, err := m.listOrg(ctx)
	if err != nil {
		m.log.Error("listing the organization's repositories failed", "org", m.cfg.GitHubOrg, "err", err)
		return
	}
	for _, r := range repos {
		if r.Archived || r.Disabled || (r.Fork && !m.cfg.IncludeForks) {
			continue
		}
		m.addGitHub(r)
	}
}

// addGitHub serves a repository of the organization, unless one of the
// same name is configured already.
func (m *Manager) addGitHub(r ghRepo) *repo {
	m.mu.Lock()
	for _, existing := range m.byID {
		if strings.EqualFold(existing.spec.FullName, r.FullName) {
			m.mu.Unlock()
			return existing
		}
	}
	m.mu.Unlock()
	src := r.CloneURL
	if src == "" {
		src = m.cfg.GitHubURL + "/" + r.FullName + ".git"
	}
	return m.add(RepoSpec{Name: r.Name, Source: src, Branch: r.DefaultBranch, FullName: r.FullName})
}

// pushEvent is the part of GitHub's push event codeweft reads.
type pushEvent struct {
	Ref        string `json:"ref"`
	After      string `json:"after"`
	Deleted    bool   `json:"deleted"`
	Repository ghRepo `json:"repository"`
}

// webhook handles GitHub deliveries: a push to the branch a repository is
// served from syncs it. With an organization configured, a push to one of
// its repositories not served yet (a new one) starts serving it.
func (m *Manager) webhook(w http.ResponseWriter, r *http.Request) {
	if m.cfg.WebhookSecret == "" {
		http.Error(w, "webhooks are not enabled: set GITHUB_WEBHOOK_SECRET", http.StatusNotFound)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 25<<20))
	if err != nil {
		http.Error(w, "reading the body failed", http.StatusBadRequest)
		return
	}
	if !validSignature(m.cfg.WebhookSecret, r.Header.Get("X-Hub-Signature-256"), body) {
		m.metrics.webhook("bad_signature")
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}
	switch event := r.Header.Get("X-GitHub-Event"); event {
	case "ping":
		m.metrics.webhook("ping")
		fmt.Fprintln(w, "pong")
		return
	case "push":
	default:
		m.metrics.webhook("ignored")
		fmt.Fprintf(w, "ignored %s event\n", event)
		return
	}
	var ev pushEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		http.Error(w, "invalid push event", http.StatusBadRequest)
		return
	}
	branch, isBranch := strings.CutPrefix(ev.Ref, "refs/heads/")
	if !isBranch || ev.Deleted {
		m.metrics.webhook("ignored")
		fmt.Fprintln(w, "ignored: not a push to a branch")
		return
	}
	repo := m.find(ev.Repository.FullName, branch)
	if repo == nil && m.cfg.GitHubOrg != "" && branch == ev.Repository.DefaultBranch &&
		strings.EqualFold(strings.SplitN(ev.Repository.FullName, "/", 2)[0], m.cfg.GitHubOrg) &&
		!ev.Repository.Archived && (!ev.Repository.Fork || m.cfg.IncludeForks) {
		repo = m.addGitHub(ev.Repository) // a new repository of the organization
	}
	if repo == nil {
		m.metrics.webhook("ignored")
		fmt.Fprintf(w, "ignored: %s %s is not served\n", ev.Repository.FullName, branch)
		return
	}
	m.Sync(repo.spec.Name)
	m.metrics.webhook("synced")
	w.WriteHeader(http.StatusAccepted)
	fmt.Fprintf(w, "syncing %s\n", repo.spec.Name)
}

// validSignature checks GitHub's HMAC-SHA256 signature of a delivery.
func validSignature(secret, header string, body []byte) bool {
	sig, ok := strings.CutPrefix(header, "sha256=")
	if !ok {
		return false
	}
	got, err := hex.DecodeString(sig)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}
