package hosted

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/anjanikumarjangid/codeweft/internal/server"
)

// fakeGitHub answers the OAuth and REST endpoints sign-in uses. Codes are
// "code-<login>" and tokens "gho_<login>"; reads lists which repositories
// each user can read.
func fakeGitHub(t *testing.T, reads map[string][]string) *httptest.Server {
	t.Helper()
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		login := strings.TrimPrefix(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), "gho_")
		switch {
		case r.URL.Path == "/login/oauth/access_token":
			r.ParseForm() //nolint:errcheck
			if r.PostForm.Get("client_secret") != "shh" {
				writeJSON(w, http.StatusOK, map[string]string{"error": "incorrect_client_credentials"})
				return
			}
			who := strings.TrimPrefix(r.PostForm.Get("code"), "code-")
			writeJSON(w, http.StatusOK, map[string]string{"access_token": "gho_" + who, "token_type": "bearer"})
		case r.URL.Path == "/user":
			if _, ok := reads[login]; !ok {
				http.Error(w, "bad credentials", http.StatusUnauthorized)
				return
			}
			writeJSON(w, http.StatusOK, map[string]string{"login": login})
		case strings.HasPrefix(r.URL.Path, "/repos/"):
			for _, full := range reads[login] {
				if r.URL.Path == "/repos/"+full {
					writeJSON(w, http.StatusOK, map[string]string{"full_name": full})
					return
				}
			}
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(gh.Close)
	return gh
}

