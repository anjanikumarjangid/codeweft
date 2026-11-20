package hosted

// Per-user sign-in with GitHub.
//
// With a GitHub OAuth App or GitHub App configured (client ID and secret),
// people sign in with their own GitHub account and see only the
// repositories that account can read. MCP clients find out how through the
// MCP authorization flow: an unauthenticated request gets a 401 pointing at
// the protected resource metadata, which names this server as the
// authorization server. The client registers itself (dynamic client
// registration), sends the user's browser to /oauth/authorize, which passes
// it on to GitHub, and exchanges the code it gets back, with PKCE, for an
// access token.
//
// The server keeps no sessions: client IDs, OAuth states, authorization
// codes and tokens are sealed (AES-GCM) with a key derived from the client
// secret, and the access token carries the user's GitHub token. Replicas
// sharing the secret accept each other's tokens, and a restart signs no
// one out. What a user may read is asked of GitHub with their own token,
// and remembered for a few minutes.

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/anjanikumarjangid/codeweft/internal/server"
)

const (
	tokenPrefix   = "cwu_"
	accessTTL     = 8 * time.Hour
	refreshTTL    = 30 * 24 * time.Hour
	codeTTL       = 5 * time.Minute
	stateTTL      = 10 * time.Minute
	identityTTL   = 5 * time.Minute  // how long a token's GitHub user is remembered
	permissionTTL = 10 * time.Minute // how long a repository permission is remembered
)

// sealer encrypts and authenticates the server's tokens.
type sealer struct{ aead cipher.AEAD }

func newSealer(secret string) *sealer {
	key := sha256.Sum256([]byte("codeweft sign-in v1\x00" + secret))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		panic(err) // a 32-byte key is always valid
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		panic(err)
	}
	return &sealer{aead: aead}
}

// sealed is the plaintext of a sealed value: what it is, when it expires
// (0 for never), and the value.
type sealed struct {
	Kind    string          `json:"k"`
	Expires int64           `json:"e,omitempty"`
	Value   json.RawMessage `json:"v"`
}

func (s *sealer) seal(kind string, v any, ttl time.Duration) (string, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	p := sealed{Kind: kind, Value: raw}
	if ttl > 0 {
		p.Expires = time.Now().Add(ttl).Unix()
	}
	plain, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	out := s.aead.Seal(nonce, nonce, plain, []byte(kind))
	return tokenPrefix + base64.RawURLEncoding.EncodeToString(out), nil
}

var errBadToken = errors.New("invalid or expired token")

func (s *sealer) open(kind, token string, v any) error {
	if !strings.HasPrefix(token, tokenPrefix) {
		return errBadToken
	}
	raw, err := base64.RawURLEncoding.DecodeString(token[len(tokenPrefix):])
	if err != nil || len(raw) < s.aead.NonceSize() {
		return errBadToken
	}
	plain, err := s.aead.Open(nil, raw[:s.aead.NonceSize()], raw[s.aead.NonceSize():], []byte(kind))
	if err != nil {
		return errBadToken
	}
	var p sealed
	if json.Unmarshal(plain, &p) != nil || p.Kind != kind || (p.Expires != 0 && time.Now().Unix() > p.Expires) {
		return errBadToken
	}
	if json.Unmarshal(p.Value, v) != nil {
		return errBadToken
	}
	return nil
}

// The sealed values.
type (
	oauthClient struct {
		RedirectURIs []string `json:"r"`
		Name         string   `json:"n,omitempty"`
	}
	oauthState struct {
		ClientID    string `json:"c"`
		RedirectURI string `json:"r"`
		State       string `json:"s,omitempty"`
		Challenge   string `json:"p"`
	}
	ghGrant struct {
		Token        string `json:"t"`
		Refresh      string `json:"f,omitempty"`
		Expires      int64  `json:"x,omitempty"` // when Token expires, for expiring GitHub App tokens
		ClientID     string `json:"c,omitempty"`
		RedirectURI  string `json:"r,omitempty"`
		Challenge    string `json:"p,omitempty"`
		RefreshUntil int64  `json:"u,omitempty"`
	}
)

// signIn holds what sign-in needs at run time.
type signIn struct {
	m      *Manager
	seal   *sealer
	client *http.Client

	mu         sync.Mutex
	identities map[[32]byte]identity // by token hash
	users      map[string]*user      // by login
	usedCodes  map[string]time.Time  // authorization codes already exchanged
}

