package server

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/dcadolph/yardmaster/internal/auth"
)

// enforcementCacheTTL bounds how often the middleware re-checks whether any tokens exist.
const enforcementCacheTTL = 5 * time.Second

// authGate authenticates API requests with bearer tokens. While no token exists the API stays
// open, so a fresh install works immediately; creating the first token turns enforcement on.
type authGate struct {
	// tokens is the token store.
	tokens auth.Store
	// log records authentication activity, never token material.
	log *zap.Logger
	// mu guards enforced and checkedAt.
	mu sync.Mutex
	// enforced caches whether any token exists.
	enforced bool
	// checkedAt is when enforced was last refreshed.
	checkedAt time.Time
}

// wrap guards next with token authentication.
func (g *authGate) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !g.protects(r) || !g.enforcing(r.Context()) {
			next.ServeHTTP(w, r)
			return
		}

		plain := auth.FromHeader(r.Header.Get("Authorization"))
		if plain == "" && isStream(r) {
			// EventSource cannot set headers, so the stream endpoint alone accepts the token as
			// a query parameter.
			plain = r.URL.Query().Get("access_token")
		}
		if plain == "" {
			unauthorized(w)
			return
		}
		tok, err := g.tokens.FindByHash(r.Context(), auth.HashToken(plain))
		if err != nil {
			unauthorized(w)
			return
		}
		g.touch(tok)
		next.ServeHTTP(w, r)
	})
}

// protects reports whether the request needs authentication. Liveness and the UI shell stay
// public; every page's data calls are still guarded.
func (g *authGate) protects(r *http.Request) bool {
	if r.Method == http.MethodGet && r.URL.Path == "/healthz" {
		return false
	}
	if r.Method == http.MethodGet &&
		(r.URL.Path == "/" || strings.HasPrefix(r.URL.Path, "/ui/")) {
		return false
	}
	return true
}

// enforcing reports whether any token exists, cached briefly to keep request overhead flat.
func (g *authGate) enforcing(ctx context.Context) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if time.Since(g.checkedAt) < enforcementCacheTTL {
		return g.enforced
	}
	n, err := g.tokens.Count(ctx)
	if err != nil {
		g.log.Error("server: count tokens: " + err.Error())
		// Fail closed: an unreadable token store should not open the API.
		g.enforced = true
	} else {
		g.enforced = n > 0
	}
	g.checkedAt = time.Now()
	return g.enforced
}

// touch records the token's last use, at most once a minute, without blocking the request.
func (g *authGate) touch(tok *auth.Token) {
	if tok.LastUsedAt != nil && time.Since(*tok.LastUsedAt) < time.Minute {
		return
	}
	now := time.Now()
	tok.LastUsedAt = &now
	go func() {
		if err := g.tokens.Save(context.Background(), tok); err != nil {
			g.log.Error("server: touch token: " + err.Error())
		}
	}()
}

// isStream reports whether the request targets the live stream endpoint.
func isStream(r *http.Request) bool {
	return r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/stream") &&
		strings.HasPrefix(r.URL.Path, "/runs/")
}

// unauthorized writes a 401 with a JSON error body.
func unauthorized(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
}

// authCheckHandler confirms a token works. The gate has already authenticated the request by the
// time it runs, so it only needs to answer.
func authCheckHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}
}