// TestSignIn walks an MCP client through sign-in with GitHub and checks
// that each user sees only the repositories their account can read.
func TestSignIn(t *testing.T) {
	gh := fakeGitHub(t, map[string][]string{"alice": {"acme/app"}, "bob": {}})
	var specs []RepoSpec
	for _, name := range []string{"app", "secret"} {
		src := upstream(t)
		write(t, filepath.Join(src, "app.go"), "package app\n\n// Hello greets.\nfunc Hello() string { return \"hi\" }\n")
		spec, err := ParseRepo("file://"+src, "https://github.com")
		if err != nil {
			t.Fatal(err)
		}
		spec.Name, spec.FullName = name, "acme/"+name
		specs = append(specs, spec)
	}
	cfg := Config{DataDir: t.TempDir(), Repos: specs, APITokens: []string{"ops"}, SyncConcurrency: 2,
		GitHubURL: gh.URL, GitHubAPIURL: gh.URL, GitHubClientID: "Iv1.test", GitHubClientSecret: "shh"}
	ctx, cancel := context.WithCancel(context.Background())
	set := server.NewRepoSet()
	m := NewManager(cfg, set, slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.Start(ctx)
	defer func() { cancel(); m.Wait() }()
	ts := httptest.NewServer(nil)
	defer ts.Close()
	m.cfg.PublicURL = ts.URL
	ts.Config.Handler = m.Handler()
	if err := m.cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	eventually(t, "both indexes", func() bool { return len(set.Repos()) == 2 })
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	// An unauthenticated request says where to sign in.
	resp, err := http.Post(ts.URL+"/mcp", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized ||
		!strings.Contains(resp.Header.Get("WWW-Authenticate"), `resource_metadata="`+ts.URL+`/.well-known/oauth-protected-resource"`) {
		t.Fatalf("unauthenticated: %d %q", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
	}
	getJSON := func(path string, out any) {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatal(err)
		}
	}
	var prm struct {
		Servers []string `json:"authorization_servers"`
	}
	getJSON("/.well-known/oauth-protected-resource", &prm)
	var asm struct {
		Authorize string `json:"authorization_endpoint"`
		Token     string `json:"token_endpoint"`
		Register  string `json:"registration_endpoint"`
	}
	getJSON("/.well-known/oauth-authorization-server", &asm)
	if len(prm.Servers) != 1 || prm.Servers[0] != ts.URL || asm.Token != ts.URL+"/oauth/token" {
		t.Fatalf("metadata: %+v %+v", prm, asm)
	}

	// Dynamic client registration; a non-loopback http redirect is refused.
	register := func(redirect string) (int, string) {
		body, _ := json.Marshal(map[string]any{"client_name": "test", "redirect_uris": []string{redirect}})
		resp, err := http.Post(asm.Register, "application/json", strings.NewReader(string(body)))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out struct {
			ClientID string `json:"client_id"`
		}
		json.NewDecoder(resp.Body).Decode(&out) //nolint:errcheck
		return resp.StatusCode, out.ClientID
	}
	if code, _ := register("http://evil.example.com/cb"); code != http.StatusBadRequest {
		t.Errorf("plain http redirect registered: %d", code)
	}
	code, clientID := register("http://127.0.0.1:33418/callback")
	if code != http.StatusCreated || clientID == "" {
		t.Fatalf("register: %d", code)
	}

	// The browser's journey: authorize -> GitHub -> callback -> client.
	signIn := func(login string) (access, refresh string) {
		verifier := "verifier-" + login + "-0123456789abcdefghijklmnopqrstuvwxyz"
		sum := sha256.Sum256([]byte(verifier))
		q := url.Values{"response_type": {"code"}, "client_id": {clientID},
			"redirect_uri": {"http://127.0.0.1:40000/callback"}, "state": {"xyz"},
			"code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "code_challenge_method": {"S256"}}
		// First the confirmation page, with its cookie.
		resp, err := noRedirect.Get(asm.Authorize + "?" + q.Encode())
		if err != nil {
			t.Fatal(err)
		}
		page, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		var cookie *http.Cookie
		for _, c := range resp.Cookies() {
			if c.Name == consentCookie {
				cookie = c
			}
		}
		if resp.StatusCode != http.StatusOK || cookie == nil || !strings.Contains(string(page), "<strong>test</strong> wants to read") ||
			!strings.Contains(string(page), "http://127.0.0.1:40000") || resp.Header.Get("X-Frame-Options") != "DENY" {
			t.Fatalf("consent page: %d %v\n%s", resp.StatusCode, cookie, page)
		}
		confirm := func(withCookie bool) *http.Response {
			form := url.Values{}
			for k, v := range q {
				form[k] = v
			}
			form.Set("csrf", cookie.Value)
			req, _ := http.NewRequest(http.MethodPost, asm.Authorize, strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			if withCookie {
				req.AddCookie(cookie)
			}
			resp, err := noRedirect.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			return resp
		}
		// Another site posting the form cannot send the cookie: the page is shown again.
		if resp := confirm(false); resp.StatusCode != http.StatusOK || resp.Header.Get("Location") != "" {
			t.Errorf("confirmation without the cookie: %d %s", resp.StatusCode, resp.Header.Get("Location"))
		}
		resp = confirm(true)
		toGitHub, _ := url.Parse(resp.Header.Get("Location"))
		if resp.StatusCode != http.StatusFound || !strings.HasPrefix(toGitHub.String(), gh.URL+"/login/oauth/authorize") ||
			toGitHub.Query().Get("redirect_uri") != ts.URL+"/oauth/callback" {
			t.Fatalf("authorize: %d %s", resp.StatusCode, toGitHub)
		}
		// GitHub sends the browser back with its code.
		cb := url.Values{"code": {"code-" + login}, "state": {toGitHub.Query().Get("state")}}
		resp, err = noRedirect.Get(ts.URL + "/oauth/callback?" + cb.Encode())
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		back, _ := url.Parse(resp.Header.Get("Location"))
		if !strings.HasPrefix(back.String(), "http://127.0.0.1:40000/callback?") || back.Query().Get("state") != "xyz" ||
			back.Query().Get("code") == "" {
			t.Fatalf("callback: %d %s", resp.StatusCode, back)
		}
		exchange := func(verifier string) (int, map[string]any) {
			resp, err := http.PostForm(asm.Token, url.Values{"grant_type": {"authorization_code"},
				"code": {back.Query().Get("code")}, "code_verifier": {verifier}, "client_id": {clientID},
				"redirect_uri": {"http://127.0.0.1:40000/callback"}})
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			var out map[string]any
			json.NewDecoder(resp.Body).Decode(&out) //nolint:errcheck
			return resp.StatusCode, out
		}
		if code, _ := exchange("not-the-verifier"); code != http.StatusBadRequest {
			t.Errorf("wrong PKCE verifier accepted: %d", code)
		}
		code, tok := exchange(verifier)
		if code != http.StatusOK {
			t.Fatalf("token: %d %v", code, tok)
		}
		if code, _ := exchange(verifier); code != http.StatusBadRequest {
			t.Errorf("code used twice: %d", code)
		}
		return tok["access_token"].(string), tok["refresh_token"].(string)
	}

	connect := func(token string) *mcp.ClientSession {
		client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "v0"}, nil)
		cs, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: ts.URL + "/mcp",
			HTTPClient: &http.Client{Transport: bearerTransport{token}}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { cs.Close() })
		return cs
	}
	search := func(cs *mcp.ClientSession) string {
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "search_code", Arguments: map[string]any{"query": "Hello"}})
		if err != nil {
			t.Fatal(err)
		}
		return res.Content[0].(*mcp.TextContent).Text
	}

	access, refresh := signIn("alice")
	got := search(connect(access))
	if !strings.Contains(got, "[app] app.Hello") || strings.Contains(got, "[secret]") {
		t.Errorf("alice sees:\n%s", got)
	}
	// Bob can read neither repository.
	bobAccess, _ := signIn("bob")
	if got := search(connect(bobAccess)); !strings.Contains(got, "no repository is available to bob") {
		t.Errorf("bob sees:\n%s", got)
	}
	// An API token still reads everything.
	if got := search(connect("ops")); !strings.Contains(got, "[app]") || !strings.Contains(got, "[secret]") {
		t.Errorf("API token sees:\n%s", got)
	}
	// A GitHub token sent as it is works like a sign-in.
	if got := search(connect("gho_alice")); !strings.Contains(got, "[app] app.Hello") || strings.Contains(got, "[secret]") {
		t.Errorf("alice's GitHub token sees:\n%s", got)
	}
	// Refreshing gives a working access token.
	resp, err = http.PostForm(asm.Token, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}})
	if err != nil {
		t.Fatal(err)
	}
	var fresh map[string]any
	json.NewDecoder(resp.Body).Decode(&fresh) //nolint:errcheck
	resp.Body.Close()
	if a, _ := fresh["access_token"].(string); a == "" || !strings.Contains(search(connect(a)), "[app] app.Hello") {
		t.Errorf("refresh: %v", fresh)
	}
	// Tampered or misused tokens are refused: a refresh token is no access
	// token, and an unknown GitHub user is no user.
	for _, bad := range []string{access[:len(access)-2] + "xx", refresh, "gho_mallory"} {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/mcp", strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer "+bad)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("token %.12s...: status %d, want 401", bad, resp.StatusCode)
		}
	}
	// /status stays for API tokens only.
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/status", nil)
	req.Header.Set("Authorization", "Bearer "+access)
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("/status with a user token: %v %v", resp.StatusCode, err)
	}
}