type identity struct {
	login string
	at    time.Time
}

// user is a signed-in GitHub user: the repositories they may read, and the
// MCP server showing only those.
type user struct {
	login  string
	server *mcp.Server

	mu      sync.Mutex
	allowed map[string]bool      // by GitHub full name
	checked map[string]time.Time // when each permission was asked of GitHub
}

func newSignIn(m *Manager) *signIn {
	return &signIn{m: m, seal: newSealer(m.cfg.GitHubClientSecret), client: &http.Client{Timeout: 20 * time.Second},
		identities: map[[32]byte]identity{}, users: map[string]*user{}, usedCodes: map[string]time.Time{}}
}

// githubToken returns the GitHub token a bearer token stands for: the one
// sealed in an access token this server issued, or a GitHub token sent as
// it is (a personal access token, or `gh auth token`).
func (si *signIn) githubToken(bearer string) (string, bool) {
	if strings.HasPrefix(bearer, tokenPrefix) {
		var g ghGrant
		if si.seal.open("access", bearer, &g) != nil {
			return "", false
		}
		return g.Token, true
	}
	for _, p := range []string{"ghp_", "gho_", "ghu_", "github_pat_"} {
		if strings.HasPrefix(bearer, p) {
			return bearer, true
		}
	}
	return "", false
}

// resolve returns the user a GitHub token belongs to, with their
// permissions on the served repositories brought up to date.
func (si *signIn) resolve(ctx context.Context, token string) (*user, error) {
	h := sha256.Sum256([]byte(token))
	si.mu.Lock()
	id, ok := si.identities[h]
	si.mu.Unlock()
	if !ok || time.Since(id.at) > identityTTL {
		login, err := si.login(ctx, token)
		if err != nil {
			return nil, err
		}
		id = identity{login: login, at: time.Now()}
		si.mu.Lock()
		if len(si.identities) > 10000 {
			si.identities = map[[32]byte]identity{} // forget old tokens rather than grow
		}
		si.identities[h] = id
		si.mu.Unlock()
	}
	si.mu.Lock()
	u, ok := si.users[id.login]
	if !ok {
		u = &user{login: id.login, allowed: map[string]bool{}, checked: map[string]time.Time{}}
		u.server = server.NewDynamic(si.m.set.Filter(si.m.allows(u)), server.Options{
			EmptyMessage: "no repository is available to " + id.login + " yet: either the server is still indexing, " +
				"or this GitHub account cannot read any of the repositories it serves",
		})
		si.users[id.login] = u
	}
	si.mu.Unlock()
	si.refresh(ctx, u, token)
	return u, nil
}

// refresh asks GitHub, with the user's own token, which of the served
// repositories they can read, for those not asked recently.
func (si *signIn) refresh(ctx context.Context, u *user, token string) {
	var due []string
	u.mu.Lock()
	for _, full := range si.m.fullNames() {
		if at, ok := u.checked[full]; !ok || time.Since(at) > permissionTTL {
			due = append(due, full)
		}
	}
	u.mu.Unlock()
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for _, full := range due {
		wg.Add(1)
		sem <- struct{}{}
		go func(full string) {
			defer func() { <-sem; wg.Done() }()
			ok, err := si.canRead(ctx, token, full)
			if err != nil {
				si.m.log.Warn("checking repository access", "user", u.login, "repo", full, "err", err)
				return // ask again on the next request
			}
			u.mu.Lock()
			u.allowed[full], u.checked[full] = ok, time.Now()
			u.mu.Unlock()
		}(full)
	}
	wg.Wait()
}

func (si *signIn) login(ctx context.Context, token string) (string, error) {
	var me struct {
		Login string `json:"login"`
	}
	code, err := si.api(ctx, token, "/user", &me)
	if err != nil {
		return "", err
	}
	if code != http.StatusOK || me.Login == "" {
		return "", errBadToken
	}
	return me.Login, nil
}

func (si *signIn) canRead(ctx context.Context, token, full string) (bool, error) {
	code, err := si.api(ctx, token, "/repos/"+full, nil)
	switch {
	case err != nil:
		return false, err
	case code == http.StatusOK:
		return true, nil
	case code == http.StatusNotFound || code == http.StatusForbidden || code == http.StatusUnauthorized:
		return false, nil
	default:
		return false, fmt.Errorf("GitHub answered %d", code)
	}
}

