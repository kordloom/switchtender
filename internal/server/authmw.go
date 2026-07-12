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
	// jwt validates a bearer JWT when configured, nil when JWT sign-in is off.
	jwt *JWTAuth
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
		if g.jwt != nil && looksLikeJWT(plain) {
			u, err := g.jwt.Authenticate(r.Context(), plain)
			if err != nil {
				unauthorized(w)
				return
			}
			if !roleAllows(u.Role, requiredRole(r)) {
				forbidden(w)
				return
			}
			g.record(u.Username, r)
			ctx := context.WithValue(r.Context(), actorKey{},
				Actor{UserID: u.ID, Role: u.Role, Name: u.Username})
			next.ServeHTTP(w, r.WithContext(ctx))
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
		g.record(tok.Name, r)
		ctx := context.WithValue(r.Context(), actorKey{},
			Actor{UserID: tok.UserID, Role: role, Name: tok.Name})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// actorKey is the context key under which the authenticated actor is stored.
type actorKey struct{}

// Actor is the authenticated caller resolved by the gate, carried in the request context so
// object-level authorization can identify the user and role behind a request.
type Actor struct {
	// UserID is the caller's account id, empty for a command-line admin token.
	UserID string
	// Role is the caller's global role.
	Role user.Role
	// Name is the token name, used for audit attribution.
	Name string
}

// actorFrom returns the authenticated actor from the context, and whether one was present. It is
// absent when the API is not enforcing tokens, in which case authorization is open.
func actorFrom(ctx context.Context) (Actor, bool) {
	a, ok := ctx.Value(actorKey{}).(Actor)
	return a, ok
}

// record appends an audit entry for an authenticated mutation without blocking the request. It
// takes the actor name directly so token and JWT callers are both recorded on the same trail.
func (g *authGate) record(actor string, r *http.Request) {
	if g.audits == nil || r.Method == http.MethodGet || r.Method == http.MethodHead {
		return
	}
	entry := &audit.Entry{
		ID: audit.NewID(), At: time.Now(), Actor: actor,
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
	// Path checks compare against the unversioned path, so the /v1 API prefix does not repeat.
	p := strings.TrimPrefix(r.URL.Path, "/v1")
	// The audit trail is management data even to read.
	if p == "/audit" || strings.HasPrefix(p, "/audit/") {
		return user.RoleAdmin
	}
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return user.RoleViewer
	}
	switch {
	case p == "/auth/check":
		return user.RoleViewer
	case p == "/runs", p == "/pipelines":
		return user.RoleOperator
	case strings.HasPrefix(p, "/runs/") && strings.HasSuffix(p, "/explain"):
		// Explaining a run is an advisory read, so a viewer may ask.
		return user.RoleViewer
	case p == "/ai/draft":
		// A draft feeds execution configuration, so it takes the same role as launching work.
		return user.RoleOperator
	case p == "/drift/reconcile":
		// Proposing a reconcile is operator work. Releasing the held proposal stays admin work.
		return user.RoleOperator
	case p == "/ai/ask":
		// Asking about the fleet is an advisory read over data a viewer can already see.
		return user.RoleViewer
	case p == "/ai/propose-run":
		// Proposing a run is operator work, the same role that launches one. The proposal is held
		// for approval, so releasing it stays admin work.
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

// looksLikeJWT reports whether a bearer credential is a JWT rather than a Yardmaster token, so the
// gate routes it to JWT validation. A JWT is three base64 segments joined by two dots, which a
// Yardmaster token never carries.
func looksLikeJWT(s string) bool {
	return strings.Count(s, ".") == 2
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
	// Compare against the unversioned path so the /v1 API prefix does not repeat, while the bare
	// infrastructure paths (healthz, the UI, OIDC, hooks) still match unchanged.
	p := strings.TrimPrefix(r.URL.Path, "/v1")
	if r.Method == http.MethodGet && p == "/healthz" {
		return false
	}
	if r.Method == http.MethodGet && (p == "/" || strings.HasPrefix(p, "/ui/")) {
		return false
	}
	// Sign in must be reachable while the API is enforced.
	if r.Method == http.MethodPost && p == "/auth/login" {
		return false
	}
	// The single sign-on handshake runs before the user has a token.
	if r.Method == http.MethodGet && strings.HasPrefix(p, "/auth/oidc/") {
		return false
	}
	// Webhook triggers carry their own secret token in the path, so they bypass the token gate.
	if r.Method == http.MethodPost && strings.HasPrefix(p, "/hooks/") {
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
	p := strings.TrimPrefix(r.URL.Path, "/v1")
	return r.Method == http.MethodGet && strings.HasSuffix(p, "/stream") &&
		strings.HasPrefix(p, "/runs/")
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
