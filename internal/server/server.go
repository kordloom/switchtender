// Package server exposes the SwitchTender HTTP API over the run store and dispatcher.
package server

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"net/url"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/beatfeed"
	"github.com/kordloom/switchtender/internal/ai"
	"github.com/kordloom/switchtender/internal/attention"
	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/auth"
	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/decision"
	"github.com/kordloom/switchtender/internal/dispatch"
	"github.com/kordloom/switchtender/internal/factcache"
	"github.com/kordloom/switchtender/internal/federation"
	"github.com/kordloom/switchtender/internal/forgelink"
	"github.com/kordloom/switchtender/internal/grant"
	"github.com/kordloom/switchtender/internal/importer"
	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/invsource"
	"github.com/kordloom/switchtender/internal/live"
	"github.com/kordloom/switchtender/internal/notification"
	"github.com/kordloom/switchtender/internal/org"
	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/project"
	"github.com/kordloom/switchtender/internal/relay"
	"github.com/kordloom/switchtender/internal/review"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/schedule"
	"github.com/kordloom/switchtender/internal/team"
	"github.com/kordloom/switchtender/internal/template"
	"github.com/kordloom/switchtender/internal/trigger"
	"github.com/kordloom/switchtender/internal/ui"
	"github.com/kordloom/switchtender/internal/user"
)

// Submitter accepts a run request and returns the created run. The dispatcher satisfies it.
type Submitter interface {
	Submit(ctx context.Context, playbook, inventory string, opts ...run.SubmitOption) (*run.Run, error)
	SubmitSplit(ctx context.Context, playbook, inventory string, shards int, opts ...run.SubmitOption) (*run.Run, error)
	SubmitPipeline(ctx context.Context, name, inventory string, steps []run.PipelineStep, opts ...run.SubmitOption) (*run.Run, error)
}

// Streamer subscribes to a run's live output. The live Hub satisfies it.
type Streamer interface {
	Subscribe(id string) (<-chan live.Message, func())
}

// Canceler stops a pending or executing run. The dispatcher satisfies it.
type Canceler interface {
	// Cancel stops a run this process is executing and reports whether it held one.
	Cancel(id string) bool
	// CancelWaiting cancels a run waiting unclaimed, pending or held for approval, settles its end,
	// and reports whether it did.
	CancelWaiting(ctx context.Context, id string) (bool, error)
}

// Retrier starts a new split run from the failed shards of a finished one. The dispatcher
// satisfies it.
type Retrier interface {
	RetryFailedShards(ctx context.Context, parentID string, opts ...run.SubmitOption) (*run.Run, error)
	RelaunchFailedHosts(ctx context.Context, runID string, opts ...run.SubmitOption) (*run.Run, error)
}

// Approver releases or denies a run held for approval, naming the decider so the decision entry on
// the chain carries who decided, how they authenticated, and the account they acted for. The
// dispatcher satisfies it.
type Approver interface {
	Approve(ctx context.Context, id string, by outcome.Decider) (*run.Run, error)
	Reject(ctx context.Context, id, reason string, by outcome.Decider) (*run.Run, error)
}

// Option configures a Server.
type Option func(*Server)

// WithStreamer enables the live stream endpoint backed by s.
func WithStreamer(s Streamer) Option {
	return func(srv *Server) { srv.streamer = s }
}

// WithCanceler enables the cancel endpoint backed by c.
func WithCanceler(c Canceler) Option {
	return func(srv *Server) { srv.canceler = c }
}

// WithAnnouncer makes the relay announce, through a, the runs workers finish and the applies their
// plans leave held. The dispatcher satisfies it.
func WithAnnouncer(a relay.Announcer) Option {
	return func(srv *Server) { srv.announcer = a }
}

// WithSecretOpener lets the relay deliver a claimed run's credentials and secret answers, sealed to
// the delivery key the claiming worker's pool registered. The dispatcher satisfies it. Without it
// no relay worker receives a secret, and a run that needs one fails on a relay worker with the
// reason.
func WithSecretOpener(o relay.SecretOpener) Option {
	return func(srv *Server) { srv.secretOpener = o }
}

// WithPlanSealer lets the relay accept the plan file a worker's plan saved and seal it, with this
// control node's key, onto the apply proposed from it. The dispatcher satisfies it. Without it a
// plan-gated apply cannot complete on a relay worker, and the worker's proposal is refused with the
// reason.
func WithPlanSealer(p relay.PlanSealer) Option {
	return func(srv *Server) { srv.planSealer = p }
}

// WithRetrier enables the failed shard retry endpoint backed by r.
func WithRetrier(r Retrier) Option {
	return func(srv *Server) { srv.retrier = r }
}

// WithApprover enables the run approval endpoints backed by a.
func WithApprover(a Approver) Option {
	return func(srv *Server) { srv.approver = a }
}

// WithDecisions serves decision records from store: the reasons approvers gave, their corrections
// and redactions, and the separation-of-duties evaluations of agent-initiated runs, for the run's
// decisions route, its evidence dossier, and its receipt.
func WithDecisions(store decision.Store) Option {
	return func(srv *Server) { srv.decisions = store }
}

// WithForgeLinks keeps the links between forge accounts and SwitchTender accounts in store, and
// lets people link an account on each forge in apps through that forge's OAuth application. A pull
// request comment from a linked account acts as its SwitchTender account. Without it no comment
// acts as anybody, and every commenter is told how linking is set up.
func WithForgeLinks(store forgelink.Store, apps ...forgelink.App) Option {
	return func(srv *Server) {
		srv.forgeLinks = store
		srv.forgeApps = apps
	}
}

// WithSchedules enables the schedule endpoints backed by the given store.
func WithSchedules(store schedule.Store) Option {
	return func(srv *Server) { srv.schedules = store }
}

// WithTokens guards the API with bearer tokens from the given store. The API stays open until the
// first token exists.
func WithTokens(tokens auth.Store) Option {
	return func(srv *Server) { srv.tokens = tokens }
}

// WithUsers enables accounts: role enforcement on the gate, sign in, and the user endpoints.
func WithUsers(users user.Store) Option {
	return func(srv *Server) { srv.users = users }
}

// WithAudit records authenticated mutations to the given store and serves the trail.
func WithAudit(store audit.Store) Option {
	return func(srv *Server) { srv.audits = store }
}

// WithProducerIdentity publishes the install's signing identity so a relying party can pin the
// fingerprint that attributes a bundle to this install. Version stamps the trust document.
func WithProducerIdentity(id *audit.Identity, version string) Option {
	return func(srv *Server) {
		srv.producer = id
		srv.productVersion = version
	}
}

