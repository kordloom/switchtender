package server

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/auth"
	"github.com/kordloom/switchtender/internal/user"
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
	// authz enforces object grants so a manage grant can delegate editing a specific object beyond
	// the global role. Nil leaves only the global role gate in force.
	authz *authorizer
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
			// A webhook trigger starts real runs without a token, so it is recorded. Sign-in is
			// not.
			//
			// Recording every unauthenticated request was worse than the gap it closed. Sign-in and
			// SAML mint no run and change no configuration, but they are reachable by anyone on the
			// network, so each failed attempt appended a permanent, hash-linked entry: an unbounded
			// append by strangers into the structure the product's integrity story rests on. And
			// because the append is fail-closed, an unhealthy audit store then locked every account
			// out of signing in, including on a fresh install with no token yet. Authentication
			// attempts belong in the log, which already carries them.
			if !isSignIn(r) && !g.record(w, unauthenticatedActor(r), r) {
				return
			}
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
			actor := Actor{UserID: u.ID, Role: u.Role, Name: u.Username}
			if !g.decide(w, r, actor) {
				return
			}
			if !g.record(w, u.Username, r) {
				return
			}
			ctx := context.WithValue(r.Context(), actorKey{}, actor)
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
		actor := Actor{UserID: tok.UserID, Role: role, Name: tok.Name}
		if !g.decide(w, r, actor) {
			return
		}
		g.touch(tok)
		if !g.record(w, tok.Name, r) {
			return
		}
		ctx := context.WithValue(r.Context(), actorKey{}, actor)
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

// isHook reports whether the request is a webhook trigger, which authenticates by a secret in its
// path rather than by a token header. It matches with and without the version prefix, because
// protects strips one and the two must agree about what a hook is.
func isHook(r *http.Request) bool {
	p := strings.TrimPrefix(r.URL.Path, "/v1")
	return r.Method == http.MethodPost && strings.HasPrefix(p, "/hooks/")
}

// isSignIn reports whether the request is an authentication attempt.
//
// These are the only unauthenticated mutations left out of the chain. They are reachable by anyone
// on the network, so recording each attempt let a stranger append without bound to the structure
// the integrity story rests on, and the fail-closed append then locked everyone out whenever the
// audit store was unhealthy. Every other unauthenticated mutation is recorded, including the ones
// that provision an account, which an earlier narrowing dropped by mistake.
func isSignIn(r *http.Request) bool {
	p := strings.TrimPrefix(r.URL.Path, "/v1")
	return p == "/auth/login" || p == "/auth/logout" || strings.HasPrefix(p, "/auth/oidc/")
}

// unauthenticatedActor names the kind of caller on a path that carries no token.
func unauthenticatedActor(r *http.Request) string {
	switch {
	case isHook(r):
		return "webhook"
	case strings.HasSuffix(strings.TrimPrefix(r.URL.Path, "/v1"), "/auth/saml/acs"):
		return "saml"
	default:
		return "unauthenticated"
	}
}

// auditPath returns the request path with anything secret removed, which is what the chain records.
//
// A webhook authenticates by a secret in its path, and the trigger store deliberately keeps only
// that secret's SHA-256 because the token itself must never persist. Recording the raw path put it
// back: hash-linked, unredactable without breaking the chain, and carried into every bundle handed
// to a third party. A failed probe of a guessed path would have been written down just as
// permanently.
func auditPath(r *http.Request) string {
	if !isHook(r) {
		return r.URL.Path
	}
	// Everything after the prefix is redacted, not just the first segment. An encoded slash inside
	// a token would otherwise split it and record the tail, and nothing downstream needs the rest.
	return "/hooks/[redacted]"
}

// AuditReceiptHeader carries the chain position of the entry recorded for a mutation, as
// "seq:hash". A caller that keeps its receipts can later demand that a chain contain them, which is
// the only way an omitted entry becomes detectable by the party it happened to. A chain proves that
// what it holds was not altered; it cannot prove that nothing is missing, because the same process
// decides both what happens and what gets written down. A receipt moves that from the server's word
// to the holder's evidence.
const AuditReceiptHeader = "Audit-Receipt"

