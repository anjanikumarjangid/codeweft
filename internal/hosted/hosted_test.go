package hosted

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/anjanikumarjangid/codeweft/internal/server"
)

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// upstream creates a Git repository with one Go file, standing for a
// repository on GitHub.
func upstream(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	dir := t.TempDir()
	gitRun(t, dir, "init", "-q", "-b", "main")
	write(t, filepath.Join(dir, "go.mod"), "module example.com/app\n\ngo 1.24\n")
	write(t, filepath.Join(dir, "app.go"), "package app\n\n// Hello greets.\nfunc Hello() string { return \"hi\" }\n")
	gitRun(t, dir, "add", ".")
	gitRun(t, dir, "commit", "-q", "-m", "first")
	return dir
}

func write(t *testing.T, path, src string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
}

// bearerTransport adds a bearer token to every request.
type bearerTransport struct{ token string }

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

func sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// eventually retries f until it succeeds or 20 seconds pass.
func eventually(t *testing.T, what string, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !f() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestServer runs a hosted server end to end: it clones a repository,
// indexes it, answers MCP over HTTP only with a token, and re-indexes when
// a signed push webhook arrives.
func TestServer(t *testing.T) {
	src := upstream(t)
	spec, err := ParseRepo("file://"+src, "https://github.com")
	if err != nil {
		t.Fatal(err)
	}
	spec.Name, spec.FullName = "app", "acme/app"
	cfg := Config{DataDir: t.TempDir(), Repos: []RepoSpec{spec}, APITokens: []string{"s3cret"},
		WebhookSecret: "whsec", SyncConcurrency: 1, GitHubURL: "https://github.com"}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	set := server.NewRepoSet()
	m := NewManager(cfg, set, slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.Start(ctx)
	defer func() { cancel(); m.Wait() }()
	ts := httptest.NewServer(m.Handler())
	defer ts.Close()

	if resp, err := http.Get(ts.URL + "/readyz"); err == nil {
		resp.Body.Close() // may not be ready yet: only checks it answers
	}
	eventually(t, "the first index", func() bool { return len(set.Repos()) == 1 })

	// Without a token, or with a wrong one, nothing is served.
	for _, token := range []string{"", "wrong"} {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/mcp", strings.NewReader(`{}`))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("token %q: status %d, want 401", token, resp.StatusCode)
		}
	}
	for _, path := range []string{"/status", "/metrics"} {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s without a token: status %d, want 401", path, resp.StatusCode)
		}
	}

	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "v0"}, nil)
	cs, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: ts.URL + "/mcp",
		HTTPClient: &http.Client{Transport: bearerTransport{"s3cret"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	search := func(q string) string {
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "search_code", Arguments: map[string]any{"query": q}})
		if err != nil {
			t.Fatal(err)
		}
		return res.Content[0].(*mcp.TextContent).Text
	}
	if got := search("Hello"); !strings.Contains(got, "[app] app.Hello (func)") {
		t.Fatalf("search over HTTP:\n%s", got)
	}

	// A push to main, signed: the new function is served.
	write(t, filepath.Join(src, "bye.go"), "package app\n\n// Goodbye parts.\nfunc Goodbye() string { return Hello() }\n")
	gitRun(t, src, "add", ".")
	gitRun(t, src, "commit", "-q", "-m", "second")
	head := gitRun(t, src, "rev-parse", "HEAD")
	deliver := func(event, secret string, payload any) (int, string) {
		body, _ := json.Marshal(payload)
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/webhooks/github", bytes.NewReader(body))
		req.Header.Set("X-GitHub-Event", event)
		req.Header.Set("X-Hub-Signature-256", sign(secret, body))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	push := map[string]any{"ref": "refs/heads/main", "after": head,
		"repository": map[string]any{"name": "app", "full_name": "acme/app", "default_branch": "main"}}
	if code, _ := deliver("push", "not-the-secret", push); code != http.StatusUnauthorized {
		t.Errorf("forged delivery: status %d, want 401", code)
	}
	if code, body := deliver("ping", "whsec", map[string]any{}); code != http.StatusOK || !strings.Contains(body, "pong") {
		t.Errorf("ping: %d %s", code, body)
	}
	other := map[string]any{"ref": "refs/heads/feature", "repository": map[string]any{"full_name": "acme/app"}}
	if code, body := deliver("push", "whsec", other); code != http.StatusOK || !strings.Contains(body, "ignored") {
		t.Errorf("push to another branch: %d %s", code, body)
	}
	if code, body := deliver("push", "whsec", push); code != http.StatusAccepted {
		t.Fatalf("push: %d %s", code, body)
	}
	eventually(t, "the pushed commit to be indexed", func() bool {
		return strings.Contains(search("Goodbye"), "[app] app.Goodbye (func)")
	})

	// /status and /metrics, with the token.
	get := func(path string) string {
		req, _ := http.NewRequest(http.MethodGet, ts.URL+path, nil)
		req.Header.Set("Authorization", "Bearer s3cret")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}
	var status struct {
		Repos []RepoStatus `json:"repositories"`
	}
	if err := json.Unmarshal([]byte(get("/status")), &status); err != nil {
		t.Fatal(err)
	}
	if len(status.Repos) != 1 || status.Repos[0].Commit != head || status.Repos[0].Branch != "main" ||
		status.Repos[0].Symbols != 2 || status.Repos[0].LastError != "" {
		t.Errorf("status: %+v", status.Repos)
	}
	metrics := get("/metrics")
	for _, want := range []string{`codeweft_repositories{state="indexed"} 1`, `codeweft_repository_symbols{repo="app"} 2`,
		`codeweft_webhook_deliveries_total{result="synced"} 1`, `codeweft_webhook_deliveries_total{result="bad_signature"} 1`} {
		if !strings.Contains(metrics, want) {
			t.Errorf("metrics: missing %q in:\n%s", want, metrics)
		}
	}
}

