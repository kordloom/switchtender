package server

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/dcadolph/yardmaster/internal/audit"
	"github.com/dcadolph/yardmaster/internal/auth"
	"github.com/dcadolph/yardmaster/internal/user"
)

// enforcementCacheTTL bounds how often the middleware re-checks whether any tokens exist.
const enforcementCacheTTL = 5 * time.Second

// authGate authenticates API requests with bearer tokens and enforces the owning user's role.
// While no token exists the API stays open, so a fresh install works immediately; creating the
// first token turns enforcement on.
type authGate struct {
	// tokens is the token store.
	tokens auth.Store
	// users resolves token owners to roles, nil when accounts are not configured.
	users user.Store
	// audits records authenticated mutations, nil when the trail is off.
	audits audit.Store
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
		if tok.Expired(time.Now()) {
			// A dead token is useless forever; clear it out as it is caught.
			go func() { _ = g.tokens.Delete(context.Background(), tok.ID) }()
			unauthorized(w)
			return
		}
		role, err := g.roleFor(r.Context(), tok)
		if err != nil {
			unauthorized(w)
			return
		}
		if !roleAllows(role, requiredRole(r)) {
			forbidden(w)
			return
		}
		g.touch(tok)
		g.record(tok, r)
		next.ServeHTTP(w, r)
	})
}

// record appends an audit entry for an authenticated mutation without blocking the request.
func (g *authGate) record(tok *auth.Token, r *http.Request) {
	if g.audits == nil || r.Method == http.MethodGet || r.Method == http.MethodHead {
		return
	}
	entry := &audit.Entry{
		ID: audit.NewID(), At: time.Now(), Actor: tok.Name,
		Method: r.Method, Path: r.URL.Path,
	}
	go func() {
		if err := g.audits.Append(context.Background(), entry); err != nil {
			g.log.Error("server: append audit entry: " + err.Error())
		}
	}()
}

// roleFor resolves a token to its user's role. Tokens without an owner come from the command
// line and carry admin rights; tokens whose owner is gone stop working.
func (g *authGate) roleFor(ctx context.Context, tok *auth.Token) (user.Role, error) {
	if tok.UserID == "" {
		return user.RoleAdmin, nil
	}
	if g.users == nil {
		return user.RoleAdmin, nil
	}
	u, err := g.users.Get(ctx, tok.UserID)
	if err != nil {
		return "", err
	}
	return u.Role, nil
}

// requiredRole maps a request to the minimum role that may perform it. Reads are for viewers,
// launching and stopping work is for operators, and managing configuration is for admins.
func requiredRole(r *http.Request) user.Role {
	// The audit trail is management data even to read.
	if r.URL.Path == "/audit" {
		return user.RoleAdmin
	}
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return user.RoleViewer
	}
	p := r.URL.Path
	switch {
	case p == "/auth/check":
		return user.RoleViewer
	case p == "/runs", p == "/pipelines":
		return user.RoleOperator
	case strings.HasPrefix(p, "/runs/") &&
		(strings.HasSuffix(p, "/cancel") || strings.HasSuffix(p, "/retry")):
		return user.RoleOperator
	case strings.HasPrefix(p, "/templates/") && strings.HasSuffix(p, "/launch"):
		return user.RoleOperator
	default:
		return user.RoleAdmin
	}
}

// roleAllows reports whether a role meets a requirement, with admin above operator above viewer.
func roleAllows(have, need user.Role) bool {
	rank := map[user.Role]int{user.RoleViewer: 1, user.RoleOperator: 2, user.RoleAdmin: 3}
	return rank[have] >= rank[need]
}

// forbidden writes a 403 with a JSON error body.
func forbidden(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusForbidden)
	_, _ = w.Write([]byte(`{"error":"forbidden"}`))
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
	// Sign in must be reachable while the API is enforced.
	if r.Method == http.MethodPost && r.URL.Path == "/auth/login" {
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
