// Package hosted runs codeweft as a shared service: it clones the
// repositories it is given, keeps their indexes current from GitHub push
// webhooks and periodic syncs, and serves every one of them to MCP
// clients over Streamable HTTP, behind bearer tokens.
package hosted

import (
	"github.com/anjanikumarjangid/codeweft/internal/store"

	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Config is the configuration of a hosted server. Every field can be set
// from the environment (see FromEnv).
type Config struct {
	Addr    string // listen address, e.g. ":8080"
	DataDir string // where repositories are cloned and indexes kept

	// DatabaseURL, when set, keeps the indexes in PostgreSQL (one schema per
	// repository) instead of SQLite files under DataDir, so that several
	// replicas can share them.
	DatabaseURL string

	Repos     []RepoSpec // repositories to serve
	GitHubOrg string     // also serve every repository of this organization or user

	GitHubURL     string // web URL of GitHub (or GitHub Enterprise), for owner/name sources
	GitHubAPIURL  string // REST API URL, for listing an organization's repositories
	GitHubToken   string // used to clone private repositories and list the organization's
	IncludeForks  bool   // with GitHubOrg: also serve forks
	WebhookSecret string // GitHub webhook secret; the webhook endpoint is off without it

	APITokens            []string // bearer tokens accepted on /mcp, /status and /metrics
	AllowUnauthenticated bool     // serve without tokens: only behind something that authenticates

	SyncInterval    time.Duration // how often every repository is fetched, besides webhooks; 0 disables
	SyncConcurrency int           // how many repositories may be indexed at once
}

// RepoSpec is one repository to serve.
type RepoSpec struct {
	Name   string // labels its results; defaults to the repository's name
	Source string // a Git URL, or a local directory indexed in place
	Branch string // "" for the remote's default branch
	// FullName is "owner/name" for a GitHub repository, used to match
	// webhook deliveries; "" otherwise.
	FullName string
}

// Local reports whether the source is a directory indexed in place rather
// than a Git repository to clone.
func (r RepoSpec) Local() bool {
	return filepath.IsAbs(r.Source) && !strings.HasSuffix(r.Source, ".git")
}

// FromEnv reads a configuration from environment variables:
//
//	CODEWEFT_ADDR                    listen address (default :8080)
//	CODEWEFT_DATA_DIR                clones and indexes (default ./codeweft-data)
//	CODEWEFT_DATABASE_URL            postgres://... to keep indexes in PostgreSQL rather than
//	                                 SQLite files; DATABASE_URL is read too
//	CODEWEFT_REPOS                   repositories, separated by commas, spaces or new lines:
//	                                 owner/name, a Git URL, or an absolute local path,
//	                                 each optionally name=... and ...#branch
//	CODEWEFT_GITHUB_ORG              also serve every repository of this organization or user
//	CODEWEFT_GITHUB_INCLUDE_FORKS    with CODEWEFT_GITHUB_ORG, also serve forks (default false)
//	CODEWEFT_GITHUB_URL              GitHub web URL (default https://github.com)
//	CODEWEFT_GITHUB_API_URL          GitHub API URL (default https://api.github.com)
//	GITHUB_TOKEN                     clones private repositories, lists the organization
//	GITHUB_WEBHOOK_SECRET            enables /webhooks/github
//	CODEWEFT_API_TOKENS              bearer tokens for clients, separated by commas
//	CODEWEFT_ALLOW_UNAUTHENTICATED   true to serve without tokens
//	CODEWEFT_SYNC_INTERVAL           e.g. 10m (default); 0 turns periodic syncs off
//	CODEWEFT_SYNC_CONCURRENCY        repositories indexed at once (default 2)
func FromEnv(getenv func(string) string) (Config, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	c := Config{
		Addr:          or(getenv("CODEWEFT_ADDR"), ":8080"),
		DataDir:       or(getenv("CODEWEFT_DATA_DIR"), "codeweft-data"),
		DatabaseURL:   strings.TrimSpace(or(getenv("CODEWEFT_DATABASE_URL"), getenv("DATABASE_URL"))),
		GitHubOrg:     strings.TrimSpace(getenv("CODEWEFT_GITHUB_ORG")),
		GitHubURL:     strings.TrimSuffix(or(getenv("CODEWEFT_GITHUB_URL"), "https://github.com"), "/"),
		GitHubAPIURL:  strings.TrimSuffix(or(getenv("CODEWEFT_GITHUB_API_URL"), "https://api.github.com"), "/"),
		GitHubToken:   or(getenv("CODEWEFT_GITHUB_TOKEN"), getenv("GITHUB_TOKEN")),
		WebhookSecret: getenv("GITHUB_WEBHOOK_SECRET"),
	}
	var err error
	if c.IncludeForks, err = boolEnv(getenv, "CODEWEFT_GITHUB_INCLUDE_FORKS"); err != nil {
		return c, err
	}
	if c.AllowUnauthenticated, err = boolEnv(getenv, "CODEWEFT_ALLOW_UNAUTHENTICATED"); err != nil {
		return c, err
	}
	for _, t := range strings.Split(getenv("CODEWEFT_API_TOKENS"), ",") {
		if t = strings.TrimSpace(t); t != "" {
			c.APITokens = append(c.APITokens, t)
		}
	}
	c.SyncInterval = 10 * time.Minute
	if v := strings.TrimSpace(getenv("CODEWEFT_SYNC_INTERVAL")); v != "" {
		if v == "0" {
			c.SyncInterval = 0
		} else if c.SyncInterval, err = time.ParseDuration(v); err != nil {
			return c, fmt.Errorf("CODEWEFT_SYNC_INTERVAL: %w", err)
		}
	}
	c.SyncConcurrency = 2
	if v := strings.TrimSpace(getenv("CODEWEFT_SYNC_CONCURRENCY")); v != "" {
		if c.SyncConcurrency, err = strconv.Atoi(v); err != nil || c.SyncConcurrency < 1 {
			return c, fmt.Errorf("CODEWEFT_SYNC_CONCURRENCY: want a positive number, got %q", v)
		}
	}
	for _, f := range strings.FieldsFunc(getenv("CODEWEFT_REPOS"), func(r rune) bool {
		return r == ',' || r == ' ' || r == '\n' || r == '\t'
	}) {
		spec, err := ParseRepo(f, c.GitHubURL)
		if err != nil {
			return c, fmt.Errorf("CODEWEFT_REPOS: %w", err)
		}
		c.Repos = append(c.Repos, spec)
	}
	return c, nil
}

// Validate checks that a configuration can run safely.
func (c Config) Validate() error {
	if len(c.Repos) == 0 && c.GitHubOrg == "" {
		return errors.New("no repositories: set CODEWEFT_REPOS (owner/name, Git URLs or local paths) or CODEWEFT_GITHUB_ORG")
	}
	if len(c.APITokens) == 0 && !c.AllowUnauthenticated {
		return errors.New("no API tokens: set CODEWEFT_API_TOKENS so that only your clients can read the code, " +
			"or CODEWEFT_ALLOW_UNAUTHENTICATED=true if something in front of the server authenticates them")
	}
	if c.GitHubOrg != "" && c.GitHubToken == "" {
		return errors.New("CODEWEFT_GITHUB_ORG needs GITHUB_TOKEN to list the organization's repositories")
	}
	if c.DatabaseURL != "" && !store.IsPostgres(c.DatabaseURL) {
		return errors.New("CODEWEFT_DATABASE_URL must be a postgres:// URL; leave it unset to keep indexes in SQLite files")
	}
	seen := map[string]bool{}
	for _, r := range c.Repos {
		if seen[r.Name] {
			return fmt.Errorf("two repositories are named %q: name one of them with name=...", r.Name)
		}
		seen[r.Name] = true
	}
	return nil
}

// ParseRepo reads one repository: owner/name (on GitHub), a Git URL or an
// absolute local path, optionally prefixed with "name=" and suffixed with
// "#branch".
func ParseRepo(s, githubURL string) (RepoSpec, error) {
	var r RepoSpec
	s = strings.TrimSpace(s)
	if name, rest, ok := strings.Cut(s, "="); ok && !strings.Contains(name, "/") && !strings.Contains(name, ":") {
		r.Name, s = name, rest
	}
	if src, branch, ok := strings.Cut(s, "#"); ok {
		s, r.Branch = src, branch
	}
	switch {
	case s == "":
		return r, errors.New("empty repository")
	case filepath.IsAbs(s):
		r.Source = filepath.Clean(s)
	case strings.Contains(s, "://") || strings.HasPrefix(s, "git@"):
		r.Source = s
		r.FullName = githubFullName(s, githubURL)
	case strings.Count(s, "/") == 1:
		r.Source = githubURL + "/" + s + ".git"
		r.FullName = s
	default:
		return r, fmt.Errorf("%q: want owner/name, a Git URL or an absolute path", s)
	}
	if r.Name == "" {
		r.Name = strings.TrimSuffix(path.Base(filepath.ToSlash(r.Source)), ".git")
	}
	return r, nil
}

// githubFullName returns "owner/name" for a URL of a repository on GitHub
// (https://github.com/owner/name.git, git@github.com:owner/name.git).
func githubFullName(src, githubURL string) string {
	host := githubURL
	if u, err := url.Parse(githubURL); err == nil {
		host = u.Host
	}
	var p string
	switch {
	case strings.HasPrefix(src, "git@"+host+":"):
		p = strings.TrimPrefix(src, "git@"+host+":")
	default:
		u, err := url.Parse(src)
		if err != nil || u.Host != host {
			return ""
		}
		p = strings.TrimPrefix(u.Path, "/")
	}
	p = strings.TrimSuffix(p, ".git")
	if strings.Count(p, "/") != 1 {
		return ""
	}
	return p
}

func or(v, def string) string {
	if v = strings.TrimSpace(v); v != "" {
		return v
	}
	return def
}

func boolEnv(getenv func(string) string, key string) (bool, error) {
	v := strings.TrimSpace(getenv(key))
	if v == "" {
		return false, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("%s: want true or false, got %q", key, v)
	}
	return b, nil
}