// WithProducerUnavailable records why this server has no signing identity, so a request for a
// signed bundle is told the reason instead of that the export is not enabled. Empty means there is
// nothing to explain.
func WithProducerUnavailable(reason string) Option {
	return func(srv *Server) { srv.producerProblem = reason }
}

// WithInventories enables the inventory endpoints backed by the given store.
func WithInventories(store inventory.Store) Option {
	return func(srv *Server) { srv.inventories = store }
}

// WithPolicies enables the approval policy endpoints backed by the given store.
func WithPolicies(store policy.Store) Option {
	return func(srv *Server) { srv.policies = store }
}

// WithTriggers enables webhook triggers that launch templates from inbound git pushes. The sealer
// seals per-trigger HMAC signing secrets and verifies inbound signatures; pass the same sealer used
// for credentials.
func WithTriggers(store trigger.Store, sealer *credential.Sealer) Option {
	return func(srv *Server) {
		srv.triggers = store
		srv.sealer = sealer
	}
}

// WithNotificationTargets enables the named notification target endpoints backed by the given
// store. A target's secrets are sealed with the sealer WithCredentials or WithTriggers set.
func WithNotificationTargets(store notification.Store) Option {
	return func(srv *Server) { srv.notifications = store }
}

// WithTeams enables the team endpoints backed by the given store.
func WithTeams(store team.Store) Option {
	return func(srv *Server) { srv.teams = store }
}

// WithOrgs enables the organization endpoints backed by the given store.
func WithOrgs(store org.Store) Option {
	return func(srv *Server) { srv.orgs = store }
}

// WithGrants enables per-object access grants. When strict is set, an object with no grants denies
// non-admins; otherwise the global role decides for ungranted objects.
func WithGrants(store grant.Store, strict bool) Option {
	return func(srv *Server) {
		srv.grants = store
		srv.strictGrants = strict
	}
}

// WithDocs serves the documentation tree inside the web UI. A nil filesystem disables the pages.
func WithDocs(docs fs.FS) Option {
	return func(srv *Server) { srv.docs = docs }
}

// WithReadOnly rejects every mutating request when set, so a public instance cannot be changed by
// its visitors.
func WithReadOnly(readOnly bool) Option {
	return func(srv *Server) { srv.readOnly = readOnly }
}

// WithDemo marks the server as the public demo, whose pages speak as a showcase. It implies nothing
// about what is allowed: WithReadOnly decides that.
func WithDemo(demo bool) Option {
	return func(srv *Server) { srv.demo = demo }
}

// WithShutdown gives the server the context that is canceled when the process begins draining. A
// live event stream ends on it instead of holding the graceful shutdown open for its whole timeout,
// since a stream only finishes when its run does. Without it, shutdown waits out every open stream.
func WithShutdown(ctx context.Context) Option {
	return func(srv *Server) { srv.shutdown = ctx }
}

// WithWorkerPools confines each worker token to the queues it may lease from.
//
// A queue routes work to the segment that can reach it, so the queues a token may claim are that
// token's blast radius. One shared token made the least trusted worker in the estate a path to the
// most trusted queue. Set this and a token that serves the DMZ cannot lease a production run.
func WithWorkerPools(pools *relay.Pools) Option {
	return func(srv *Server) { srv.workerPools = pools }
}

// WithRelay mounts the phase-1 mesh relay worker endpoints, backed by the given run store and
// guarded by the worker token. A relay worker in an isolated segment dials them over one outbound
// connection to lease and execute runs without a path to the database. An empty token leaves the
// endpoints off, so the mesh is opt-in.
func WithRelay(store run.Store, workerToken string) Option {
	return func(srv *Server) {
		srv.relayStore = store
		srv.workerToken = workerToken
	}
}

// DefaultMatrixCap is the host matrix cell limit used when none is configured. It is generous
// enough for large runs while keeping the browser from drawing a grid too big to read or render.
const DefaultMatrixCap = 50000

// WithMatrixCap sets the largest host matrix, in cells, the UI will draw. Past it the detail page
// shows a notice instead of the grid. A value of zero or less means no limit.
func WithMatrixCap(cap int) Option {
	return func(srv *Server) { srv.matrixCap = cap }
}

// WithOIDC enables single sign-on through the given OpenID Connect provider. When set, the sign-in
// page offers an SSO button and the /auth/oidc routes are served.
func WithOIDC(o *OIDCAuth) Option {
	return func(srv *Server) { srv.oidc = o }
}

// WithLDAP enables sign-in against an LDAP directory. When set, the login handler tries a local
// account first and then the directory, so directory and local accounts both work.
func WithLDAP(l *LDAPAuth) Option {
	return func(srv *Server) { srv.ldap = l }
}

// WithSAML enables single sign-on through a SAML identity provider. When set, the sign-in page
// offers a SAML button and the /auth/saml routes are served.
func WithSAML(a *SAMLAuth) Option {
	return func(srv *Server) { srv.saml = a }
}

// WithJWT enables bearer JWT authentication. When set, a request whose bearer token is a JWT is
// validated against the issuer's keys instead of the token store, so a service can present a JWT
// minted elsewhere.
// WithEnforcedAuth declares that this install authenticates, whatever the token and account tables
// currently hold, so the gate never falls back to open mode.
//
// Open mode exists so a fresh install works before anything is set up, and it was keyed on finding
// no tokens and no accounts. An install whose way in is single sign-on has exactly that shape: SSO
// provisions an account on the first sign-in, so before anybody has signed in there is nothing in
// either table, and the whole API was served to anonymous callers with admin authority. The sign-in
// and single sign-on handshake routes are exempt from the gate, so enforcing from the first request
// still lets the first person in.
func WithEnforcedAuth() Option {
	return func(s *Server) { s.enforceAuth = true }
}

func WithJWT(j *JWTAuth) Option {
	return func(srv *Server) { srv.jwt = j }
}

// WithInventorySources enables the dynamic inventory source endpoints.
func WithInventorySources(store invsource.Store, refresher SourceRefresher) Option {
	return func(srv *Server) {
		srv.invSources = store
		srv.refresher = refresher
	}
}

// WithInventoryPreviewer enables previewing the hosts a smart or constructed inventory resolves to.
func WithInventoryPreviewer(p InventoryPreviewer) Option {
	return func(srv *Server) { srv.previewer = p }
}

// WithTemplates enables the template endpoints backed by the given store.
func WithTemplates(store template.Store) Option {
	return func(srv *Server) { srv.templates = store }
}

