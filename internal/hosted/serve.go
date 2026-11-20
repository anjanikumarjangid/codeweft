package hosted

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/anjanikumarjangid/codeweft/internal/server"
)

// Handler returns the server's HTTP routes:
//
//	/mcp              MCP over Streamable HTTP (bearer token)
//	/webhooks/github  GitHub push deliveries (signed with the webhook secret)
//	/status           the repositories and their indexes, as JSON (bearer token)
//	/metrics          Prometheus metrics (bearer token)
//	/healthz          200 while the process runs
//	/readyz           200 once a repository is indexed
func (m *Manager) Handler() http.Handler {
	mcpServer := server.NewDynamic(m.set, server.Options{})
	// Stateless: every tool only reads, so no session needs to be kept,
	// and replicas behind a load balancer need no affinity. A signed-in
	// user gets a server showing only the repositories they may read.
	mcpHandler := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		if u, ok := r.Context().Value(userKey{}).(*user); ok {
			return u.server
		}
		return mcpServer
	}, &mcp.StreamableHTTPOptions{Stateless: true})
	mux := http.NewServeMux()
	if m.cfg.SignIn() {
		si := newSignIn(m)
		si.routes(mux)
		mux.Handle("/mcp", m.userAuth(si, m.countRequests(mcpHandler)))
	} else {
		mux.Handle("/mcp", m.auth(m.countRequests(mcpHandler)))
	}
	mux.HandleFunc("/webhooks/github", m.webhook)
	mux.Handle("/status", m.auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(struct {
			Version string       `json:"version"`
			Repos   []RepoStatus `json:"repositories"`
		}{server.Version, m.Statuses()})
	})))
	mux.Handle("/metrics", m.auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		m.metrics.write(w, m.Statuses())
	})))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if len(m.set.Repos()) == 0 {
			http.Error(w, "no repository indexed yet", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ready\n"))
	})
	return mux
}

// auth requires one of the configured bearer tokens, unless the server
// was explicitly configured to run without.
func (m *Manager) auth(next http.Handler) http.Handler {
	if len(m.cfg.APITokens) == 0 && m.cfg.AllowUnauthenticated {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearer(r)
		if !ok || !m.validToken(token) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="codeweft"`)
			http.Error(w, "a valid bearer token is required", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// userKey is the request context key of a signed-in user.
type userKey struct{}

// userAuth accepts an API token, which reads every repository, or a
// GitHub user's token (one this server issued at sign-in, or a GitHub
// token sent as it is), which reads the repositories that user can.
func (m *Manager) userAuth(si *signIn, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearer(r)
		if ok && m.validToken(token) {
			next.ServeHTTP(w, r)
			return
		}
		if ok {
			if gh, ok := si.githubToken(token); ok {
				u, err := si.resolve(r.Context(), gh)
				if err == nil {
					next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userKey{}, u)))
					return
				}
				if !errors.Is(err, errBadToken) {
					m.log.Warn("signing in", "err", err)
					http.Error(w, "GitHub could not be reached to check the token", http.StatusBadGateway)
					return
				}
			}
		}
		w.Header().Set("WWW-Authenticate", si.challenge())
		http.Error(w, "sign in with GitHub, or send a valid bearer token", http.StatusUnauthorized)
	})
}

func bearer(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	if len(h) < 7 || !equalFoldASCII(h[:7], "bearer ") {
		return "", false
	}
	return h[7:], true
}

func equalFoldASCII(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		x, y := a[i], b[i]
		if 'A' <= x && x <= 'Z' {
			x += 'a' - 'A'
		}
		if 'A' <= y && y <= 'Z' {
			y += 'a' - 'A'
		}
		if x != y {
			return false
		}
	}
	return true
}

// validToken compares in constant time against every configured token.
func (m *Manager) validToken(token string) bool {
	ok := false
	for _, t := range m.cfg.APITokens {
		if subtle.ConstantTimeCompare([]byte(token), []byte(t)) == 1 {
			ok = true
		}
	}
	return ok
}

// statusRecorder captures the status code of a response.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (m *Manager) countRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		m.metrics.request(rec.status)
	})
}

// Serve runs a hosted server until ctx ends: it starts syncing the
// repositories and serves HTTP on cfg.Addr.
func Serve(ctx context.Context, cfg Config, log *slog.Logger) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	if cfg.AllowUnauthenticated && len(cfg.APITokens) == 0 {
		log.Warn("serving without authentication: anyone who can reach the server can read the indexed code")
	}
	switch {
	case cfg.WebhookSecret == "" && cfg.SyncInterval == 0:
		log.Warn("webhooks off (no GITHUB_WEBHOOK_SECRET) and periodic syncs off: repositories are synced at start only")
	case cfg.WebhookSecret == "":
		log.Info("webhooks off (no GITHUB_WEBHOOK_SECRET): repositories are synced periodically",
			"interval", cfg.SyncInterval.String())
	}
	m := NewManager(cfg, server.NewRepoSet(), log)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	m.Start(ctx)
	srv := &http.Server{Addr: cfg.Addr, Handler: m.Handler(), ReadHeaderTimeout: 10 * time.Second,
		BaseContext: func(net.Listener) context.Context { return ctx }}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Info("serving", "addr", cfg.Addr, "repositories", len(cfg.Repos), "org", cfg.GitHubOrg)
	var err error
	select {
	case <-ctx.Done():
	case err = <-errc:
	}
	shutdown, done := context.WithTimeout(context.Background(), 10*time.Second)
	defer done()
	_ = srv.Shutdown(shutdown)
	cancel()
	m.Wait()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