// api calls the GitHub REST API with a user's token.
func (si *signIn) api(ctx context.Context, token, path string, out any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, si.m.cfg.GitHubAPIURL+path, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := si.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if out != nil && resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out); err != nil {
			return 0, err
		}
	}
	return resp.StatusCode, nil
}

// allows reports whether a user may read a served repository, from what
// was last learned from GitHub. Repositories not on GitHub are only for
// API tokens.
func (m *Manager) allows(u *user) func(name string) bool {
	return func(name string) bool {
		m.mu.Lock()
		r, ok := m.byID[name]
		m.mu.Unlock()
		if !ok || r.spec.FullName == "" {
			return false
		}
		u.mu.Lock()
		defer u.mu.Unlock()
		return u.allowed[strings.ToLower(r.spec.FullName)]
	}
}

// fullNames lists the GitHub repositories served, lower-cased.
func (m *Manager) fullNames() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, r := range m.byID {
		if r.spec.FullName != "" {
			out = append(out, strings.ToLower(r.spec.FullName))
		}
	}
	return out
}

// --- OAuth endpoints ---

func (si *signIn) routes(mux *http.ServeMux) {
	mux.HandleFunc("/.well-known/oauth-protected-resource", si.resourceMetadata)
	mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", si.resourceMetadata)
	mux.HandleFunc("/.well-known/oauth-authorization-server", si.serverMetadata)
	mux.HandleFunc("/oauth/register", si.register)
	mux.HandleFunc("/oauth/authorize", si.authorize)
	mux.HandleFunc("/oauth/callback", si.callback)
	mux.HandleFunc("/oauth/token", si.token)
}

func (si *signIn) public() string { return si.m.cfg.PublicURL }

// challenge is the WWW-Authenticate header of a 401 on /mcp.
func (si *signIn) challenge() string {
	return `Bearer realm="codeweft", resource_metadata="` + si.public() + `/.well-known/oauth-protected-resource"`
}

func (si *signIn) resourceMetadata(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"resource":                 si.public() + "/mcp",
		"authorization_servers":    []string{si.public()},
		"bearer_methods_supported": []string{"header"},
		"resource_name":            "codeweft",
	})
}

func (si *signIn) serverMetadata(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                si.public(),
		"authorization_endpoint":                si.public() + "/oauth/authorize",
		"token_endpoint":                        si.public() + "/oauth/token",
		"registration_endpoint":                 si.public() + "/oauth/register",
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":      []string{"S256"},
		"token_endpoint_auth_methods_supported": []string{"none"},
	})
}