// record appends an audit entry for an authenticated mutation and reports whether it was written.
//
// It runs before the handler, so a change that cannot be recorded does not happen. That ordering is
// the whole point: the append used to run in a goroutine whose error was logged and dropped, so a
// full disk or a locked database meant the mutation succeeded, returned 200, and left no trace. The
// chain showed no gap either, because a sequence number is assigned at append and an entry that was
// never appended leaves no hole to notice.
//
// Recording before the change means the trail holds attempts rather than outcomes: a request that
// is recorded and then rejected by its handler leaves an entry for something that did not take
// effect. That is the better direction to be wrong in. An attempt that failed is worth seeing, and
// the alternative ordering loses the record entirely whenever a process dies mid-change.
//
// It takes the actor name directly so token and JWT callers are recorded on the same trail.
func (g *authGate) record(w http.ResponseWriter, actor string, r *http.Request) bool {
	if g.audits == nil || r.Method == http.MethodGet || r.Method == http.MethodHead {
		return true
	}
	entry := &audit.Entry{
		ID: audit.NewID(), At: time.Now(), Actor: actor,
		Method: r.Method, Path: auditPath(r),
	}
	if err := g.audits.Append(r.Context(), entry); err != nil {
		g.log.Error("server: append audit entry: "+err.Error(),
			zap.String("method", r.Method), zap.String("path", auditPath(r)))
		respondError(w, g.log, http.StatusServiceUnavailable,
			"refused: the change could not be recorded in the audit trail")
		return false
	}
	w.Header().Set(AuditReceiptHeader, strconv.FormatInt(entry.Seq, 10)+":"+entry.Hash)
	return true
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
	// An account carries a profile of personal data, so listing accounts is management data even
	// to read. Without this a viewer could read every user's name, email, phone, and notes.
	if p == "/users" || strings.HasPrefix(p, "/users/") {
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

// decide applies the authorization decision for actor on r, writing the denial or error response and
// reporting whether the caller should proceed.
func (g *authGate) decide(w http.ResponseWriter, r *http.Request, actor Actor) bool {
	allow, err := g.allowed(r.Context(), actor, r)
	if err != nil {
		g.log.Error("server: authorize: " + err.Error())
		respondError(w, g.log, http.StatusInternalServerError, "could not authorize request")
		return false
	}
	if !allow {
		forbidden(w)
		return false
	}
	return true
}

// allowed reports whether actor may perform r. The global role gate decides first. When the role is
// insufficient and r edits or deletes a single grantable object, an explicit manage grant on that
// object authorizes it, so managing a specific object can be delegated without the global admin role.
// The manage path is additive: it only ever allows a request the role gate would otherwise deny.
func (g *authGate) allowed(ctx context.Context, actor Actor, r *http.Request) (bool, error) {
	if roleAllows(actor.Role, requiredRole(r)) {
		return true, nil
	}
	object := delegatedObject(r)
	if object == "" {
		return false, nil
	}
	return g.authz.manages(ctx, actor, object)
}

// delegatedObjectKinds are the resource paths whose edit and delete a manage grant can delegate.
// They match the grant package's grantable object kinds.
var delegatedObjectKinds = map[string]bool{
	"projects": true, "templates": true, "inventories": true, "credentials": true,
}

// delegatedObject returns the object id an edit or delete targets when the request is a manage-
// delegable mutation, or empty otherwise. It matches only an exact PUT or DELETE on a single
// grantable object, so creates, sub-resources, and non-grantable paths never qualify.
func delegatedObject(r *http.Request) string {
	if r.Method != http.MethodPut && r.Method != http.MethodDelete {
		return ""
	}
	p := strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1"), "/")
	parts := strings.Split(p, "/")
	if len(parts) != 2 || !delegatedObjectKinds[parts[0]] {
		return ""
	}
	return parts[1]
}

// looksLikeJWT reports whether a bearer credential is a JWT rather than a SwitchTender token, so the
// gate routes it to JWT validation. A JWT is three base64 segments joined by two dots, which a
// SwitchTender token never carries.
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
	// The trust document is how a third party learns which key signs this install's bundles. They
	// have no account here, and requiring one would defeat a record meant to be checkable without us.
	if r.Method == http.MethodGet && p == "/.well-known/loomseal.json" {
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
	// The SAML handshake also runs before the user has a token: the login redirect, the metadata an
	// IdP administrator reads, and the assertion the identity provider posts back to the ACS.
	if strings.HasPrefix(p, "/auth/saml/") &&
		(r.Method == http.MethodGet || (r.Method == http.MethodPost && p == "/auth/saml/acs")) {
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
