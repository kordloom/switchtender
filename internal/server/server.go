// Package server exposes the SwitchTender HTTP API over the run store and dispatcher.
package server

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"strings"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/beatfeed"
	"github.com/kordloom/switchtender/internal/ai"
	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/auth"
	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/grant"
	"github.com/kordloom/switchtender/internal/importer"
	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/invsource"
	"github.com/kordloom/switchtender/internal/live"
	"github.com/kordloom/switchtender/internal/org"
	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/project"
	"github.com/kordloom/switchtender/internal/relay"
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
	Cancel(id string) bool
}

// Retrier starts a new split run from the failed shards of a finished one. The dispatcher
// satisfies it.
type Retrier interface {
	RetryFailedShards(ctx context.Context, parentID string) (*run.Run, error)
	RelaunchFailedHosts(ctx context.Context, runID, actor, actorType string) (*run.Run, error)
}

// Approver releases or denies a run held for approval, naming the deciding actor so the decision
// entry on the chain carries who decided. The dispatcher satisfies it.
type Approver interface {
	Approve(ctx context.Context, id, by, byType string) (*run.Run, error)
	Reject(ctx context.Context, id, reason, by, byType string) (*run.Run, error)
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

// WithRetrier enables the failed shard retry endpoint backed by r.
func WithRetrier(r Retrier) Option {
	return func(srv *Server) { srv.retrier = r }
}

// WithApprover enables the run approval endpoints backed by a.
func WithApprover(a Approver) Option {
	return func(srv *Server) { srv.approver = a }
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

// WithShutdown gives the server the context that is canceled when the process begins draining. A
// live event stream ends on it instead of holding the graceful shutdown open for its whole timeout,
// since a stream only finishes when its run does. Without it, shutdown waits out every open stream.
func WithShutdown(ctx context.Context) Option {
	return func(srv *Server) { srv.shutdown = ctx }
}

// WithRelay mounts the phase-1 mesh relay worker endpoints, backed by the given run store and
// guarded by the worker token. A relay worker in an isolated segment dials them over one outbound
// connection to lease and execute runs without a path to the database. An empty token leaves the
// endpoints off, so the mesh is opt-in.
// WithWorkerPools confines each worker token to the queues it may lease from.
//
// A queue routes work to the segment that can reach it, so the queues a token may claim are that
// token's blast radius. One shared token made the least trusted worker in the estate a path to the
// most trusted queue. Set this and a token that serves the DMZ cannot lease a production run.
func WithWorkerPools(pools *relay.Pools) Option {
	return func(srv *Server) { srv.workerPools = pools }
}

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
	// retrier backs the failed shard retry endpoint when configured.
	retrier Retrier
	// approver backs the run approval endpoints when configured.
	approver Approver
	// schedules backs the schedule endpoints when configured.
	schedules schedule.Store
	// tokens backs API authentication when configured.
	tokens auth.Store
	// credentials backs the credential endpoints when configured.
	credentials credential.Store
	// credTypes backs the custom credential type endpoints when configured.
	credTypes credential.TypeStore
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
	// productVersion stamps the trust document.
	productVersion string
	// invSources backs the dynamic inventory source endpoints when configured.
	invSources invsource.Store
	// refresher refreshes inventory sources when configured.
	refresher SourceRefresher
	// triggers backs webhook triggers when configured.
	triggers trigger.Store
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
	// readOnly rejects mutating requests when set, for a public demo.
	readOnly bool
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
	srv := &Server{store: store, submitter: submitter, log: log}
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
	srv.web = ui.New(srv.log, srv.docs, srv.readOnly, srv.matrixCap, srv.oidc != nil, srv.saml != nil, srv.ai != nil)
	return srv
}

// Handler returns the HTTP handler serving the SwitchTender API and web interface.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	authz := &authorizer{
		grants: s.grants, teams: s.teams, orgs: s.orgs,
		orgOwners: s.orgResolver(), strict: s.strictGrants,
	}
	mux.Handle("GET /healthz", healthHandler())
	mux.Handle("GET /readyz", readyHandler(s.store))
	// The producer identity's install id binds the tree profile's leaves, so every anchor check
	// that may meet a tree anchor carries it. Without a producer it stays empty and tree anchors
	// are reported uncheckable rather than silently passed.
	var installID string
	if s.producer != nil {
		installID = s.producer.InstallID
	}
	var health *chainHealth
	if s.audits != nil {
		health = newChainHealth(s.audits, installID)
	}
	mux.Handle("GET /metrics", metricsHandler(s.store, health, s.log))
	mux.Handle("GET /v1/fleet", fleetHandler(s.store, authz, s.log))
	mux.Handle("GET /v1/drift", driftHandler(s.store, authz, s.log))
	mux.Handle("POST /v1/drift/reconcile", reconcileDriftHandler(s.store, s.submitter, authz, s.log))
	mux.Handle("GET /v1/hosts/{host}/runs", hostHistoryHandler(s.store, authz, s.log))
	mux.Handle("GET /v1/hosts/{host}/facts", hostFactsHandler(s.store, authz, s.log))
	mux.Handle("GET /v1/tasks", taskTrendsHandler(s.store, authz, s.log))
	mux.Handle("GET /v1/workers", workersHandler(s.store, authz, s.log))
	mux.Handle("GET /v1/audit", auditHandler(s.audits, s.log))
	mux.Handle("GET /v1/audit/verify", auditVerifyHandler(s.audits, installID, s.log))
	mux.Handle("GET /v1/audit/bundle", auditBundleHandler(s.audits, s.producer, s.productVersion, s.log))
	mux.Handle("GET /v1/audit/register", auditRegisterHandler(s.store, s.audits, installID, s.log))
	// Served unauthenticated: the beat feed exists so an outside watcher can see the chain is
	// alive and whole, and that watcher has no account here.
	mux.Handle("GET "+beatfeed.APIPath, auditBeatsHandler(s.audits, s.log))
	// Served unversioned and unauthenticated: a relying party checking a bundle has no account here,
	// and the document holds only the public half of the signing key.
	mux.Handle("GET /.well-known/loomseal.json", trustHandler(s.producer, s.productVersion, s.log))
	mux.Handle("POST /v1/runs", createRunHandler(s.submitter, authz, s.log))
	mux.Handle("POST /v1/pipelines", createPipelineHandler(s.submitter, authz, s.log))
	mux.Handle("POST /v1/runs/{id}/cancel", cancelRunHandler(s.store, s.canceler, authz, s.log))
	mux.Handle("POST /v1/runs/{id}/retry", retryRunHandler(s.store, s.retrier, authz, s.log))
	mux.Handle("POST /v1/runs/{id}/relaunch-failed", relaunchFailedHandler(s.store, s.retrier, authz, s.log))
	mux.Handle("POST /v1/runs/{id}/rerun", rerunRunHandler(s.store, s.submitter, authz, s.log))
	mux.Handle("POST /v1/runs/{id}/approve", approveRunHandler(s.approver, s.store, authz, s.log))
	mux.Handle("POST /v1/runs/{id}/reject", rejectRunHandler(s.approver, s.store, authz, s.log))
	mux.Handle("GET /v1/runs", listRunsHandler(s.store, authz, s.log))
	mux.Handle("GET /v1/runs/{id}", getRunHandler(s.store, authz, s.log))
	mux.Handle("GET /v1/runs/{id}/compare", runCompareHandler(s.store, authz, s.log))
	mux.Handle("GET /v1/runs/{id}/shards", runShardsHandler(s.store, authz, s.log))
	mux.Handle("GET /v1/runs/{id}/steps", runStepsHandler(s.store, authz, s.log))
	mux.Handle("GET /v1/runs/{id}/logs", runLogsHandler(s.store, authz, s.log))
	mux.Handle("GET /v1/runs/{id}/events", runEventsHandler(s.store, authz, s.log))
	mux.Handle("GET /v1/runs/{id}/evidence",
		runEvidenceHandler(s.store, s.audits, installID, authz, s.log))
	mux.Handle("GET /v1/runs/{id}/receipt",
		runReceiptHandler(s.store, s.audits, s.producer, s.productVersion, authz, s.log))
	mux.Handle("POST /v1/runs/{id}/explain", explainRunHandler(s.store, s.ai, authz, s.log))
	mux.Handle("POST /v1/ai/draft", draftStepHandler(s.ai, s.log))
	mux.Handle("POST /v1/ai/ask", askFleetHandler(s.store, s.ai, authz, s.log))
	mux.Handle("POST /v1/ai/propose-run", proposeRunHandler(s.submitter, s.ai, s.log))
	mux.Handle("GET /v1/runs/{id}/stream",
		runStreamHandler(s.streamer, s.store, authz, s.log, s.shutdown))
	mux.Handle("GET /v1/schedules/preview", previewScheduleHandler(s.log))
	mux.Handle("GET /v1/doctor", doctorHandler(s.templates, s.schedules, s.credentials, s.inventories, s.projects, s.log))
	mux.Handle("POST /v1/schedules", createScheduleHandler(s.schedules, authz, s.log))
	mux.Handle("GET /v1/schedules", listSchedulesHandler(s.schedules, authz, s.log))
	mux.Handle("GET /v1/schedules/{id}", getScheduleHandler(s.schedules, authz, s.log))
	mux.Handle("PUT /v1/schedules/{id}", updateScheduleHandler(s.schedules, authz, s.log))
	mux.Handle("DELETE /v1/schedules/{id}", deleteScheduleHandler(s.schedules, authz, s.log))
	mux.Handle("/ui/", s.web.Handler())
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/ui/", http.StatusFound)
	})
	mux.Handle("POST /v1/auth/check", authCheckHandler())
	mux.Handle("GET /v1/auth/me", authMeHandler(s.log))
	mux.Handle("POST /v1/auth/login", loginHandler(s.users, s.tokens, s.ldap, s.log))
	if s.oidc != nil {
		mux.HandleFunc("GET /auth/oidc/login", s.oidc.login)
		mux.HandleFunc("GET /auth/oidc/callback", s.oidc.callback)
	}
	if s.saml != nil {
		mux.HandleFunc("GET /auth/saml/login", s.saml.login)
		mux.HandleFunc("POST /auth/saml/acs", s.saml.acs)
		mux.HandleFunc("GET /auth/saml/metadata", s.saml.metadata)
	}
	mux.Handle("POST /v1/users", createUserHandler(s.users, s.log))
	mux.Handle("PUT /v1/users/{id}", updateUserHandler(s.users, s.log))
	mux.Handle("GET /v1/users", listUsersHandler(s.users, s.log))
	mux.Handle("DELETE /v1/users/{id}", deleteUserHandler(s.users, s.log))
	mux.Handle("POST /v1/credential-types", createCredTypeHandler(s.credTypes, s.log))
	mux.Handle("GET /v1/credential-types", listCredTypesHandler(s.credTypes, s.log))
	mux.Handle("GET /v1/credential-types/{id}", getCredTypeHandler(s.credTypes, s.log))
	mux.Handle("PUT /v1/credential-types/{id}", updateCredTypeHandler(s.credTypes, s.log))
	mux.Handle("DELETE /v1/credential-types/{id}", deleteCredTypeHandler(s.credTypes, s.log))
	mux.Handle("POST /v1/credentials", createCredentialHandler(s.credentials, s.credTypes, s.sealer, authz, s.log))
	mux.Handle("PUT /v1/credentials/{id}", updateCredentialHandler(s.credentials, s.sealer, authz, s.log))
	mux.Handle("GET /v1/credentials", listCredentialsHandler(s.credentials, authz, s.log))
	// refs lets a credential or project delete refuse to orphan an object that still uses it.
	refs := &refChecker{
		templates: s.templates, inventories: s.inventories,
		projects: s.projects, invSources: s.invSources,
	}
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
	mux.Handle("DELETE /v1/inventories/{id}", deleteInventoryHandler(s.inventories, s.log))
	mux.Handle("POST /v1/policies", createPolicyHandler(s.policies, s.log))
	mux.Handle("GET /v1/policies", listPoliciesHandler(s.policies, s.log))
	mux.Handle("PUT /v1/policies/{id}", updatePolicyHandler(s.policies, s.log))
	mux.Handle("DELETE /v1/policies/{id}", deletePolicyHandler(s.policies, s.log))
	mux.Handle("POST /v1/inventory-sources", createSourceHandler(s.invSources, s.inventories, authz, s.log))
	mux.Handle("PUT /v1/inventory-sources/{id}", updateSourceHandler(s.invSources, authz, s.log))
	mux.Handle("GET /v1/inventory-sources", listSourcesHandler(s.invSources, authz, s.log))
	mux.Handle("DELETE /v1/inventory-sources/{id}", deleteSourceHandler(s.invSources, authz, s.log))
	mux.Handle("POST /v1/inventory-sources/{id}/refresh", refreshSourceHandler(s.refresher, s.invSources, authz, s.log))
	mux.Handle("POST /v1/triggers", createTriggerHandler(s.triggers, s.templates, s.sealer, authz, s.log))
	mux.Handle("PUT /v1/triggers/{id}", updateTriggerHandler(s.triggers, s.templates, authz, s.log))
	mux.Handle("POST /v1/triggers/{id}/rotate-secret", rotateTriggerSecretHandler(s.triggers, s.sealer, authz, s.log))
	mux.Handle("GET /v1/triggers", listTriggersHandler(s.triggers, authz, s.log))
	mux.Handle("DELETE /v1/triggers/{id}", deleteTriggerHandler(s.triggers, authz, s.log))
	mux.Handle("POST /hooks/{token}", hookHandler(s.triggers, s.templates, s.submitter, s.store, s.sealer, s.audits, s.log))
	mux.Handle("POST /v1/templates", createTemplateHandler(s.templates, authz, s.log))
	mux.Handle("PUT /v1/templates/{id}", updateTemplateHandler(s.templates, authz, s.log))
	mux.Handle("GET /v1/templates", listTemplatesHandler(s.templates, authz, s.log))
	mux.Handle("DELETE /v1/templates/{id}", deleteTemplateHandler(s.templates, s.log))
	mux.Handle("POST /v1/templates/{id}/launch", launchTemplateHandler(s.templates, s.submitter, authz, s.log))
	mux.Handle("POST /v1/teams", createTeamHandler(s.teams, s.log))
	mux.Handle("GET /v1/teams", listTeamsHandler(s.teams, s.log))
	mux.Handle("DELETE /v1/teams/{id}", deleteTeamHandler(s.teams, s.log))
	mux.Handle("GET /v1/teams/{id}/members", listTeamMembersHandler(s.teams, s.log))
	mux.Handle("POST /v1/teams/{id}/members", addTeamMemberHandler(s.teams, s.log))
	mux.Handle("DELETE /v1/teams/{id}/members/{userID}", removeTeamMemberHandler(s.teams, s.log))
	mux.Handle("POST /v1/orgs", createOrgHandler(s.orgs, s.log))
	mux.Handle("GET /v1/orgs", listOrgsHandler(s.orgs, s.log))
	mux.Handle("DELETE /v1/orgs/{id}", deleteOrgHandler(s.orgs, s.log))
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
			Credentials: s.credentials, Templates: s.templates, Schedules: s.schedules,
		}, true
	}, s.log))
	// Compression sits under the gate, so a refusal is written by the gate itself and only a
	// response the handlers produced is ever encoded.
	handler := compress(mux)
	if s.tokens != nil {
		gate := &authGate{tokens: s.tokens, users: s.users, jwt: s.jwt, audits: s.audits, log: s.log,
			authz: authz, alwaysEnforce: s.enforceAuth}
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
		handler = relayGate(relay.NewHandler(s.relayStore, pools, s.log, s.policies, s.audits), handler)
	}
	return securityHeaders(bodyLimit(handler))
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

// readOnlyGate rejects every request that would change state, so a demo cannot be mutated. Reads
// and the UI pass through.
func readOnlyGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
		default:
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":"this is a read-only demo"}`))
		}
	})
}