// WithProjectFiles enables read-only browsing of project checkouts through the given syncer.
// Without it the file endpoints report that browsing is not enabled.
func WithProjectFiles(syncer *project.Syncer) Option {
	return func(srv *Server) { srv.syncer = syncer }
}

// WithProjects enables the project endpoints backed by the given store.
func WithProjects(store project.Store) Option {
	return func(srv *Server) { srv.projects = store }
}

// WithAI enables the advisory AI endpoints backed by the given provider. A nil provider leaves them
// disabled, so AI is off unless an operator configures it.
func WithAI(provider ai.Provider) Option {
	return func(srv *Server) { srv.ai = provider }
}

// WithCredentials enables the credential endpoints backed by the given store and sealer.
func WithCredentials(store credential.Store, sealer *credential.Sealer) Option {
	return func(srv *Server) {
		srv.credentials = store
		srv.sealer = sealer
	}
}

// WithCredentialTypes enables the operator-defined credential type endpoints.
func WithCredentialTypes(store credential.TypeStore) Option {
	return func(srv *Server) { srv.credTypes = store }
}

// Server wires the run store and submitter into an HTTP handler.
type Server struct {
	// store reads runs and their logs for the query endpoints.
	store run.Store
	// ai provides advisory completions such as explaining a run, nil when AI is off.
	ai ai.Provider
	// submitter accepts new runs.
	submitter Submitter
	// log records request handling activity.
	log *zap.Logger
	// web serves the embedded user interface.
	web *ui.UI
	// streamer backs the live stream endpoint when configured.
	streamer Streamer
	// canceler backs the cancel endpoint when configured.
	canceler Canceler
	// announcer tells the notification channels about what relay workers finish, nil when the
	// control node announces nothing for them.
	announcer relay.Announcer
	// secretOpener opens a claimed run's secrets for sealed delivery to a relay worker pool, nil when
	// the relay delivers none.
	secretOpener relay.SecretOpener
	// planSealer seals the plan file a relay worker's plan saved onto the apply proposed from it, nil
	// when this control node accepts none.
	planSealer relay.PlanSealer
	// retrier backs the failed shard retry endpoint when configured.
	retrier Retrier
	// approver backs the run approval endpoints when configured.
	approver Approver
	// decisions holds the decision records approvals, denials, and corrections leave beside the
	// chain, nil when the install keeps none.
	decisions decision.Store
	// schedules backs the schedule endpoints when configured.
	schedules schedule.Store
	// tokens backs API authentication when configured.
	tokens auth.Store
	// credentials backs the credential endpoints when configured.
	credentials credential.Store
	// credTypes backs the custom credential type endpoints when configured.
	credTypes credential.TypeStore
	// federation is the workload identity issuer whose discovery document and keys are served, nil
	// when no issuer URL is set.
	federation *federation.Issuer
	// sealer encrypts credential secrets.
	sealer *credential.Sealer
	// syncer browses project checkouts for the file endpoints when configured.
	syncer *project.Syncer
	// projects backs the project endpoints when configured.
	projects project.Store
	// templates backs the template endpoints when configured.
	templates template.Store
	// users backs accounts when configured.
	users user.Store
	// inventories backs the inventory endpoints when configured.
	inventories inventory.Store
	// policies backs the approval policy endpoints when configured.
	policies policy.Store
	// audits backs the audit trail when configured.
	audits audit.Store
	// producer is the install's signing identity, published so a verifier can pin it. Nil when the
	// install has none.
	producer *audit.Identity
	// producerProblem is why producer is nil, empty when there is nothing to explain.
	producerProblem string
	// runFiles is where this server stages run files, for the doctor, nil when it was not told.
	runFiles *runFilesState
	// productVersion stamps the trust document.
	productVersion string
	// invSources backs the dynamic inventory source endpoints when configured.
	invSources invsource.Store
	// refresher refreshes inventory sources when configured.
	refresher SourceRefresher
	// previewer resolves smart and constructed inventories for a preview, nil when not configured.
	previewer InventoryPreviewer
	// triggers backs webhook triggers when configured.
	triggers trigger.Store
	// notifications backs the named notification target endpoints when configured.
	notifications notification.Store
	// factCache backs the per-host fact cache endpoints when configured.
	factCache factcache.Store
	// callbackResolver resolves names for provisioning callback host matching. Nil uses the
	// system resolver.
	callbackResolver callbackResolver
	// callbackCfg holds the provisioning callback rate limits and the template limit check.
	callbackCfg callbackSettings
	// factCacheAdminOnly restricts reading cached facts to admins.
	factCacheAdminOnly bool
	// teams backs the team endpoints when configured.
	teams team.Store
	// orgs backs the organization endpoints when configured.
	orgs org.Store
	// grants backs per-object access grants when configured.
	grants grant.Store
	// strictGrants makes an object with no grants deny non-admins.
	strictGrants bool
	// docs is the documentation tree rendered inside the UI, nil when not wired.
	docs fs.FS
	// readOnly rejects mutating requests when set, for a public demo or any exposed install.
	readOnly bool
	// demo marks the public demo.
	demo bool
	// enforceAuth declares the install authenticates regardless of what the token and account
	// tables hold, so the gate never serves open mode. Set for an install whose way in is SSO.
	enforceAuth bool
	// matrixCap is the largest host matrix, in cells, the UI draws. Zero or less means no limit.
	matrixCap int
	// oidc enables single sign-on when configured, nil when SSO is off.
	oidc *OIDCAuth
	// saml enables SAML single sign-on when configured, nil when SAML is off.
	saml *SAMLAuth
	// ldap enables directory sign-in when configured, nil when LDAP is off.
	ldap *LDAPAuth
	// jwt validates a bearer JWT when configured, nil when JWT sign-in is off.
	jwt *JWTAuth
	// relayStore backs the mesh relay worker endpoints when a worker token is set, nil when the
	// relay is off.
	relayStore run.Store
	// workerToken guards the mesh relay worker endpoints, empty when the relay is off. It is the
	// single-pool form: one token, every queue.
	workerToken string
	// workerPools confines each worker token to the queues it may lease from, nil when the install
	// uses the single-token form.
	workerPools *relay.Pools
	// shutdown is canceled when the process begins draining, ending live streams. Nil when unset,
	// which leaves a stream running until its run ends or its client goes away.
	shutdown context.Context
	// reviewPublicURL is this server's public address, used to link a review comment to its run.
	reviewPublicURL string
	// reviewClient reaches the forge a review reports to, nil for the guarded default.
	reviewClient *http.Client
	// reviewInterval is how often a review plan is checked for a change to report, zero for the
	// default.
	reviewInterval time.Duration
	// reviews reports review plans to their pull requests, nil when this server cannot.
	reviews *review.Reporter
	// reviewStore holds what each review plan's pull request was last told, shown on its run.
	reviewStore review.Store
	// attention reads what is stopping work for the dashboard and the doctor, nil when not wired.
	attention *attention.Source
	// hooks runs the work webhook deliveries start, answering each sender before it stops waiting.
	hooks *hookFlights
	// forgeLinks holds the links between forge accounts and SwitchTender accounts, nil when no
	// pull request comment may act as anybody.
	forgeLinks forgelink.Store
	// forgeApps are the forge OAuth applications people link their accounts through.
	forgeApps []forgelink.App
}

