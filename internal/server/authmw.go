package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/beatfeed"
	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/auth"
	"github.com/kordloom/switchtender/internal/run"
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
	// alwaysEnforce declares this install authenticates whatever the tables hold, so open mode is
	// never entered. It is set for an install configured with single sign-on, whose tables are
	// legitimately empty until the first person signs in.
	alwaysEnforce bool
	// mu guards enforced and checkedAt.
	mu sync.Mutex
	// enforced caches whether the install is configured and so must authenticate.
	enforced bool
	// latched records that enforcement has been on once. It never returns to off in this process,
	// so an emptied or briefly unreadable store cannot reopen a running server.
	latched bool
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
			// A webhook is in the same position as a sign-in and for the same reason: the path is
			// the credential, so anyone on the network can present a guess, and each guess wrote a
			// permanent hash-linked entry. Fifty probes made fifty entries, and because the append
			// is fail-closed, filling the store that way eventually refuses every real mutation in
			// the install. A hook that resolves to a trigger is recorded by the handler, where the
			// trigger is known and the entry can say which one fired.
			if !isSignIn(r) && !isHook(r) {
				receipt, ok := g.record(w, recordedActor{
					Name: unauthenticatedActor(r), Type: actorTypeUnauthenticated,
				}, r)
				if !ok {
					return
				}
				r = r.WithContext(run.WithAuditReceipt(r.Context(), receipt))
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
			actor := Actor{UserID: u.ID, Role: u.Role, Name: u.Username, Type: actorTypeSession}
			if !g.decide(w, r, actor) {
				return
			}
			receipt, ok := g.record(w, recordedActor{
				Name: u.Username, Type: actorTypeSession,
			}, r)
			if !ok {
				return
			}
			ctx := run.WithAuditReceipt(context.WithValue(r.Context(), actorKey{}, actor), receipt)
			ctx = g.stampSubmitterOrg(ctx, actor)
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
		role, boundUser, err := g.roleFor(r.Context(), tok)
		if err != nil {
			unauthorized(w)
			return
		}
		// An agent token is capped at operator no matter what account it is bound to, so it can
		// launch and propose work but can never manage identity, access, or secrets, and can never
		// approve its own held run. Every route that does those is admin, so one ceiling closes the
		// whole surface at the door. Capping here, before decide, means the guarantee holds however
		// the agent reaches the API, not only through the MCP client that also restricts it.
		actorType := actorTypeToken
		switch {
		case tok.IsAgent():
			actorType = actorTypeAgent
			role = capAgentRole(role)
		case tok.IsSession():
			// A person at a browser is a session, not a script. The chain recorded every interactive
			// change as actor_type "token", which is exactly the distinction the identity stage of the
			// boundary exists to make, and it made the session indistinguishable from a job's token in
			// every entry, dossier, and receipt.
			actorType = actorTypeSession
		}
		actor := Actor{UserID: tok.UserID, Role: role, Name: tok.Name, Agent: tok.IsAgent(),
			Type: actorType, TokenID: tok.ID}
		if !g.decide(w, r, actor) {
			return
		}
		g.touch(tok)
		// A token's label is chosen by whoever minted it and is not unique, so the label alone cannot
		// attribute a change: two tokens named "agent" on different accounts read identically. The
		// bound account is recorded beside it, which is also the delegation an agent operates under.
		// The type says whether a person or an agent held the token, which is set when the token is
		// minted rather than inferred from the request.
		receipt, ok := g.record(w, recordedActor{
			Name: tok.Name, Type: actorType, OnBehalfOf: boundUser,
		}, r)
		if !ok {
			return
		}
		ctx := run.WithAuditReceipt(context.WithValue(r.Context(), actorKey{}, actor), receipt)
		ctx = g.stampSubmitterOrg(ctx, actor)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// stampSubmitterOrg carries the actor's owning organization on the context so a run created while
// handling the request is stamped with the tenant that scopes an objectless run. Resolving it here,
// once per request beside the actor and the receipt, keeps every submit path stamping the same way
// without each handler re-deriving it. A resolution failure leaves the org unset, which fails closed
// for reads: an unowned objectless run is visible only to an admin under strict grants.
func (g *authGate) stampSubmitterOrg(ctx context.Context, actor Actor) context.Context {
	if g.authz == nil {
		return ctx
	}
	orgID, err := g.authz.submitterOrg(ctx, actor)
	if err != nil {
		g.log.Error("server: resolve submitter org: " + err.Error())
		return ctx
	}
	return run.WithSubmitterOrg(ctx, orgID)
}

// recordedActor is the identity written into one audit entry.
//
// Each field is something the server observed rather than something it inferred. Type says how the
// caller authenticated, not what kind of thing they are: a token cannot tell whether a person or an
// agent is holding it, and writing a guess into a signed record would be the one thing this trail
// must never do. OnBehalfOf carries the account a token is bound to, which is observed directly and
// is what distinguishes two tokens sharing a label on different accounts.
type recordedActor struct {
	// Name is the actor as it appears in the trail: a token label, a username, or a caller class.
	Name string
	// Type is how the caller authenticated: session, token, webhook, saml, or unauthenticated.
	Type string
	// OnBehalfOf is the account whose authority the actor used, empty when it acted as itself.
	OnBehalfOf string
}

// Actor types, naming how a caller authenticated.
const (
	// actorTypeSession is an interactive sign-in, a person at a browser or an SSO session.
	actorTypeSession = "session"
	// actorTypeToken is an API token held by a person or a script.
	actorTypeToken = "token"
	// actorTypeAgent is a token minted for an AI agent, declared at issuance and recorded so the
	// chain attributes the change to an agent rather than leaving a reader to guess.
	actorTypeAgent = "agent"
	// actorTypeUnauthenticated is a caller that presented no credential, such as a webhook whose
	// path is its only secret.
	actorTypeUnauthenticated = "unauthenticated"
)

// digestBody reads the request body, returns the digest committed for it, and restores the body so
// the handler still reads it.
//
// A body that cannot be read fails the request closed. The alternative, recording an entry with no
// digest and letting the change proceed, would let a truncated body buy an unrecorded payload, which
// is the hole the digest exists to close.
//
// The read is bounded here rather than trusted to the bodyLimit middleware, because the digest runs
// for every mutating method and bodyLimit does not cap all of them the same way. The import and hook
// uploads are allowed a larger body and carry no secret by design, an export omits secret material
// and a hook authenticates by the token in its path, so they pass through undigested rather than
// buffered whole in the gate on top of the handler's own copy. Every other mutation, which is where
// a secret can appear, is read up to one byte past maxBodyBytes and refused if it exceeds it, so a
// DELETE with a multi-gigabyte body cannot exhaust memory.
func (g *authGate) digestBody(w http.ResponseWriter, r *http.Request) (digest, nonce string, ok bool) {
	if r.Body == nil || r.Body == http.NoBody {
		return "", "", true
	}
	if uploadPath(r.URL.Path) {
		return "", "", true
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	_ = r.Body.Close()
	if err != nil {
		respondError(w, g.log, http.StatusRequestEntityTooLarge,
			"the request body could not be read, so the change was not recorded or made")
		return "", "", false
	}
	if len(body) > maxBodyBytes {
		respondError(w, g.log, http.StatusRequestEntityTooLarge,
			"the request body exceeds the limit, so the change was not recorded or made")
		return "", "", false
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	// ContentLength is left as the client sent it; the restored body is the same bytes.
	digest, nonce, err = audit.ContentDigestOf(body)
	if err != nil {
		// The nonce comes from the system random source. If it cannot be read the change is refused
		// rather than recorded under a weaker digest, the same fail-closed stance the trail takes
		// when the entry cannot be appended at all.
		respondError(w, g.log, http.StatusServiceUnavailable,
			"refused: the change could not be recorded in the audit trail")
		return "", "", false
	}
	return digest, nonce, true
}

// actorKey is the context key under which the authenticated actor is stored.
type actorKey struct{}

// Actor is the authenticated caller resolved by the gate, carried in the request context so
// object-level authorization can identify the user and role behind a request.
type Actor struct {
	// UserID is the caller's account id, empty for a command-line admin token.
	UserID string
	// Role is the caller's global role, already capped when the caller is an agent.
	Role user.Role
	// Name is the token name, used for audit attribution.
	Name string
	// Agent reports whether an AI agent holds the token, so a handler that needs to treat an agent
	// differently can, without re-reading the token.
	Agent bool
	// Type is how the caller authenticated, in the audit chain's vocabulary: session, token, or
	// agent. It is stamped onto submitted runs so policies can tell who is asking.
	Type string
	// TokenID identifies the credential that authenticated this request, so a handler can revoke
	// exactly it. Empty for a sign-in that carries no stored token, such as a bearer JWT verified
	// against an issuer.
	TokenID string
}

// capAgentRole lowers an admin role to operator for an agent, and leaves any lower role unchanged.
// An agent may launch and propose work but must not manage identity, access, or secrets, or approve
// its own held run, all of which are admin.
func capAgentRole(role user.Role) user.Role {
	if role == user.RoleAdmin {
		return user.RoleOperator
	}
	return role
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
	return hookPath(r.URL.Path) && r.Method == http.MethodPost
}

// hookPath reports whether p addresses a webhook, whatever method carries it and however the path
// is spelled.
//
// Redaction used to run only for a POST on an exactly spelled prefix, so a PUT, a DELETE, a doubled
// slash, or a different case fell through and wrote the raw token into the chain. The request is
// refused either way, but the entry is written before the refusal, and that entry is hash-linked,
// unredactable without breaking the chain, and carried into every bundle handed to a third party. A
// mistyped curl was enough to embed a live webhook token in an artifact meant to be shared.
func hookPath(p string) bool {
	clean := path.Clean("/" + strings.TrimPrefix(strings.ToLower(p), "/v1"))
	return strings.HasPrefix(clean, "/hooks/")
}

// isSignIn reports whether the request is an authentication attempt.
//
// These are the only unauthenticated mutations left out of the chain. They are reachable by anyone
// on the network, so recording each attempt let a stranger append without bound to the structure
// the integrity story rests on, and the fail-closed append then locked everyone out whenever the
// audit store was unhealthy. Every other unauthenticated mutation is recorded, including the ones
// that provision an account, which an earlier narrowing dropped by mistake.
//
// SAML belongs here for the same reason as the rest, and its omission was worse than the others.
// The OIDC callback is a GET and never reached the chain anyway, so the SAML assertion consumer was
// the only sign-in that did: an unhealthy audit store answered it 503, which in a SAML deployment
// is the login itself and locks every user out, and it was reachable without a credential and
// without a rate limiter, so a stranger could append to the chain without bound.
func isSignIn(r *http.Request) bool {
	p := strings.TrimPrefix(r.URL.Path, "/v1")
	return p == "/auth/login" || p == "/auth/logout" ||
		strings.HasPrefix(p, "/auth/oidc/") || strings.HasPrefix(p, "/auth/saml/")
}

// denyUnlessAdminOrActor refuses a caller who is neither an admin nor the actor recorded on this run,
// writing the refusal and reporting that the handler should stop. It is what makes a run's evidence
// readable by whoever asked for that run and by nobody else below admin.
//
// An install with authentication off has no actor to compare against and no roles to speak of, so
// everything is allowed there, exactly as every other check behaves.
func denyUnlessAdminOrActor(w http.ResponseWriter, r *http.Request, log *zap.Logger, rn *run.Run) bool {
	actor, ok := actorFrom(r.Context())
	if !ok {
		return false
	}
	if roleAllows(actor.Role, user.RoleAdmin) {
		return false
	}
	if rn != nil && sameActor(actor, rn) {
		return false
	}
	respondError(w, log, http.StatusForbidden,
		"reading a run's evidence needs the admin role, or that you are the actor who asked for it")
	return true
}

// sameActor reports whether the caller is the actor recorded on the run. The account is compared first
// and the credential's name only when one side has no account: a person's token and their browser
// session record different names, so a name comparison alone answers "same person" wrongly in the
// direction that matters.
func sameActor(actor Actor, rn *run.Run) bool {
	if actor.UserID != "" && rn.ActorUserID != "" {
		return actor.UserID == rn.ActorUserID
	}
	return actor.Name != "" && actor.Name == rn.Actor
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
	// Redaction keys on the path alone. The method decides whether a hook runs, never whether its
	// token is a secret.
	if !hookPath(r.URL.Path) {
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
func (g *authGate) record(w http.ResponseWriter, who recordedActor, r *http.Request) (receipt string, ok bool) {
	if g.audits == nil || r.Method == http.MethodGet || r.Method == http.MethodHead {
		return "", true
	}
	digest, nonce, ok := g.digestBody(w, r)
	if !ok {
		return "", false
	}
	entry := &audit.Entry{
		ID: audit.NewID(), At: time.Now(), Actor: who.Name,
		ActorType: who.Type, OnBehalfOf: who.OnBehalfOf,
		Method: r.Method, Path: auditPath(r), ContentDigest: digest, Nonce: nonce,
	}
	if err := g.audits.Append(r.Context(), entry); err != nil {
		g.log.Error("server: append audit entry: "+err.Error(),
			zap.String("method", r.Method), zap.String("path", auditPath(r)))
		respondError(w, g.log, http.StatusServiceUnavailable,
			"refused: the change could not be recorded in the audit trail")
		return "", false
	}
	receipt = audit.Receipt(entry)
	w.Header().Set(AuditReceiptHeader, receipt)
	return receipt, true
}

// errNoAccounts is returned when a token names an owning account but no account store is wired, so
// the token's role cannot be read.
var errNoAccounts = errors.New("token is bound to an account but accounts are not configured")

// roleFor resolves a token to its user's role. Tokens without an owner come from the command
// line and carry admin rights; tokens whose owner is gone stop working.
//
// A token that names an owner is refused when there is no account store to read that owner from.
// The missing store used to resolve to admin, which is the one direction an unreadable role must
// never move: a token minted for a viewer would have carried admin rights for as long as the store
// was absent, and nothing in the reply would have said so. Every path that binds a token to an
// account needs an account store to do it, so on a real serve path the store is always there and
// this is unreachable; if it ever becomes reachable it denies rather than promotes.
// It also returns the bound account's username, empty for an unbound token, so the audit entry can
// name the authority a token acted under rather than only the token's own label.
func (g *authGate) roleFor(ctx context.Context, tok *auth.Token) (user.Role, string, error) {
	if tok.UserID == "" {
		return user.RoleAdmin, "", nil
	}
	if g.users == nil {
		g.log.Error("server: " + errNoAccounts.Error())
		return "", "", errNoAccounts
	}
	u, err := g.users.Get(ctx, tok.UserID)
	if err != nil {
		return "", "", err
	}
	return u.Role, u.Username, nil
}

// requiredRole maps a request to the minimum role that may perform it. Reads are for viewers,
// launching and stopping work is for operators, and managing configuration is for admins.
func requiredRole(r *http.Request) user.Role {
	// Path checks compare against the unversioned path, so the /v1 API prefix does not repeat.
	p := strings.TrimPrefix(r.URL.Path, "/v1")
	// The audit trail is management data even to read. A run's evidence dossier embeds a slice of
	// it, the approver identities and chain entries over that run, so it takes the same role as
	// the trail it quotes rather than the viewer read its path shape suggests.
	// A signed receipt is drawn from the same trail: it carries the chain entries over that run,
	// the approver identities that decided it, and, on the contiguous shape, the entries recorded
	// between them. It takes the trail's role for the same reason the dossier does.
	if p == "/audit" || strings.HasPrefix(p, "/audit/") {
		return user.RoleAdmin
	}
	// A run's evidence and its signed receipt quote the trail, so they are management data, with one
	// exception the handlers enforce themselves: the actor who asked for that run may read the evidence
	// for it. Without that exception the whole point of an accountable machine principal was
	// unreachable, since the MCP server refuses an admin token by design and so could never call the
	// evidence tool it advertises. The gate lets an operator through and the handler decides whether
	// this is their own run; a viewer is stopped here.
	if strings.HasPrefix(p, "/runs/") &&
		(strings.HasSuffix(p, "/evidence") || strings.HasSuffix(p, "/receipt")) {
		return user.RoleOperator
	}
	// An account carries a profile of personal data, so listing accounts is management data even
	// to read. Without this a viewer could read every user's name, email, phone, and notes.
	// Tokens belong here for the same reason and more sharply: the list names every credential that
	// holds access to this install, which account each acts as, and when each was last used. It
	// carries no secret, but it is a map of what is worth stealing, and reading it is not a viewer's
	// business. Without this the GET fallback below would have made it one.
	if p == "/users" || strings.HasPrefix(p, "/users/") ||
		p == "/tokens" || strings.HasPrefix(p, "/tokens/") {
		return user.RoleAdmin
	}
	// Grants and approval policies decide who may do what, so they are management data even to
	// read, the same as the audit trail and the account list above. A viewer in any organization
	// could read the whole access map and every approval gate in the install, which is both a map
	// of what is worth attacking and a list of which changes nobody is watching.
	if p == "/grants" || strings.HasPrefix(p, "/grants/") ||
		p == "/policies" || strings.HasPrefix(p, "/policies/") {
		return user.RoleAdmin
	}
	// Organizations and teams are the subject side of every grant, so reading them reads half the
	// access map. Leaving them open to viewers while the grants themselves were admin only closed
	// one side of the same door: the membership list says who a grant to an organization actually
	// reaches. The doctor is here for the same reason, since it enumerates every template,
	// schedule, credential, inventory, and project in the install by id and name whether or not the
	// caller may use any of them.
	if p == "/orgs" || strings.HasPrefix(p, "/orgs/") ||
		p == "/teams" || strings.HasPrefix(p, "/teams/") ||
		p == "/credential-types" || strings.HasPrefix(p, "/credential-types/") ||
		p == "/doctor" {
		return user.RoleAdmin
	}
	// Reading the configuration that makes things run without a person is operator ground rather
	// than something every viewer needs, and listing it was unfiltered: there is no organization on
	// these objects to filter by, so the role is what bounds them.
	//
	// This raises reads only. Sitting above the GET check it was method-blind, and since the switch
	// below defaults to admin, it quietly lowered every write on these three families from admin to
	// operator. Nothing tested it, because the test that came with it asserted GET paths only.
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		if p == "/schedules" || strings.HasPrefix(p, "/schedules/") ||
			p == "/triggers" || strings.HasPrefix(p, "/triggers/") ||
			p == "/inventory-sources" || strings.HasPrefix(p, "/inventory-sources/") {
			return user.RoleOperator
		}
		return user.RoleViewer
	}
	switch {
	case p == "/auth/check", p == "/auth/logout":
		// Ending your own session is not management: it acts on the credential the caller already
		// holds and on nothing else, so every role may do it. Requiring admin here left a viewer
		// signed in with no way out.
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
		(strings.HasSuffix(p, "/cancel") || strings.HasSuffix(p, "/retry") ||
			strings.HasSuffix(p, "/relaunch-failed")):
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
	// An agent gets its role and nothing more. The cap lowers an agent to operator so it can never
	// manage identity, access, or secrets, and the manage-grant path walked straight around it: an
	// agent token inherited whatever object grants its human held, which let it replace the secret
	// inside a credential and delete the credential a schedule depended on. Delegation is a person
	// lending authority to another person; it is not a channel an agent operates through.
	if actor.Agent {
		return false, nil
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
	if r.Method == http.MethodGet && p == "/readyz" {
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
	// The span beat feed is how an outside watcher notices a chain that went quiet or lost its
	// tail. Like the trust document, it exists for a party with no account here, and a feed that
	// needs a token cannot be watched by the one the record is meant to convince.
	if r.Method == http.MethodGet && p == beatfeed.FeedPath {
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
	//
	// The same test decides this as decides whether the token is redacted from the record. They
	// used to differ: this one compared the raw path and the redaction cleaned it first, so
	// /hooks/<token>/../../probe read as public here and as not-a-hook there. That combination is
	// the worst of both: an unauthenticated stranger appended a permanent hash-linked entry for
	// every probe, and because redaction had already decided it was not a hook, the presented token
	// went into the chain verbatim and travels in every bundle handed to a third party.
	if isHook(r) {
		return false
	}
	return true
}

// enforcing reports whether the install has been configured and so must authenticate, cached briefly
// to keep request overhead flat.
//
// Open mode is a first-run convenience, reachable only before anything has been set up. It is not a
// state an install may return to. Enforcement keyed on the live token count alone did allow that:
// session tokens carry a thirty day lifetime, this gate deletes an expired token the moment it meets
// one, and the count then reached zero. A browser or single sign-on install, whose only tokens are
// sessions, therefore served its whole API to anonymous callers with admin authority thirty days
// after the last sign-in, silently. Revoking the last API token did it immediately.
//
// Two things close it. An account is durable where a session is not, so an install that has ever had
// a user keeps authenticating across restarts. And enforcement latches in memory, so a store that
// briefly reads empty cannot reopen a running server.
func (g *authGate) enforcing(ctx context.Context) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.enforced && g.latched {
		return true
	}
	if time.Since(g.checkedAt) < enforcementCacheTTL {
		return g.enforced
	}
	g.enforced = g.configured(ctx)
	if g.enforced {
		g.latched = true
	}
	g.checkedAt = time.Now()
	return g.enforced
}

// configured reports whether this install has any credential or account, meaning setup has happened
// and authentication applies. An unreadable store counts as configured, so a database problem cannot
// open the API.
func (g *authGate) configured(ctx context.Context) bool {
	// An install told it authenticates does, from the first request, before any table has a row.
	// Deriving this from the tables alone is what served an SSO install's whole API to anonymous
	// callers as admin until somebody happened to sign in.
	if g.alwaysEnforce {
		return true
	}
	n, err := g.tokens.Count(ctx)
	if err != nil {
		g.log.Error("server: count tokens: " + err.Error())
		return true
	}
	if n > 0 {
		return true
	}
	if g.users == nil {
		return false
	}
	accounts, err := g.users.List(ctx)
	if err != nil {
		g.log.Error("server: list users: " + err.Error())
		return true
	}
	return len(accounts) > 0
}

// touch records the token's last use, at most once a minute, without blocking the request.
func (g *authGate) touch(tok *auth.Token) {
	if tok.LastUsedAt != nil && time.Since(*tok.LastUsedAt) < time.Minute {
		return
	}
	now := time.Now()
	tok.LastUsedAt = &now
	// Touch updates the stored row and cannot create one. Saving the whole token here re-inserted it,
	// so an admin who revoked a token while a request was in flight watched it come back: the
	// attacker's own polling kept firing touches, and each one restored the row that had just been
	// deleted. A note about a request that already happened must never resurrect the authority for
	// the next one.
	go func() {
		if err := g.tokens.Touch(context.Background(), tok.ID, now); err != nil {
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

// authLogoutHandler ends the caller's own session by revoking the token that authenticated the
// request, so the credential stops working everywhere rather than only in the tab that dropped it.
//
// A session token lives for thirty days. Before this there was no way to end one at all: signing out
// could only clear the browser's copy, and anyone who had obtained the token, from a shared machine,
// a synced profile, or a copied header, kept full use of the account for the rest of that month. A
// person who suspects they left a session somewhere needs the credential itself invalidated.
//
// Only a session is revoked. An API token used to browse belongs to whatever else holds it, a
// scheduled job most likely, and a browser tab closing must not take that down. The response is the
// same either way, because from the caller's side the session is over regardless.
func authLogoutHandler(tokens auth.Store, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, ok := actorFrom(r.Context())
		if !ok {
			// An install running without authentication has no session to end, and no caller to
			// identify. Saying so is more honest than reporting a sign out that did nothing.
			respondError(w, log, http.StatusConflict,
				"this install runs without authentication, so there is no session to end")
			return
		}
		if actor.Type == actorTypeSession && actor.TokenID != "" && tokens != nil {
			if err := tokens.Delete(r.Context(), actor.TokenID); err != nil &&
				!errors.Is(err, auth.ErrNotFound) {
				log.Error("server: revoke session: " + err.Error())
				respondError(w, log, http.StatusInternalServerError, "could not end the session")
				return
			}
			log.Info("server: sign-out", zap.String("username", actor.Name))
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// authMeHandler tells an authenticated caller who the server resolved it to be: the audit name,
// the effective role after any agent cap, and how it authenticated. The UI gates its controls
// with this instead of guessing, which used to make every token session look like an admin and
// rendered buttons whose only future was a 403.
func authMeHandler(log *zap.Logger) http.HandlerFunc {
	type me struct {
		// Name is the caller's audit name: the username or the token label.
		Name string `json:"name,omitempty"`
		// Role is the effective role, after the agent cap.
		Role string `json:"role,omitempty"`
		// ActorType is how the caller authenticated, in the audit chain's vocabulary.
		ActorType string `json:"actor_type,omitempty"`
		// Open reports the API is running without authentication, where there is nobody to be.
		Open bool `json:"open,omitempty"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		out := me{}
		if a, ok := actorFrom(r.Context()); ok {
			out.Name, out.Role, out.ActorType = a.Name, string(a.Role), a.Type
		} else {
			out.Open = true
		}
		respondJSON(w, log, http.StatusOK, out, wantsPretty(r))
	}
}
