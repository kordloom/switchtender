// Package server exposes the Yardmaster HTTP API over the run store and dispatcher.
package server

import (
	"context"
	"io/fs"
	"net/http"

	"go.uber.org/zap"

	"github.com/dcadolph/yardmaster/internal/ai"
	"github.com/dcadolph/yardmaster/internal/audit"
	"github.com/dcadolph/yardmaster/internal/auth"
	"github.com/dcadolph/yardmaster/internal/credential"
	"github.com/dcadolph/yardmaster/internal/grant"
	"github.com/dcadolph/yardmaster/internal/importer"
	"github.com/dcadolph/yardmaster/internal/inventory"
	"github.com/dcadolph/yardmaster/internal/invsource"
	"github.com/dcadolph/yardmaster/internal/live"
	"github.com/dcadolph/yardmaster/internal/policy"
	"github.com/dcadolph/yardmaster/internal/project"
	"github.com/dcadolph/yardmaster/internal/run"
	"github.com/dcadolph/yardmaster/internal/schedule"
	"github.com/dcadolph/yardmaster/internal/team"
	"github.com/dcadolph/yardmaster/internal/template"
	"github.com/dcadolph/yardmaster/internal/trigger"
	"github.com/dcadolph/yardmaster/internal/ui"
	"github.com/dcadolph/yardmaster/internal/user"
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
}