func TestSignInConfig(t *testing.T) {
	base := map[string]string{"CODEWEFT_REPOS": "acme/orders"}
	with := func(extra map[string]string) error {
		env := map[string]string{}
		for k, v := range base {
			env[k] = v
		}
		for k, v := range extra {
			env[k] = v
		}
		cfg, err := FromEnv(func(k string) string { return env[k] })
		if err != nil {
			return err
		}
		return cfg.Validate()
	}
	ok := map[string]string{"CODEWEFT_GITHUB_CLIENT_ID": "id", "CODEWEFT_GITHUB_CLIENT_SECRET": "s",
		"CODEWEFT_PUBLIC_URL": "https://codeweft.example.com/"}
	if err := with(ok); err != nil {
		t.Errorf("sign-in without API tokens: %v", err)
	}
	for name, extra := range map[string]map[string]string{
		"id without secret": {"CODEWEFT_GITHUB_CLIENT_ID": "id", "CODEWEFT_PUBLIC_URL": "https://x"},
		"no public URL":     {"CODEWEFT_GITHUB_CLIENT_ID": "id", "CODEWEFT_GITHUB_CLIENT_SECRET": "s"},
		"unauthenticated too": {"CODEWEFT_GITHUB_CLIENT_ID": "id", "CODEWEFT_GITHUB_CLIENT_SECRET": "s",
			"CODEWEFT_PUBLIC_URL": "https://x", "CODEWEFT_ALLOW_UNAUTHENTICATED": "true"},
	} {
		if with(extra) == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestRedirectRules(t *testing.T) {
	for raw, want := range map[string]bool{
		"https://app.example.com/cb":   true,
		"http://127.0.0.1:5000/cb":     true,
		"http://localhost/cb":          true,
		"http://[::1]:8000/cb":         true,
		"cursor://anysphere/oauth":     true,
		"http://example.com/cb":        false,
		"javascript:alert(1)":          false,
		"https://app.example.com/cb#x": false,
		"relative/path":                false,
	} {
		if got := validRedirect(raw); got != want {
			t.Errorf("validRedirect(%q) = %v, want %v", raw, got, want)
		}
	}
	c := oauthClient{RedirectURIs: []string{"http://127.0.0.1:1234/cb", "https://app.example.com/cb"}}
	for raw, want := range map[string]bool{
		"http://127.0.0.1:9999/cb":    true, // any loopback port
		"http://127.0.0.1:9999/other": false,
		"https://app.example.com/cb":  true,
		"https://app.example.com/cb2": false,
	} {
		if got := redirectAllowed(c, raw); got != want {
			t.Errorf("redirectAllowed(%q) = %v, want %v", raw, got, want)
		}
	}
}