// register implements dynamic client registration (RFC 7591). The client
// ID is the sealed list of redirect URIs, so nothing is stored.
func (si *signIn) register(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST a client registration", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		RedirectURIs []string `json:"redirect_uris"`
		ClientName   string   `json:"client_name"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_client_metadata", "the body is not JSON")
		return
	}
	if len(req.RedirectURIs) == 0 || len(req.RedirectURIs) > 10 {
		oauthError(w, http.StatusBadRequest, "invalid_redirect_uri", "give one to ten redirect_uris")
		return
	}
	for _, u := range req.RedirectURIs {
		if !validRedirect(u) {
			oauthError(w, http.StatusBadRequest, "invalid_redirect_uri",
				"redirect URIs must be https, http on a loopback address, or an application's own scheme: "+u)
			return
		}
	}
	if len(req.ClientName) > 100 {
		req.ClientName = req.ClientName[:100]
	}
	id, err := si.seal.seal("client", oauthClient{RedirectURIs: req.RedirectURIs, Name: req.ClientName}, 0)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"client_id":                  id,
		"client_id_issued_at":        time.Now().Unix(),
		"client_name":                req.ClientName,
		"redirect_uris":              req.RedirectURIs,
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
	})
}

// validRedirect accepts https URLs, http on a loopback address, and a
// native application's private scheme (such as cursor://).
func validRedirect(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Fragment != "" || u.Scheme == "" {
		return false
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return u.Host != ""
	case "http":
		return loopback(u.Hostname())
	case "javascript", "data", "file", "vbscript", "blob", "about":
		return false
	}
	return true
}

func loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// redirectAllowed matches a redirect URI against a client's registered
// ones; on a loopback address any port is accepted (RFC 8252).
func redirectAllowed(client oauthClient, raw string) bool {
	for _, reg := range client.RedirectURIs {
		if reg == raw {
			return true
		}
		a, err1 := url.Parse(reg)
		b, err2 := url.Parse(raw)
		if err1 == nil && err2 == nil && a.Scheme == "http" && b.Scheme == "http" && loopback(a.Hostname()) &&
			a.Hostname() == b.Hostname() && a.Path == b.Path && a.RawQuery == b.RawQuery {
			return true
		}
	}
	return false
}

// authorize checks the client's request, asks the user to confirm which
// application is signing in, and then sends the browser to GitHub.
//
// The confirmation matters because anyone can register a client: without
// it, a link could have a user sign in on behalf of an application that
// is not theirs, with GitHub skipping its own prompt for an OAuth App the
// user has already authorized. The confirmation form only counts when it
// carries the value of a SameSite cookie set with it, so another site
// cannot submit it for the user.
func (si *signIn) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if r.Method == http.MethodPost {
		if r.ParseForm() != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		q = r.PostForm
	}
	var client oauthClient
	if si.seal.open("client", q.Get("client_id"), &client) != nil {
		http.Error(w, "unknown client_id: register the client first", http.StatusBadRequest)
		return
	}
	redirect := q.Get("redirect_uri")
	if redirect == "" && len(client.RedirectURIs) == 1 {
		redirect = client.RedirectURIs[0]
	}
	if !redirectAllowed(client, redirect) {
		http.Error(w, "redirect_uri is not one the client registered", http.StatusBadRequest)
		return
	}
	// From here on, errors go back to the client.
	fail := func(code, desc string) {
		redirectWith(w, r, redirect, url.Values{"error": {code}, "error_description": {desc}, "state": {q.Get("state")}})
	}
	if q.Get("response_type") != "code" {
		fail("unsupported_response_type", "only the code flow is supported")
		return
	}
	if q.Get("code_challenge") == "" || q.Get("code_challenge_method") != "S256" {
		fail("invalid_request", "PKCE with S256 is required")
		return
	}
	if r.Method == http.MethodPost && q.Get("deny") != "" {
		fail("access_denied", "the user declined")
		return
	}
	if r.Method != http.MethodPost || !si.confirmed(r, q.Get("csrf")) {
		si.consent(w, q, client, redirect)
		return
	}
	state, err := si.seal.seal("state", oauthState{ClientID: q.Get("client_id"), RedirectURI: redirect,
		State: q.Get("state"), Challenge: q.Get("code_challenge")}, stateTTL)
	if err != nil {
		fail("server_error", err.Error())
		return
	}
	gh := url.Values{"client_id": {si.m.cfg.GitHubClientID}, "redirect_uri": {si.public() + "/oauth/callback"},
		"state": {state}}
	if si.m.cfg.GitHubOAuthScope != "" {
		gh.Set("scope", si.m.cfg.GitHubOAuthScope)
	}
	http.Redirect(w, r, si.m.cfg.GitHubURL+"/login/oauth/authorize?"+gh.Encode(), http.StatusFound)
}

const consentCookie = "codeweft_consent"

// confirmed reports whether a confirmation form came with the cookie set
// alongside it.
func (si *signIn) confirmed(r *http.Request, value string) bool {
	c, err := r.Cookie(consentCookie)
	return err == nil && value != "" && subtle.ConstantTimeCompare([]byte(c.Value), []byte(value)) == 1
}

var consentPage = template.Must(template.New("consent").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Sign in to codeweft</title>
<style>
body{font-family:system-ui,sans-serif;max-width:32rem;margin:4rem auto;padding:0 1rem;color:#1f2328;background:#fff}
@media (prefers-color-scheme:dark){body{color:#e6edf3;background:#0d1117}}
code{font-size:.95em}button{font:inherit;padding:.5rem 1rem;margin-right:.5rem;cursor:pointer}
</style></head><body>
<h1>Sign in to codeweft</h1>
<p><strong>{{.Name}}</strong> wants to read the code this server indexes, as your GitHub account.
It will see only the repositories your account can read.</p>
<p>After you sign in, you are sent back to <code>{{.Target}}</code>. Continue only if you started this sign-in from that application.</p>
<form method="post" action="authorize">
{{range $k, $v := .Fields}}<input type="hidden" name="{{$k}}" value="{{$v}}">
{{end}}<button type="submit">Continue with GitHub</button><button type="submit" name="deny" value="1">Cancel</button>
</form></body></html>
`))