// Approver releases or denies a run held for approval. The dispatcher satisfies it.
type Approver interface {
	Approve(ctx context.Context, id string) (*run.Run, error)
	Reject(ctx context.Context, id, reason string) (*run.Run, error)
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

// WithAuditSigner signs audit exports with the given signer so they can be verified offline.
func WithAuditSigner(signer *audit.Signer) Option {
	return func(srv *Server) { srv.auditSigner = signer }
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

// WithJWT enables bearer JWT authentication. When set, a request whose bearer token is a JWT is
// validated against the issuer's keys instead of the token store, so a service can present a JWT
// minted elsewhere.
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
	// sealer encrypts credential secrets.
	sealer *credential.Sealer
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
	// auditSigner signs audit exports when configured, nil when export signing is off.
	auditSigner *audit.Signer
	// invSources backs the dynamic inventory source endpoints when configured.
	invSources invsource.Store
	// refresher refreshes inventory sources when configured.
	refresher SourceRefresher
	// triggers backs webhook triggers when configured.
	triggers trigger.Store
	// teams backs the team endpoints when configured.
	teams team.Store
	// grants backs per-object access grants when configured.
	grants grant.Store
	// strictGrants makes an object with no grants deny non-admins.
	strictGrants bool
	// docs is the documentation tree rendered inside the UI, nil when not wired.
	docs fs.FS
	// readOnly rejects mutating requests when set, for a public demo.
	readOnly bool
	// matrixCap is the largest host matrix, in cells, the UI draws. Zero or less means no limit.
	matrixCap int
	// oidc enables single sign-on when configured, nil when SSO is off.
	oidc *OIDCAuth
	// ldap enables directory sign-in when configured, nil when LDAP is off.
	ldap *LDAPAuth
	// jwt validates a bearer JWT when configured, nil when JWT sign-in is off.
	jwt *JWTAuth
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
	srv.web = ui.New(srv.log, srv.docs, srv.readOnly, srv.matrixCap, srv.oidc != nil)
	return srv
}

// Handler returns the HTTP handler serving the Yardmaster API and web interface.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	authz := &authorizer{grants: s.grants, teams: s.teams, strict: s.strictGrants}
	mux.Handle("GET /healthz", healthHandler())
	mux.Handle("GET /metrics", metricsHandler(s.store, s.log))
	mux.Handle("GET /v1/fleet", fleetHandler(s.store, s.log))
	mux.Handle("GET /v1/drift", driftHandler(s.store, s.log))
	mux.Handle("POST /v1/drift/reconcile", reconcileDriftHandler(s.store, s.submitter, authz, s.log))
	mux.Handle("GET /v1/hosts/{host}/runs", hostHistoryHandler(s.store, s.log))
	mux.Handle("GET /v1/tasks", taskTrendsHandler(s.store, s.log))
	mux.Handle("GET /v1/workers", workersHandler(s.store, s.log))
	mux.Handle("GET /v1/audit", auditHandler(s.audits, s.log))
	mux.Handle("GET /v1/audit/verify", auditVerifyHandler(s.audits, s.log))
	mux.Handle("GET /v1/audit/export", auditExportHandler(s.audits, s.auditSigner, s.log))
	mux.Handle("POST /v1/runs", createRunHandler(s.submitter, authz, s.log))
	mux.Handle("POST /v1/pipelines", createPipelineHandler(s.submitter, authz, s.log))
	mux.Handle("POST /v1/runs/{id}/cancel", cancelRunHandler(s.store, s.canceler, authz, s.log))
	mux.Handle("POST /v1/runs/{id}/retry", retryRunHandler(s.store, s.retrier, authz, s.log))
	mux.Handle("POST /v1/runs/{id}/approve", approveRunHandler(s.approver, s.log))
	mux.Handle("POST /v1/runs/{id}/reject", rejectRunHandler(s.approver, s.log))
	mux.Handle("GET /v1/runs", listRunsHandler(s.store, s.log))
	mux.Handle("GET /v1/runs/{id}", getRunHandler(s.store, authz, s.log))
	mux.Handle("GET /v1/runs/{id}/shards", runShardsHandler(s.store, authz, s.log))
	mux.Handle("GET /v1/runs/{id}/steps", runStepsHandler(s.store, authz, s.log))
	mux.Handle("GET /v1/runs/{id}/logs", runLogsHandler(s.store, authz, s.log))
	mux.Handle("GET /v1/runs/{id}/events", runEventsHandler(s.store, authz, s.log))
	mux.Handle("POST /v1/runs/{id}/explain", explainRunHandler(s.store, s.ai, authz, s.log))
	mux.Handle("POST /v1/ai/draft", draftStepHandler(s.ai, s.log))
	mux.Handle("POST /v1/ai/ask", askFleetHandler(s.store, s.ai, s.log))
	mux.Handle("POST /v1/ai/propose-run", proposeRunHandler(s.submitter, s.ai, s.log))
	mux.Handle("GET /v1/runs/{id}/stream", runStreamHandler(s.streamer, s.store, authz, s.log))
	mux.Handle("POST /v1/schedules", createScheduleHandler(s.schedules, s.log))
	mux.Handle("GET /v1/schedules", listSchedulesHandler(s.schedules, s.log))
	mux.Handle("GET /v1/schedules/{id}", getScheduleHandler(s.schedules, s.log))
	mux.Handle("PUT /v1/schedules/{id}", updateScheduleHandler(s.schedules, s.log))
	mux.Handle("DELETE /v1/schedules/{id}", deleteScheduleHandler(s.schedules, s.log))
	mux.Handle("/ui/", s.web.Handler())
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/ui/", http.StatusFound)
	})
	mux.Handle("POST /v1/auth/check", authCheckHandler())
	mux.Handle("POST /v1/auth/login", loginHandler(s.users, s.tokens, s.ldap, s.log))
	if s.oidc != nil {
		mux.HandleFunc("GET /auth/oidc/login", s.oidc.login)
		mux.HandleFunc("GET /auth/oidc/callback", s.oidc.callback)
	}
	mux.Handle("POST /v1/users", createUserHandler(s.users, s.log))
	mux.Handle("PUT /v1/users/{id}", updateUserHandler(s.users, s.log))
	mux.Handle("GET /v1/users", listUsersHandler(s.users, s.log))
	mux.Handle("DELETE /v1/users/{id}", deleteUserHandler(s.users, s.log))
	mux.Handle("POST /v1/credentials", createCredentialHandler(s.credentials, s.sealer, s.log))
	mux.Handle("PUT /v1/credentials/{id}", updateCredentialHandler(s.credentials, s.sealer, s.log))
	mux.Handle("GET /v1/credentials", listCredentialsHandler(s.credentials, s.log))
	// refs lets a credential or project delete refuse to orphan an object that still uses it.
	refs := &refChecker{
		templates: s.templates, inventories: s.inventories,
		projects: s.projects, invSources: s.invSources,
	}
	mux.Handle("DELETE /v1/credentials/{id}", deleteCredentialHandler(s.credentials, refs, s.log))
	mux.Handle("POST /v1/projects", createProjectHandler(s.projects, s.log))
	mux.Handle("PUT /v1/projects/{id}", updateProjectHandler(s.projects, s.log))
	mux.Handle("GET /v1/projects", listProjectsHandler(s.projects, s.log))
	mux.Handle("DELETE /v1/projects/{id}", deleteProjectHandler(s.projects, refs, s.log))
	mux.Handle("POST /v1/inventories", createInventoryHandler(s.inventories, authz, s.sealer, s.log))
	mux.Handle("PUT /v1/inventories/{id}", updateInventoryHandler(s.inventories, authz, s.sealer, s.log))
	mux.Handle("GET /v1/inventories", listInventoriesHandler(s.inventories, s.log))
	mux.Handle("DELETE /v1/inventories/{id}", deleteInventoryHandler(s.inventories, s.log))
	mux.Handle("POST /v1/policies", createPolicyHandler(s.policies, s.log))
	mux.Handle("GET /v1/policies", listPoliciesHandler(s.policies, s.log))
	mux.Handle("PUT /v1/policies/{id}", updatePolicyHandler(s.policies, s.log))
	mux.Handle("DELETE /v1/policies/{id}", deletePolicyHandler(s.policies, s.log))
	mux.Handle("POST /v1/inventory-sources", createSourceHandler(s.invSources, s.inventories, authz, s.log))
	mux.Handle("PUT /v1/inventory-sources/{id}", updateSourceHandler(s.invSources, authz, s.log))
	mux.Handle("GET /v1/inventory-sources", listSourcesHandler(s.invSources, s.log))
	mux.Handle("DELETE /v1/inventory-sources/{id}", deleteSourceHandler(s.invSources, s.log))
	mux.Handle("POST /v1/inventory-sources/{id}/refresh", refreshSourceHandler(s.refresher, s.log))
	mux.Handle("POST /v1/triggers", createTriggerHandler(s.triggers, s.templates, s.sealer, s.log))
	mux.Handle("PUT /v1/triggers/{id}", updateTriggerHandler(s.triggers, s.log))
	mux.Handle("POST /v1/triggers/{id}/rotate-secret", rotateTriggerSecretHandler(s.triggers, s.sealer, s.log))
	mux.Handle("GET /v1/triggers", listTriggersHandler(s.triggers, s.log))
	mux.Handle("DELETE /v1/triggers/{id}", deleteTriggerHandler(s.triggers, s.log))
	mux.Handle("POST /hooks/{token}", hookHandler(s.triggers, s.templates, s.submitter, s.sealer, s.log))
	mux.Handle("POST /v1/templates", createTemplateHandler(s.templates, s.log))
	mux.Handle("PUT /v1/templates/{id}", updateTemplateHandler(s.templates, s.log))
	mux.Handle("GET /v1/templates", listTemplatesHandler(s.templates, s.log))
	mux.Handle("DELETE /v1/templates/{id}", deleteTemplateHandler(s.templates, s.log))
	mux.Handle("POST /v1/templates/{id}/launch", launchTemplateHandler(s.templates, s.submitter, authz, s.log))
	mux.Handle("POST /v1/teams", createTeamHandler(s.teams, s.log))
	mux.Handle("GET /v1/teams", listTeamsHandler(s.teams, s.log))
	mux.Handle("DELETE /v1/teams/{id}", deleteTeamHandler(s.teams, s.log))
	mux.Handle("GET /v1/teams/{id}/members", listTeamMembersHandler(s.teams, s.log))
	mux.Handle("POST /v1/teams/{id}/members", addTeamMemberHandler(s.teams, s.log))
	mux.Handle("DELETE /v1/teams/{id}/members/{userID}", removeTeamMemberHandler(s.teams, s.log))
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
	var handler http.Handler = mux
	if s.tokens != nil {
		gate := &authGate{tokens: s.tokens, users: s.users, jwt: s.jwt, audits: s.audits, log: s.log}
		handler = gate.wrap(mux)
	}
	if s.readOnly {
		handler = readOnlyGate(handler)
	}
	return handler
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