func TestConfig(t *testing.T) {
	env := map[string]string{
		"CODEWEFT_REPOS":            "acme/orders, billing=git@github.com:acme/payments.git#release\n/srv/legacy",
		"CODEWEFT_API_TOKENS":       "a, b",
		"CODEWEFT_SYNC_INTERVAL":    "90s",
		"GITHUB_TOKEN":              "ghp_x",
		"CODEWEFT_SYNC_CONCURRENCY": "3",
	}
	cfg, err := FromEnv(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	want := []RepoSpec{
		{Name: "orders", Source: "https://github.com/acme/orders.git", FullName: "acme/orders"},
		{Name: "billing", Source: "git@github.com:acme/payments.git", Branch: "release", FullName: "acme/payments"},
		{Name: "legacy", Source: "/srv/legacy"},
	}
	if len(cfg.Repos) != len(want) {
		t.Fatalf("repos: %+v", cfg.Repos)
	}
	for i := range want {
		if cfg.Repos[i] != want[i] {
			t.Errorf("repo %d: %+v, want %+v", i, cfg.Repos[i], want[i])
		}
	}
	if !cfg.Repos[2].Local() || cfg.Repos[0].Local() {
		t.Error("only the absolute path is local")
	}
	if len(cfg.APITokens) != 2 || cfg.SyncInterval != 90*time.Second || cfg.SyncConcurrency != 3 ||
		cfg.Addr != ":8080" || cfg.GitHubToken != "ghp_x" {
		t.Errorf("config: %+v", cfg)
	}
	if err := cfg.Validate(); err != nil {
		t.Error(err)
	}

	for name, env := range map[string]map[string]string{
		"no repositories":          {"CODEWEFT_API_TOKENS": "a"},
		"no tokens":                {"CODEWEFT_REPOS": "acme/orders"},
		"org without a token":      {"CODEWEFT_GITHUB_ORG": "acme", "CODEWEFT_API_TOKENS": "a"},
		"two repositories, a name": {"CODEWEFT_REPOS": "acme/api other/api", "CODEWEFT_API_TOKENS": "a"},
	} {
		cfg, err := FromEnv(func(k string) string { return env[k] })
		if err == nil {
			err = cfg.Validate()
		}
		if err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	unauth := map[string]string{"CODEWEFT_REPOS": "acme/orders", "CODEWEFT_ALLOW_UNAUTHENTICATED": "true"}
	if cfg, err := FromEnv(func(k string) string { return unauth[k] }); err != nil || cfg.Validate() != nil {
		t.Errorf("explicitly unauthenticated: %v", err)
	}
	if _, err := FromEnv(func(k string) string { return map[string]string{"CODEWEFT_REPOS": "just-a-name"}[k] }); err == nil {
		t.Error("a bare name is not a repository")
	}
}

func TestRedact(t *testing.T) {
	if got := redact("fatal: could not read from https://x-access-token:ghp_abc@github.com/acme/app.git"); strings.Contains(got, "ghp_abc") {
		t.Errorf("token not redacted: %s", got)
	}
}