// consent shows the confirmation page, with a fresh cookie and form value.
func (si *signIn) consent(w http.ResponseWriter, q url.Values, client oauthClient, redirect string) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	csrf := base64.RawURLEncoding.EncodeToString(b)
	http.SetCookie(w, &http.Cookie{Name: consentCookie, Value: csrf, Path: "/oauth/authorize", MaxAge: int(stateTTL.Seconds()),
		HttpOnly: true, Secure: strings.HasPrefix(si.public(), "https://"), SameSite: http.SameSiteStrictMode})
	fields := map[string]string{"csrf": csrf, "redirect_uri": redirect}
	for _, k := range []string{"client_id", "response_type", "state", "code_challenge", "code_challenge_method", "scope", "resource"} {
		if v := q.Get(k); v != "" {
			fields[k] = v
		}
	}
	name := client.Name
	if name == "" {
		name = "An MCP client"
	}
	target := redirect
	if u, err := url.Parse(redirect); err == nil && u.Host != "" {
		target = u.Scheme + "://" + u.Host
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self' "+
		si.m.cfg.GitHubURL+"; frame-ancestors 'none'")
	w.Header().Set("X-Frame-Options", "DENY")
	_ = consentPage.Execute(w, map[string]any{"Name": name, "Target": target, "Fields": fields})
}

// callback receives GitHub's answer, exchanges its code for the user's
// GitHub token, and returns an authorization code to the client.
func (si *signIn) callback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var st oauthState
	if si.seal.open("state", q.Get("state"), &st) != nil {
		http.Error(w, "this sign-in has expired; start again from your MCP client", http.StatusBadRequest)
		return
	}
	back := func(v url.Values) {
		if st.State != "" {
			v.Set("state", st.State)
		}
		redirectWith(w, r, st.RedirectURI, v)
	}
	if e := q.Get("error"); e != "" {
		back(url.Values{"error": {"access_denied"}, "error_description": {"GitHub: " + e}})
		return
	}
	g, err := si.exchange(r.Context(), url.Values{"code": {q.Get("code")},
		"redirect_uri": {si.public() + "/oauth/callback"}})
	if err != nil {
		back(url.Values{"error": {"access_denied"}, "error_description": {err.Error()}})
		return
	}
	if _, err := si.login(r.Context(), g.Token); err != nil {
		back(url.Values{"error": {"access_denied"}, "error_description": {"GitHub did not accept the new token"}})
		return
	}
	g.ClientID, g.RedirectURI, g.Challenge = st.ClientID, st.RedirectURI, st.Challenge
	code, err := si.seal.seal("code", g, codeTTL)
	if err != nil {
		back(url.Values{"error": {"server_error"}})
		return
	}
	back(url.Values{"code": {code}, "iss": {si.public()}})
}