// New returns a Server. It panics if store or submitter is nil; a nil logger becomes a no-op.
func New(store run.Store, submitter Submitter, log *zap.Logger, opts ...Option) *Server {
	if store == nil {
		panic("server: Store required")
	}
	if submitter == nil {
		panic("server: Submitter required")
	}
	if log == nil {
		log = zap.NewNop()
	}
	srv := &Server{store: store, submitter: submitter, log: log, hooks: newHookFlights()}
	for _, opt := range opts {
		opt(srv)
	}
	// The identity providers record a successful sign-in themselves. Sign-in is exempt from the
	// fail-closed append that covers every other mutation, so without this an SSO login left no
	// trace in the chain at all.
	if srv.saml != nil {
		srv.saml.WithAudits(srv.audits)
	}
	if srv.oidc != nil {
		srv.oidc.WithAudits(srv.audits)
	}
	srv.reviews = srv.newReviewReporter()
	oidcBrand := ""
	if srv.oidc != nil {
		oidcBrand = srv.oidc.Brand()
	}
	srv.web = ui.New(srv.log, srv.docs, srv.readOnly, srv.matrixCap, srv.oidc != nil, srv.saml != nil,
		srv.ai != nil, oidcBrand, ui.WithAccountCheck(srv.anyAccount), ui.WithTokenCheck(srv.anyToken),
		ui.WithSignInCheck(srv.signInRequired), ui.WithDemo(srv.demo),
		ui.WithFactCacheAdminOnly(srv.factCacheAdminOnly), ui.WithFeaturesOff(srv.featuresOff()...))
	return srv
}

// featuresOff names the optional features this install has switched off, so a page can skip asking
// for them. Each answers 404 when off, which the page reads as "not enabled", but the browser logs
// every 404 it sees, so a page that asked would open with errors in its console.
func (s *Server) featuresOff() []string {
	var off []string
	if s.tokens == nil {
		off = append(off, "tokens")
	}
	if s.credTypes == nil {
		off = append(off, "credential-types")
	}
	if s.federation == nil {
		off = append(off, "federation")
	}
	return off
}

// signInRequired reports whether the API refuses a request that carries no credential, so a page
// can send a signed-out visitor to sign in before it asks for anything. It asks what the gate asks:
// an install told it authenticates, or one holding any token or account. With no token store there
// is no gate, and nothing is refused.
func (s *Server) signInRequired() bool {
	return s.tokens != nil && (s.enforceAuth || s.anyToken() || s.anyAccount())
}

// anyAccount reports whether this install holds a user account, for the sign-in page. An unreadable
// user store counts as having one, so a database problem cannot make the page tell a reader their
// accounts are gone.
func (s *Server) anyAccount() bool {
	if s.users == nil {
		return false
	}
	accounts, err := s.users.List(context.Background())
	if err != nil {
		return true
	}
	return len(accounts) > 0
}

// canSign reports whether this install holds a producer identity, which is what a receipt, a signed
// bundle and the trust document all require. A shared-database install without one is the case the
// doctor exists to surface.
func (s *Server) canSign() bool { return s.producer != nil }

// anyToken reports whether this install holds an API token, for the sign-in page. An unreadable
// token store counts as holding one, so a database problem never produces the claim that the install
// is open.
func (s *Server) anyToken() bool {
	if s.tokens == nil {
		return false
	}
	n, err := s.tokens.Count(context.Background())
	if err != nil {
		return true
	}
	return n > 0
}

// demoLanding is where the demo's bare address lands: the runs an AI agent asked for that the
// built-in hold is keeping waiting, the list the hosted demo's banner and the homepage's first
// button open. The gate is the product, so a visitor who types the address, or follows a link
// naming only the host, starts at a change the gate stopped rather than at a dashboard any
// automation tool could show. The demo seeds the agent's held restart whatever tools the machine
// has, so the list has a run to show. The search is encoded the way the browser encodes it, so the
// address is the one the hosted demo's banner links to.
func demoLanding() string {
	q := `held_by:"` + policy.AgentDefaultName + `" status:pending_approval`
	return "/ui/runs?q=" + strings.ReplaceAll(url.QueryEscape(q), "+", "%20")
}

// heldLanding is where a read-only install that is not the demo lands: every run a rule is
// holding, since an install may have no agent runs at all.
const heldLanding = "/ui/runs?status=pending_approval"

// landing returns where the bare address sends a visitor: the agent's held runs on the read-only
// demo, every held run on any other read-only server, and the overview on a writable one.
func (s *Server) landing() string {
	switch {
	case s.readOnly && s.demo:
		return demoLanding()
	case s.readOnly:
		return heldLanding
	default:
		return "/ui/"
	}
}

// checkouts returns the reader for project checkouts, nil when this server has none. The syncer is
// returned through the interface only when it is set, so a server without one hands back a nil
// reader rather than a non-nil interface holding a nil syncer.
func (s *Server) checkouts() dispatch.CheckoutReader {
	if s.syncer == nil {
		return nil
	}
	return s.syncer
}

