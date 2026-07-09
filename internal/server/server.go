// Package server exposes the Yardmaster HTTP API over the run store and dispatcher.
package server

import (
	"context"
	"io/fs"
	"net/http"

	"go.uber.org/zap"

	"github.com/dcadolph/yardmaster/internal/audit"
	"github.com/dcadolph/yardmaster/internal/auth"
	"github.com/dcadolph/yardmaster/internal/credential"
	"github.com/dcadolph/yardmaster/internal/grant"
	"github.com/dcadolph/yardmaster/internal/importer"
	"github.com/dcadolph/yardmaster/internal/inventory"
	"github.com/dcadolph/yardmaster/internal/invsource"
	"github.com/dcadolph/yardmaster/internal/live"
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

// WithInventories enables the inventory endpoints backed by the given store.
func WithInventories(store inventory.Store) Option {
	return func(srv *Server) { srv.inventories = store }
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
	// audits backs the audit trail when configured.
	audits audit.Store
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
	mux.Handle("GET /fleet", fleetHandler(s.store, s.log))
	mux.Handle("GET /hosts/{host}/runs", hostHistoryHandler(s.store, s.log))
	mux.Handle("GET /tasks", taskTrendsHandler(s.store, s.log))
	mux.Handle("GET /workers", workersHandler(s.store, s.log))
	mux.Handle("GET /audit", auditHandler(s.audits, s.log))
	mux.Handle("POST /runs", createRunHandler(s.submitter, authz, s.log))
	mux.Handle("POST /pipelines", createPipelineHandler(s.submitter, authz, s.log))
	mux.Handle("POST /runs/{id}/cancel", cancelRunHandler(s.store, s.canceler, s.log))
	mux.Handle("POST /runs/{id}/retry", retryRunHandler(s.retrier, s.log))
	mux.Handle("GET /runs", listRunsHandler(s.store, s.log))
	mux.Handle("GET /runs/{id}", getRunHandler(s.store, s.log))
	mux.Handle("GET /runs/{id}/shards", runShardsHandler(s.store, s.log))
	mux.Handle("GET /runs/{id}/steps", runStepsHandler(s.store, s.log))
	mux.Handle("GET /runs/{id}/logs", runLogsHandler(s.store, s.log))
	mux.Handle("GET /runs/{id}/events", runEventsHandler(s.store, s.log))
	mux.Handle("GET /runs/{id}/stream", runStreamHandler(s.streamer, s.store, s.log))
	mux.Handle("POST /schedules", createScheduleHandler(s.schedules, s.log))
	mux.Handle("GET /schedules", listSchedulesHandler(s.schedules, s.log))
	mux.Handle("GET /schedules/{id}", getScheduleHandler(s.schedules, s.log))
	mux.Handle("DELETE /schedules/{id}", deleteScheduleHandler(s.schedules, s.log))
	mux.Handle("/ui/", s.web.Handler())
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/ui/", http.StatusFound)
	})
	mux.Handle("POST /auth/check", authCheckHandler())
	mux.Handle("POST /auth/login", loginHandler(s.users, s.tokens, s.log))
	if s.oidc != nil {
		mux.HandleFunc("GET /auth/oidc/login", s.oidc.login)
		mux.HandleFunc("GET /auth/oidc/callback", s.oidc.callback)
	}
	mux.Handle("POST /users", createUserHandler(s.users, s.log))
	mux.Handle("PUT /users/{id}", updateUserHandler(s.users, s.log))
	mux.Handle("GET /users", listUsersHandler(s.users, s.log))
	mux.Handle("DELETE /users/{id}", deleteUserHandler(s.users, s.log))
	mux.Handle("POST /credentials", createCredentialHandler(s.credentials, s.sealer, s.log))
	mux.Handle("PUT /credentials/{id}", updateCredentialHandler(s.credentials, s.sealer, s.log))
	mux.Handle("GET /credentials", listCredentialsHandler(s.credentials, s.log))
	mux.Handle("DELETE /credentials/{id}", deleteCredentialHandler(s.credentials, s.log))
	mux.Handle("POST /projects", createProjectHandler(s.projects, s.log))
	mux.Handle("PUT /projects/{id}", updateProjectHandler(s.projects, s.log))
	mux.Handle("GET /projects", listProjectsHandler(s.projects, s.log))
	mux.Handle("DELETE /projects/{id}", deleteProjectHandler(s.projects, s.log))
	mux.Handle("POST /inventories", createInventoryHandler(s.inventories, s.log))
	mux.Handle("PUT /inventories/{id}", updateInventoryHandler(s.inventories, s.log))
	mux.Handle("GET /inventories", listInventoriesHandler(s.inventories, s.log))
	mux.Handle("DELETE /inventories/{id}", deleteInventoryHandler(s.inventories, s.log))
	mux.Handle("POST /inventory-sources", createSourceHandler(s.invSources, s.inventories, authz, s.log))
	mux.Handle("PUT /inventory-sources/{id}", updateSourceHandler(s.invSources, authz, s.log))
	mux.Handle("GET /inventory-sources", listSourcesHandler(s.invSources, s.log))
	mux.Handle("DELETE /inventory-sources/{id}", deleteSourceHandler(s.invSources, s.log))
	mux.Handle("POST /inventory-sources/{id}/refresh", refreshSourceHandler(s.refresher, s.log))
	mux.Handle("POST /triggers", createTriggerHandler(s.triggers, s.templates, s.sealer, s.log))
	mux.Handle("PUT /triggers/{id}", updateTriggerHandler(s.triggers, s.log))
	mux.Handle("POST /triggers/{id}/rotate-secret", rotateTriggerSecretHandler(s.triggers, s.sealer, s.log))
	mux.Handle("GET /triggers", listTriggersHandler(s.triggers, s.log))
	mux.Handle("DELETE /triggers/{id}", deleteTriggerHandler(s.triggers, s.log))
	mux.Handle("POST /hooks/{token}", hookHandler(s.triggers, s.templates, s.submitter, s.sealer, s.log))
	mux.Handle("POST /templates", createTemplateHandler(s.templates, s.log))
	mux.Handle("PUT /templates/{id}", updateTemplateHandler(s.templates, s.log))
	mux.Handle("GET /templates", listTemplatesHandler(s.templates, s.log))
	mux.Handle("DELETE /templates/{id}", deleteTemplateHandler(s.templates, s.log))
	mux.Handle("POST /templates/{id}/launch", launchTemplateHandler(s.templates, s.submitter, authz, s.log))
	mux.Handle("POST /teams", createTeamHandler(s.teams, s.log))
	mux.Handle("GET /teams", listTeamsHandler(s.teams, s.log))
	mux.Handle("DELETE /teams/{id}", deleteTeamHandler(s.teams, s.log))
	mux.Handle("GET /teams/{id}/members", listTeamMembersHandler(s.teams, s.log))
	mux.Handle("POST /teams/{id}/members", addTeamMemberHandler(s.teams, s.log))
	mux.Handle("DELETE /teams/{id}/members/{userID}", removeTeamMemberHandler(s.teams, s.log))
	mux.Handle("POST /grants", createGrantHandler(s.grants, s.log))
	mux.Handle("GET /grants", listGrantsHandler(s.grants, s.log))
	mux.Handle("DELETE /grants/{id}", deleteGrantHandler(s.grants, s.log))
	mux.Handle("POST /import/{format}", importHandler(func() (importer.ApplyStores, bool) {
		if s.projects == nil || s.inventories == nil || s.credentials == nil ||
			s.templates == nil || s.schedules == nil {
			return importer.ApplyStores{}, false
		}
		return importer.ApplyStores{
			Projects: s.projects, Inventories: s.inventories, Credentials: s.credentials,
			Templates: s.templates, Schedules: s.schedules,
		}, true
	}, s.log))
	var handler http.Handler = mux
	if s.tokens != nil {
		gate := &authGate{tokens: s.tokens, users: s.users, audits: s.audits, log: s.log}
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