// exchange calls GitHub's token endpoint, for a code or a refresh token.
func (si *signIn) exchange(ctx context.Context, form url.Values) (ghGrant, error) {
	form.Set("client_id", si.m.cfg.GitHubClientID)
	form.Set("client_secret", si.m.cfg.GitHubClientSecret)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, si.m.cfg.GitHubURL+"/login/oauth/access_token",
		strings.NewReader(form.Encode()))
	if err != nil {
		return ghGrant{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := si.client.Do(req)
	if err != nil {
		return ghGrant{}, err
	}
	defer resp.Body.Close()
	var out struct {
		AccessToken           string `json:"access_token"`
		RefreshToken          string `json:"refresh_token"`
		ExpiresIn             int64  `json:"expires_in"`
		RefreshTokenExpiresIn int64  `json:"refresh_token_expires_in"`
		Error                 string `json:"error"`
		ErrorDescription      string `json:"error_description"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return ghGrant{}, fmt.Errorf("GitHub token endpoint: %w", err)
	}
	if out.Error != "" || out.AccessToken == "" {
		return ghGrant{}, fmt.Errorf("GitHub refused: %s %s", out.Error, out.ErrorDescription)
	}
	g := ghGrant{Token: out.AccessToken, Refresh: out.RefreshToken}
	now := time.Now()
	if out.ExpiresIn > 0 {
		g.Expires = now.Add(time.Duration(out.ExpiresIn) * time.Second).Unix()
	}
	if out.RefreshTokenExpiresIn > 0 {
		g.RefreshUntil = now.Add(time.Duration(out.RefreshTokenExpiresIn) * time.Second).Unix()
	}
	return g, nil
}

// token is the token endpoint: authorization codes (with PKCE) and
// refresh tokens.
func (si *signIn) token(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodPost || r.ParseForm() != nil {
		oauthError(w, http.StatusBadRequest, "invalid_request", "POST a form")
		return
	}
	var g ghGrant
	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		code := r.PostForm.Get("code")
		if si.seal.open("code", code, &g) != nil {
			oauthError(w, http.StatusBadRequest, "invalid_grant", "the code is invalid or expired")
			return
		}
		verifier := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
		if base64.RawURLEncoding.EncodeToString(verifier[:]) != g.Challenge {
			oauthError(w, http.StatusBadRequest, "invalid_grant", "the code_verifier does not match")
			return
		}
		if (r.PostForm.Get("client_id") != "" && r.PostForm.Get("client_id") != g.ClientID) ||
			(r.PostForm.Get("redirect_uri") != "" && r.PostForm.Get("redirect_uri") != g.RedirectURI) {
			oauthError(w, http.StatusBadRequest, "invalid_grant", "the code was issued to another client")
			return
		}
		if !si.useCode(code) {
			oauthError(w, http.StatusBadRequest, "invalid_grant", "the code was already used")
			return
		}
	case "refresh_token":
		if si.seal.open("refresh", r.PostForm.Get("refresh_token"), &g) != nil {
			oauthError(w, http.StatusBadRequest, "invalid_grant", "the refresh token is invalid or expired")
			return
		}
		if g.Refresh != "" {
			// An expiring GitHub App token: renew it at GitHub.
			fresh, err := si.exchange(r.Context(), url.Values{"grant_type": {"refresh_token"}, "refresh_token": {g.Refresh}})
			if err != nil {
				oauthError(w, http.StatusBadRequest, "invalid_grant", err.Error())
				return
			}
			g = fresh
		} else if _, err := si.login(r.Context(), g.Token); err != nil {
			oauthError(w, http.StatusBadRequest, "invalid_grant", "GitHub no longer accepts this sign-in")
			return
		}
	default:
		oauthError(w, http.StatusBadRequest, "unsupported_grant_type", "use authorization_code or refresh_token")
		return
	}
	si.issue(w, g)
}

// issue answers the token endpoint with a new access and refresh token.
func (si *signIn) issue(w http.ResponseWriter, g ghGrant) {
	ttl := accessTTL
	if g.Expires != 0 {
		if left := time.Until(time.Unix(g.Expires, 0)); left < ttl {
			ttl = left
		}
	}
	if ttl <= 0 {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "the GitHub token has expired")
		return
	}
	access, err := si.seal.seal("access", ghGrant{Token: g.Token}, ttl)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	rttl := refreshTTL
	if g.RefreshUntil != 0 {
		rttl = time.Until(time.Unix(g.RefreshUntil, 0))
	}
	refresh, err := si.seal.seal("refresh", ghGrant{Token: g.Token, Refresh: g.Refresh, RefreshUntil: g.RefreshUntil}, rttl)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token":  access,
		"token_type":    "Bearer",
		"expires_in":    int(ttl.Seconds()),
		"refresh_token": refresh,
	})
}

// useCode records an authorization code as exchanged, reporting false if
// it already was (on this replica: codes also expire within minutes).
func (si *signIn) useCode(code string) bool {
	si.mu.Lock()
	defer si.mu.Unlock()
	now := time.Now()
	for c, at := range si.usedCodes {
		if now.Sub(at) > codeTTL {
			delete(si.usedCodes, c)
		}
	}
	if _, ok := si.usedCodes[code]; ok {
		return false
	}
	si.usedCodes[code] = now
	return true
}

func redirectWith(w http.ResponseWriter, r *http.Request, target string, v url.Values) {
	u, err := url.Parse(target)
	if err != nil {
		http.Error(w, "bad redirect URI", http.StatusBadRequest)
		return
	}
	q := u.Query()
	for k, vs := range v {
		for _, x := range vs {
			if x != "" {
				q.Set(k, x)
			}
		}
	}
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func oauthError(w http.ResponseWriter, code int, kind, desc string) {
	writeJSON(w, code, map[string]string{"error": kind, "error_description": desc})
}