// Handler returns the HTTP handler serving the SwitchTender API and web interface.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	// tickets are the short-lived permissions the event stream is opened with, minted over the
	// ordinary header-authenticated route because EventSource cannot set a header.
	tickets := newStreamTickets(s.store)
	authz := &authorizer{
		grants: s.grants, teams: s.teams, orgs: s.orgs,
		orgOwners: s.orgResolver(), strict: s.strictGrants,
	}
	mux.Handle("GET /healthz", healthHandler())
	mux.Handle("GET /readyz", readyHandler(s.store))
	// The producer identity binds the tree profile's leaves, so every anchor check that may meet a
	// tree anchor carries it. The key travels with the name, so an anchor taken under this
	// install's earlier name recomputes rather than refusing. Without a producer it stays zero and
	// tree anchors are reported uncheckable rather than silently passed.
	var producer audit.Identity
	if s.producer != nil {
		producer = *s.producer
	}
	var health *chainHealth
	if s.audits != nil {
		health = newChainHealth(s.audits, producer)
	}
	mux.Handle("GET /metrics", metricsHandler(s.store, health, authz, s.log))
	mux.Handle("GET /v1/fleet", fleetHandler(s.store, authz, s.log))
	mux.Handle("GET /v1/drift", driftHandler(s.store, authz, s.log))
	mux.Handle("POST /v1/drift/reconcile", reconcileDriftHandler(s.store, s.submitter, authz, s.log))
	mux.Handle("GET /v1/hosts/{host}/runs", hostHistoryHandler(s.store, authz, s.log))
	mux.Handle("GET /v1/hosts/{host}/facts", hostFactsHandler(s.store, authz, s.log))
	mux.Handle("GET /v1/estate", estateHandler(s.store, authz, s.log))
	mux.Handle("GET /v1/estate/diff", estateDiffHandler(s.store, authz, s.log))
	mux.Handle("GET /v1/changes", changesHandler(s.store, authz, s.log))
	mux.Handle("GET /v1/changes/{change}", changeHandler(s.store, authz, s.log))
	mux.Handle("GET /v1/tasks", taskTrendsHandler(s.store, authz, s.log))
	mux.Handle("GET /v1/workers", workersHandler(s.store, authz, s.log))
	mux.Handle("GET /v1/audit", auditHandler(s.audits, s.log))
	mux.Handle("GET /v1/audit/verify", auditVerifyHandler(s.audits, producer, s.log))
	mux.Handle("GET /v1/audit/bundle", auditBundleHandler(s.audits, s.producer, s.producerProblem,
		s.productVersion, s.log))
	mux.Handle("GET /v1/audit/register", auditRegisterHandler(s.store, s.audits, producer, s.log))
	// Served unauthenticated: the beat feed exists so an outside watcher can see the chain is
	// alive and whole, and that watcher has no account here.
	mux.Handle("GET "+beatfeed.APIPath, auditBeatsHandler(s.audits, s.log))
	// Served unversioned and unauthenticated: a relying party checking a bundle has no account here,
	// and the document holds only the public half of the signing key.
	mux.Handle("GET /.well-known/loomseal.json", trustHandler(s.producer, s.productVersion, s.log))
	// Served unversioned and unauthenticated at the issuer URL, because the cloud verifying a run's
	// identity token fetches them from there and has no account here. They hold public keys only.
	mux.Handle("GET /.well-known/openid-configuration",
		federationDiscoveryHandler(s.federation, s.log))
	mux.Handle("GET /.well-known/jwks.json", federationJWKSHandler(s.federation, s.log))
	mux.Handle("GET /v1/federation/keys", federationKeysHandler(s.federation, s.log))
	mux.Handle("POST /v1/federation/keys/rotate", federationRotateHandler(s.federation, s.log))
	mux.Handle("POST /v1/federation/keys/rotate/emergency",
		federationEmergencyRotateHandler(s.federation, s.log))
	mux.Handle("POST /v1/runs", createRunHandler(s.submitter, authz, s.log))
	mux.Handle("POST /v1/pipelines", createPipelineHandler(s.submitter, authz, s.log))
	mux.Handle("POST /v1/runs/{id}/cancel", cancelRunHandler(s.store, s.canceler, authz, s.log))
	mux.Handle("POST /v1/runs/{id}/retry", retryRunHandler(s.store, s.retrier, authz, s.log))
	mux.Handle("POST /v1/runs/{id}/relaunch-failed", relaunchFailedHandler(s.store, s.retrier, authz, s.log))
	mux.Handle("POST /v1/runs/{id}/rerun", rerunRunHandler(s.store, s.submitter, authz, s.log))
	mux.Handle("POST /v1/runs/{id}/approve", approveRunHandler(s.approver, s.store, authz, s.log))
	mux.Handle("POST /v1/runs/{id}/reject", rejectRunHandler(s.approver, s.store, authz, s.log))
	mux.Handle("GET /v1/approvals", approvalsHandler(s.store, authz, s.log))
	mux.Handle("GET /v1/runs/{id}/decisions", runDecisionsHandler(s.store, s.decisions, authz, s.log))
	mux.Handle("POST /v1/runs/{id}/decisions/{decision}/corrections",
		addCorrectionHandler(s.store, s.approver, authz, s.log))
	mux.Handle("POST /v1/runs/{id}/decisions/{record}/redact",
		redactReasonHandler(s.store, s.approver, authz, s.log))
	mux.Handle("GET /v1/runs", listRunsHandler(s.store, authz, s.log))
	mux.Handle("GET /v1/runs/{id}",
		getRunHandler(s.store, s.checkouts(), authz, s.log, s.reviewStore))
	mux.Handle("GET /v1/runs/{id}/compare", runCompareHandler(s.store, authz, s.log))
	mux.Handle("GET /v1/runs/{id}/shards", runShardsHandler(s.store, authz, s.log))
	mux.Handle("GET /v1/runs/{id}/steps", runStepsHandler(s.store, authz, s.log))
	mux.Handle("GET /v1/runs/{id}/logs", runLogsHandler(s.store, authz, s.log))
	mux.Handle("GET /v1/runs/{id}/events", runEventsHandler(s.store, authz, s.log))
	mux.Handle("GET /v1/runs/{id}/notifications",
		runNotificationsHandler(s.store, s.notifications, authz, s.log))
	mux.Handle("GET /v1/runs/{id}/evidence",
		runEvidenceHandler(s.store, s.audits, producer, s.decisions, authz, s.log))
	mux.Handle("GET /v1/runs/{id}/receipt",
		runReceiptHandler(s.store, s.audits, s.producer, s.productVersion, s.decisions, authz, s.log))
	mux.Handle("POST /v1/runs/{id}/explain", explainRunHandler(s.store, s.ai, authz, s.log))
	mux.Handle("POST /v1/ai/draft", draftStepHandler(s.ai, s.log))
	mux.Handle("POST /v1/ai/ask", askFleetHandler(s.store, s.ai, authz, s.log))
	mux.Handle("POST /v1/ai/propose-run", proposeRunHandler(s.submitter, s.ai, s.log))
	// EventSource cannot set a header, so a stream is opened with a short-lived ticket minted here
	// over the ordinary header-authenticated route rather than with the caller's own bearer token in
	// the URL, which every reverse proxy logs.
	mux.Handle("POST /v1/runs/{id}/stream-ticket", streamTicketHandler(tickets, s.log))
	mux.Handle("GET /v1/runs/{id}/stream",
		runStreamHandler(s.streamer, s.store, authz, s.log, s.shutdown))
	mux.Handle("GET /v1/schedules/preview", previewScheduleHandler(s.log))
	mux.Handle("GET /v1/doctor", doctorHandler(s.templates, s.schedules, s.credentials, s.inventories, s.projects,
		s.canSign, s.runFiles, ansibleCoreOf(s.previewer), s.log,
		notificationDoctor(s.notifications, s.notificationObjects(), s.log), s.attentionCheck))
	mux.Handle("GET /v1/attention", attentionHandler(s.attention, authz, s.log))
	mux.Handle("POST /v1/schedules", createScheduleHandler(s.schedules, authz, s.log))
	mux.Handle("GET /v1/schedules", listSchedulesHandler(s.schedules, authz, s.log))
	mux.Handle("GET /v1/schedules/{id}", getScheduleHandler(s.schedules, authz, s.log))
	mux.Handle("PUT /v1/schedules/{id}", updateScheduleHandler(s.schedules, authz, s.log))
	mux.Handle("DELETE /v1/schedules/{id}", deleteScheduleHandler(s.schedules, authz, s.log))
	mux.Handle("/ui/", s.web.Handler())
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, s.landing(), http.StatusFound)
	})
	mux.Handle("POST /v1/auth/check", authCheckHandler())
	mux.Handle("GET /v1/auth/me", authMeHandler(s.log))
	mux.Handle("POST /v1/auth/logout", authLogoutHandler(s.tokens, s.log))
	// Token management is admin work by the default in requiredRole, which is what keeps an agent's
	// capped token from minting itself a wider one.
	mux.Handle("GET /v1/tokens", listTokensHandler(s.tokens, s.log))
	mux.Handle("POST /v1/tokens", createTokenHandler(s.tokens, s.users, s.log))
	mux.Handle("DELETE /v1/tokens/{id}", deleteTokenHandler(s.tokens, s.log))
	mux.Handle("POST /v1/auth/login", loginHandler(s.users, s.tokens, s.ldap, s.store, s.log))
	if s.oidc != nil {
		mux.HandleFunc("GET /auth/oidc/login", s.oidc.login)
		mux.HandleFunc("GET /auth/oidc/callback", s.oidc.callback)
	}
	mux.Handle("GET /v1/me/forge-links",
		forgeLinksListHandler(s.forgeLinks, s.forgeApps, s.reviewPublicURL, s.log))
	mux.Handle("POST /v1/me/forge-links",
		forgeLinkStartHandler(s.forgeLinks, s.forgeApps, s.sealer, s.reviewPublicURL, s.log))
	mux.Handle("DELETE /v1/me/forge-links/{id}", forgeLinkDeleteHandler(s.forgeLinks, s.audits, s.log))
	mux.Handle("GET /auth/forge/callback", forgeLinkCallbackHandler(s.forgeLinks, s.forgeApps,
		s.users, s.audits, s.sealer, s.reviewClient, s.reviewPublicURL, s.log))
	if s.saml != nil {
		mux.HandleFunc("GET /auth/saml/login", s.saml.login)
		mux.HandleFunc("POST /auth/saml/acs", s.saml.acs)
		mux.HandleFunc("GET /auth/saml/metadata", s.saml.metadata)
	}
	mux.Handle("POST /v1/users", createUserHandler(s.users, s.log))
	mux.Handle("PUT /v1/users/{id}", updateUserHandler(s.users, s.log))
	mux.Handle("GET /v1/users", listUsersHandler(s.users, s.log))
	mux.Handle("DELETE /v1/users/{id}", deleteUserHandler(s.users, s.forgeLinks, s.audits, s.log))
	mux.Handle("POST /v1/credential-types", createCredTypeHandler(s.credTypes, s.log))
	mux.Handle("GET /v1/credential-types", listCredTypesHandler(s.credTypes, s.log))
	mux.Handle("GET /v1/credential-types/{id}", getCredTypeHandler(s.credTypes, s.log))
	mux.Handle("PUT /v1/credential-types/{id}", updateCredTypeHandler(s.credTypes, s.log))
	mux.Handle("DELETE /v1/credential-types/{id}", deleteCredTypeHandler(s.credTypes, s.log))
	mux.Handle("POST /v1/credentials", createCredentialHandler(s.credentials, s.credTypes, s.sealer, authz, s.log))
	mux.Handle("PUT /v1/credentials/{id}", updateCredentialHandler(s.credentials, s.credTypes, s.sealer, authz, s.log))
	// refs lets a credential or project delete refuse to orphan an object that still uses it, and
	// gives the credential list the same reading so its Used by column cannot disagree.
	refs := &refChecker{
		templates: s.templates, inventories: s.inventories,
		projects: s.projects, invSources: s.invSources, schedules: s.schedules,
		policies: s.policies,
	}
	mux.Handle("GET /v1/credentials", listCredentialsHandler(s.credentials, s.sealer, refs, authz, s.log))
	mux.Handle("DELETE /v1/credentials/{id}", deleteCredentialHandler(s.credentials, refs, s.log))
	mux.Handle("POST /v1/projects", createProjectHandler(s.projects, authz, s.log))
	mux.Handle("PUT /v1/projects/{id}", updateProjectHandler(s.projects, authz, s.log))
	mux.Handle("GET /v1/projects", listProjectsHandler(s.projects, authz, s.log))
	mux.Handle("DELETE /v1/projects/{id}", deleteProjectHandler(s.projects, refs, s.log))
	mux.Handle("GET /v1/projects/{id}/files", projectTreeHandler(s.projects, s.syncer, authz, s.log))
	mux.Handle("GET /v1/projects/{id}/file", projectFileHandler(s.projects, s.syncer, authz, s.log))
	mux.Handle("POST /v1/inventories", createInventoryHandler(s.inventories, authz, s.sealer, s.log))
	mux.Handle("PUT /v1/inventories/{id}", updateInventoryHandler(s.inventories, authz, s.sealer, s.log))
	mux.Handle("GET /v1/inventories", listInventoriesHandler(s.inventories, authz, s.log))
	mux.Handle("DELETE /v1/inventories/{id}", deleteInventoryHandler(s.inventories, refs, s.log))
	mux.Handle("POST /v1/inventories/preview",
		previewInventoryHandler(s.inventories, s.previewer, authz, s.log))
	mux.Handle("POST /v1/inventories/{id}/preview",
		previewSavedInventoryHandler(s.inventories, s.previewer, authz, s.log))
	mux.Handle("GET /v1/inventories/{id}/facts",
		listFactsHandler(s.inventories, s.factCache, s.store, authz, s.factCacheAdminOnly, s.log))
	mux.Handle("GET /v1/inventories/{id}/facts/{host}",
		hostCachedFactsHandler(s.inventories, s.factCache, s.store, authz, s.factCacheAdminOnly,
			s.log))
	mux.Handle("DELETE /v1/inventories/{id}/facts/{host}",
		clearHostFactsHandler(s.inventories, s.factCache, authz, s.log))
	mux.Handle("POST /v1/policies", createPolicyHandler(s.policies, s.log))
	mux.Handle("GET /v1/policies", listPoliciesHandler(s.policies, s.log))
	mux.Handle("PUT /v1/policies/{id}", updatePolicyHandler(s.policies, s.log))
	mux.Handle("DELETE /v1/policies/{id}", deletePolicyHandler(s.policies, s.log))
	mux.Handle("POST /v1/inventory-sources", createSourceHandler(s.invSources, s.inventories, authz, s.log))
	mux.Handle("PUT /v1/inventory-sources/{id}", updateSourceHandler(s.invSources, authz, s.log))
	mux.Handle("GET /v1/inventory-sources", listSourcesHandler(s.invSources, authz, s.log))
	mux.Handle("DELETE /v1/inventory-sources/{id}", deleteSourceHandler(s.invSources, authz, s.log))
	mux.Handle("POST /v1/inventory-sources/{id}/refresh", refreshSourceHandler(s.refresher, s.invSources, authz, s.log))
	mux.Handle("POST /v1/triggers",
		createTriggerHandler(s.triggers, s.templates, s.credentials, s.sealer, authz, s.log))
	mux.Handle("PUT /v1/triggers/{id}",
		updateTriggerHandler(s.triggers, s.templates, s.credentials, authz, s.log))
	mux.Handle("POST /v1/triggers/{id}/rotate-secret", rotateTriggerSecretHandler(s.triggers, s.sealer, authz, s.log))
	mux.Handle("GET /v1/triggers", listTriggersHandler(s.triggers, authz, s.log))
	mux.Handle("DELETE /v1/triggers/{id}", deleteTriggerHandler(s.triggers, authz, s.log))
	notifyObjects := notificationObjects{
		templates: s.templates, schedules: s.schedules, projects: s.projects, orgs: s.orgs,
	}
	mux.Handle("POST /v1/notifications",
		createNotificationHandler(s.notifications, s.sealer, authz, s.log))
	mux.Handle("GET /v1/notifications",
		listNotificationsHandler(s.notifications, s.sealer, authz, s.log))
	mux.Handle("GET /v1/notifications/{id}", getNotificationHandler(s.notifications, authz, s.log))
	mux.Handle("PUT /v1/notifications/{id}",
		updateNotificationHandler(s.notifications, s.sealer, authz, s.log))
	mux.Handle("DELETE /v1/notifications/{id}", deleteNotificationHandler(s.notifications, s.log))
	mux.Handle("GET /v1/notifications/{id}/deliveries",
		targetDeliveriesHandler(s.notifications, authz, s.log))
	mux.Handle("GET /v1/notifications/{id}/attachments",
		listAttachmentsHandler(s.notifications, authz, s.log))
	mux.Handle("POST /v1/notifications/{id}/attachments",
		attachNotificationHandler(s.notifications, notifyObjects, authz, s.log))
	mux.Handle("DELETE /v1/notifications/{id}/attachments/{attachment}",
		detachNotificationHandler(s.notifications, authz, s.log))
	mux.Handle("POST /hooks/{token}", hookHandler(s.triggers, s.templates, s.submitter, s.store,
		s.sealer, s.audits, s.reviews, s.hooks, s.commentCommands(authz), s.log))
	mux.Handle("POST /v1/templates", createTemplateHandler(s.templates, s.sealer, authz, s.log))
	mux.Handle("PUT /v1/templates/{id}", updateTemplateHandler(s.templates, s.sealer, authz, s.log))
	mux.Handle("GET /v1/templates", listTemplatesHandler(s.templates, authz, s.log))
	mux.Handle("DELETE /v1/templates/{id}", deleteTemplateHandler(s.templates, refs, s.log))
	mux.Handle("POST /v1/templates/{id}/launch",
		launchTemplateHandler(s.templates, s.submitter, s.sealer, authz, s.log))
	mux.Handle("POST /v1/templates/{id}/callback-key",
		mintCallbackKeyHandler(s.templates, s.sealer, authz, s.log))
	mux.Handle("DELETE /v1/templates/{id}/callback-key",
		revokeCallbackKeyHandler(s.templates, authz, s.log))
	// Served without an account: the host config key in the body is the credential, the way a
	// webhook's path token is. The AWX-compatible address shares the native one's budgets.
	callbacks := s.newCallbacks()
	mux.Handle("POST /v1/templates/{id}/callback", callbacks.native())
	callbacks.registerAWX(mux)
	mux.Handle("POST /v1/teams", createTeamHandler(s.teams, s.log))
	mux.Handle("GET /v1/teams", listTeamsHandler(s.teams, s.log))
	mux.Handle("DELETE /v1/teams/{id}", deleteTeamHandler(s.teams, s.log))
	mux.Handle("GET /v1/teams/{id}/members", listTeamMembersHandler(s.teams, s.log))
	mux.Handle("POST /v1/teams/{id}/members", addTeamMemberHandler(s.teams, s.log))
	mux.Handle("DELETE /v1/teams/{id}/members/{userID}", removeTeamMemberHandler(s.teams, s.log))
	mux.Handle("POST /v1/orgs", createOrgHandler(s.orgs, s.log))
	mux.Handle("GET /v1/orgs", listOrgsHandler(s.orgs, s.log))
	mux.Handle("DELETE /v1/orgs/{id}", deleteOrgHandler(s.orgs, refs, s.log))
	mux.Handle("GET /v1/orgs/{id}/members", listOrgMembersHandler(s.orgs, s.log))
	mux.Handle("POST /v1/orgs/{id}/members", addOrgMemberHandler(s.orgs, s.log))
	mux.Handle("DELETE /v1/orgs/{id}/members/{userID}", removeOrgMemberHandler(s.orgs, s.log))
	mux.Handle("POST /v1/grants", createGrantHandler(s.grants, s.log))
	mux.Handle("GET /v1/grants", listGrantsHandler(s.grants, s.log))
	mux.Handle("DELETE /v1/grants/{id}", deleteGrantHandler(s.grants, s.log))
	mux.Handle("POST /v1/import/{format}", importHandler(func() (importer.ApplyStores, bool) {
		if s.projects == nil || s.inventories == nil || s.credentials == nil ||
			s.templates == nil || s.schedules == nil {
			return importer.ApplyStores{}, false
		}
		return importer.ApplyStores{
			Projects: s.projects, Inventories: s.inventories, Sources: s.invSources,
			Credentials: s.credentials, CredentialTypes: s.credTypes, Templates: s.templates,
			Schedules: s.schedules, Notifications: s.notifications, Orgs: s.orgs, Sealer: s.sealer,
		}, true
	}, s.log))
	// Every refusal this API makes is a JSON object with an "error" string, except the two the mux
	// writes itself: an unrouted path and a wrong method came back as Go's plain-text "404 page not
	// found" and "Method Not Allowed". A client that parses errors, which is every client, then met
	// two responses it could not read, on the two mistakes a caller is most likely to make while
	// learning the API. The shim below gives them the same shape as everything else.
	handler := jsonNotFound(mux)
	// Compression sits under the gate, so a refusal is written by the gate itself and only a
	// response the handlers produced is ever encoded.
	handler = compress(handler)
	if s.tokens != nil {
		gate := &authGate{tokens: s.tokens, users: s.users, jwt: s.jwt, audits: s.audits, log: s.log,
			authz: authz, alwaysEnforce: s.enforceAuth, tickets: tickets, publicReads: s.readOnly,
			cleanups: attachmentCleanups(s.notifications), secretVars: templateSecretVars(s.templates)}
		handler = gate.wrap(handler)
	}
	if s.readOnly {
		handler = readOnlyGate(handler)
	}
	if s.relayStore != nil && (s.workerToken != "" || s.workerPools != nil) {
		pools := s.workerPools
		if pools == nil {
			pools = relay.SinglePool(s.workerToken)
		}
		var opts []relay.HandlerOption
		if s.announcer != nil {
			opts = append(opts, relay.WithAnnouncer(s.announcer))
		}
		if s.secretOpener != nil {
			opts = append(opts, relay.WithSecretOpener(s.secretOpener))
		}
		if s.planSealer != nil {
			opts = append(opts, relay.WithPlanSealer(s.planSealer))
		}
		if s.attention != nil && s.attention.Presence != nil {
			opts = append(opts, relay.WithPresence(s.attention.Presence))
		}
		handler = relayGate(relay.NewHandler(s.relayStore, pools, s.log, s.policies, s.audits,
			opts...), handler)
	}
	return securityHeaders(requestTextGuard(s.log, bodyLimit(handler)))
}

// orgResolver returns an OrgResolver that reads a grantable object's owning organization from the
// store that owns its kind, so the authorizer can extend access to that organization's members. A
// missing object, an unconfigured store, or a store error resolves to not-found, so org ownership
// only ever adds access.
func (s *Server) orgResolver() OrgResolver {
	return OrgResolverFunc(func(ctx context.Context, objectID string) (string, bool) {
		orgID, found, err := s.resolveObjectOrg(ctx, objectID)
		if err != nil {
			s.log.Error("server: resolve object org: " + err.Error())
			return "", false
		}
		return orgID, found
	})
}

// resolveObjectOrg looks up the owning organization of a grantable object, dispatching on its id
// prefix to the store that owns the kind. found is false when the id is not a grantable object, its
// store is not configured, or no such object exists. A nil error with found true carries the owner,
// which is empty for an unowned object.
func (s *Server) resolveObjectOrg(ctx context.Context, objectID string) (orgID string, found bool, err error) {
	switch {
	case strings.HasPrefix(objectID, "proj_"):
		if s.projects == nil {
			return "", false, nil
		}
		p, err := s.projects.Get(ctx, objectID)
		if errors.Is(err, project.ErrNotFound) {
			return "", false, nil
		}
		if err != nil {
			return "", false, err
		}
		return p.OrgID, true, nil
	case strings.HasPrefix(objectID, "tpl_"):
		if s.templates == nil {
			return "", false, nil
		}
		t, err := s.templates.Get(ctx, objectID)
		if errors.Is(err, template.ErrNotFound) {
			return "", false, nil
		}
		if err != nil {
			return "", false, err
		}
		return t.OrgID, true, nil
	case strings.HasPrefix(objectID, "inv_"):
		if s.inventories == nil {
			return "", false, nil
		}
		i, err := s.inventories.Get(ctx, objectID)
		if errors.Is(err, inventory.ErrNotFound) {
			return "", false, nil
		}
		if err != nil {
			return "", false, err
		}
		return i.OrgID, true, nil
	case strings.HasPrefix(objectID, "cred_"):
		if s.credentials == nil {
			return "", false, nil
		}
		c, err := s.credentials.Get(ctx, objectID)
		if errors.Is(err, credential.ErrNotFound) {
			return "", false, nil
		}
		if err != nil {
			return "", false, err
		}
		return c.OrgID, true, nil
	case strings.HasPrefix(objectID, "ntf_"):
		if s.notifications == nil {
			return "", false, nil
		}
		n, err := s.notifications.Get(ctx, objectID)
		if errors.Is(err, notification.ErrNotFound) {
			return "", false, nil
		}
		if err != nil {
			return "", false, err
		}
		return n.OrgID, true, nil
	default:
		return "", false, nil
	}
}

// relayGate routes the mesh relay worker endpoints to their own handler, which authenticates with
// the worker token, and passes everything else to next. It sits outside the API token gate and the
// read-only gate so a worker presenting its worker token is never checked against the API tokens,
// mirroring how webhook triggers carry their own secret in the path.
func relayGate(relayHandler, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/relay/") {
			relayHandler.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// readOnlyGate rejects every request that would change state, so a read-only server cannot be
// mutated. Reads, the UI, and signing in and out pass through.
func readOnlyGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
		default:
			// Signing in changes nothing the install governs. Refusing it locked every account out of
			// a read-only install, so the people it was exposed for could not read what their role
			// allows, and the refusal told them the install was a demo.
			if isSignIn(r) {
				next.ServeHTTP(w, r)
				return
			}
			// An import preview is a read written as a POST, because an export is a body rather than
			// a query. Refusing it on the method alone turned the migration page into a wall in the
			// demo, which is where the most expensive question a visitor brings gets answered: does
			// my export come across. The preview parses the uploaded bytes and touches no store, and
			// the write lives behind apply, so that parameter is the whole line between the two.
			if r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/import/") &&
				!applyRequested(r) {
				next.ServeHTTP(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":"this server is read-only"}`))
		}
	})
}
