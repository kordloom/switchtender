// Package paid is the seam between the Community core and the paid module.
//
// Every paid capability is an interface here, with a registry slot that holds either the Community
// stub or the implementation the paid module registers from its init. The paid module lives in a
// private repository mounted at internal/paid/impl as a git submodule that public clones do not
// fetch, and every file in it carries the paid build tag, so a build without the tag, or without
// the submodule, is the Community binary: it cannot do the paid things because the code that does
// them is not in it.
//
// This package sits below every package that consumes a capability, so the interfaces name only
// leaf types: run, user, auth, audit, binding, and the license features that name the tiers. The
// stubs never return a silent nil. Each one refuses with ErrRequiresTier wrapping the same line
// license.Allow prints, so a Community refusal reads the same as an unlicensed one.
package paid

import (
	"context"
	"net/http"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/auth"
	"github.com/kordloom/switchtender/internal/binding"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/user"
)

// Slot holds one capability's implementation, or its stub until one is registered. A second
// registration panics, since two paid modules in one binary is a build mistake, not a choice.
type Slot[T any] struct {
	// mu guards value and set.
	mu sync.RWMutex
	// value is what Get returns: the stub until Set replaces it.
	value T
	// set reports whether a registration happened.
	set bool
	// name is the capability, for the panic message.
	name string
}

// NewSlot returns a slot that answers with stub until something registers.
func NewSlot[T any](name string, stub T) *Slot[T] {
	return &Slot[T]{value: stub, name: name}
}

// Set registers v, panicking when the slot was already registered.
func (s *Slot[T]) Set(v T) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.set {
		panic("paid: " + s.name + " registered twice")
	}
	s.value = v
	s.set = true
}

// Get returns the registered implementation, or the stub when there is none.
func (s *Slot[T]) Get() T {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.value
}

// Registered reports whether a paid implementation has been registered.
func (s *Slot[T]) Registered() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.set
}

// DirectoryConfig is what every directory sign-in kind is built from: the user store it provisions
// into, the token store a browser flow issues sessions from, the role a new account gets, the
// group-to-role map, and the logger.
type DirectoryConfig struct {
	// Users is where signed-in accounts are provisioned and looked up.
	Users user.Store
	// Tokens issues sessions for browser flows. LDAP and JWT do not need it.
	Tokens auth.Store
	// DefaultRole is the role a first-time account gets when no group maps it.
	DefaultRole user.Role
	// RoleMap maps a directory group to a role.
	RoleMap map[string]user.Role
	// Log records sign-in activity.
	Log *zap.Logger
}

// OIDCConfig configures OpenID Connect sign-in.
type OIDCConfig struct {
	// Directory is the shared sign-in configuration.
	Directory DirectoryConfig
	// Issuer is the provider's issuer URL, discovered at startup.
	Issuer string
	// ClientID is the relying party's client id.
	ClientID string
	// ClientSecret is the relying party's client secret.
	ClientSecret string
	// RedirectURL is the callback the provider returns to.
	RedirectURL string
}

// SAMLConfig configures SAML sign-in.
type SAMLConfig struct {
	// Directory is the shared sign-in configuration.
	Directory DirectoryConfig
	// IDPMetadataURL is where the identity provider's metadata is fetched from at startup.
	IDPMetadataURL string
	// BaseURL is this service provider's public base URL.
	BaseURL string
	// CertFile and KeyFile hold the service provider's PEM keypair.
	CertFile, KeyFile string
	// UsernameAttr is the assertion attribute carrying the username.
	UsernameAttr string
	// GroupsAttr is the assertion attribute carrying group names.
	GroupsAttr string
}

// LDAPConfig configures LDAP sign-in.
type LDAPConfig struct {
	// Directory is the shared sign-in configuration.
	Directory DirectoryConfig
	// URL is the directory server.
	URL string
	// BindDN and BindPassword are the search account.
	BindDN, BindPassword string
	// BaseDN is where user searches start.
	BaseDN string
	// UserFilter is the search filter with the username substituted in.
	UserFilter string
}

// JWTConfig configures bearer-token sign-in against an issuer's key set.
type JWTConfig struct {
	// Directory is the shared sign-in configuration.
	Directory DirectoryConfig
	// JWKSURL is where the issuer's keys are fetched from.
	JWKSURL string
	// Issuer is the required iss claim.
	Issuer string
	// Audience is the required aud claim, any audience when empty.
	Audience string
	// UsernameClaim names the claim carrying the username.
	UsernameClaim string
	// GroupsClaim names the claim carrying group names.
	GroupsClaim string
	// AgentClaim names the claim that marks a token as an agent's.
	AgentClaim string
}

// OIDCDirectory is a running OpenID Connect sign-in: the two browser handlers and the brand the
// sign-in page shows.
type OIDCDirectory interface {
	// Login starts the browser flow.
	Login(w http.ResponseWriter, r *http.Request)
	// Callback finishes it.
	Callback(w http.ResponseWriter, r *http.Request)
	// Brand is the provider's display name for the sign-in page.
	Brand() string
	// WithAudits records sign-ins to the audit chain.
	WithAudits(a audit.Store)
}

// SAMLDirectory is a running SAML sign-in: the login redirect, the assertion consumer, and the
// service provider metadata.
type SAMLDirectory interface {
	// Login starts the browser flow.
	Login(w http.ResponseWriter, r *http.Request)
	// ACS consumes the identity provider's assertion.
	ACS(w http.ResponseWriter, r *http.Request)
	// Metadata serves the service provider metadata.
	Metadata(w http.ResponseWriter, r *http.Request)
	// WithAudits records sign-ins to the audit chain.
	WithAudits(a audit.Store)
}

// LDAPDirectory checks a username and password against the directory.
type LDAPDirectory interface {
	// Authenticate binds as the user and returns the provisioned account.
	Authenticate(ctx context.Context, username, password string) (*user.User, error)
}

// JWTDirectory verifies bearer tokens from an external issuer.
type JWTDirectory interface {
	// Authenticate verifies raw and returns the account it maps to.
	Authenticate(ctx context.Context, raw string) (*user.User, error)
	// AuthenticateActor is Authenticate that also reports whether the token marks an agent.
	AuthenticateActor(ctx context.Context, raw string) (*user.User, bool, error)
}

// Directory builds directory sign-in. Each constructor does its network discovery at startup and
// fails there rather than half-enabling sign-in. A Community build refuses each one naming Pro.
type Directory interface {
	// OIDC discovers the provider and returns the sign-in.
	OIDC(ctx context.Context, cfg OIDCConfig) (OIDCDirectory, error)
	// SAML fetches the identity provider metadata and returns the sign-in.
	SAML(ctx context.Context, cfg SAMLConfig) (SAMLDirectory, error)
	// LDAP validates the settings and returns the sign-in.
	LDAP(ctx context.Context, cfg LDAPConfig) (LDAPDirectory, error)
	// JWT fetches the issuer's keys and returns the sign-in.
	JWT(ctx context.Context, cfg JWTConfig) (JWTDirectory, error)
}

// RegoModule is one Rego source file a Rego policy compiles.
type RegoModule struct {
	// File is the path the policy file names it by, relative to the policy file. It is part of what
	// the bundle digest covers, so moving a file is visible in the record.
	File string `json:"file"`
	// Source is the module text exactly as read.
	Source string `json:"source"`
}

// RegoSettings are what a Rego policy sets beside its modules: the syntax the modules are parsed
// as, how long one evaluation may take, and what the warn rule does.
type RegoSettings struct {
	// Package is the decision package's path, for example data.switchtender, or empty to take the
	// first module's package.
	Package string
	// Syntax is the Rego syntax the modules are parsed as, v1 or v0.
	Syntax string
	// Timeout bounds one evaluation.
	Timeout time.Duration
	// Warn is what the warn rule does, hold or note.
	Warn string
}

// RegoDecision is what a Rego policy decided about one run, in the vocabulary the YAML rules use.
type RegoDecision struct {
	// Deny holds the reasons the submission is refused, empty when nothing refused it.
	Deny []string
	// Hold holds the reasons the run waits for approval, empty when nothing held it.
	Hold []string
	// Note holds the warnings a policy set to warn: note recorded instead of holding the run.
	Note []string
	// RequireDistinctApprover demands an approver other than the requester.
	RequireDistinctApprover bool
	// PlanGate sends a Terraform or OpenTofu apply through a plan before it applies.
	PlanGate bool
	// Err is why the policy could not decide. A decision with an error is a refusal, never a pass.
	Err error
}

// RegoProgram is a compiled Rego bundle ready to decide runs.
type RegoProgram interface {
	// Package is the decision package's path after compilation resolved it.
	Package() string
	// Reads reports whether the modules reference the named top-level input field.
	Reads(field string) bool
	// Evaluate evaluates the bundle against r once, with the input r describes. The caller owns any
	// second pass, such as judging a dry run again as the real run it partly is.
	Evaluate(r *run.Run) RegoDecision
}

// RegoCompiler compiles Rego bundles. A Community build refuses naming Team.
type RegoCompiler interface {
	// Compile parses and prepares the modules under the settings.
	Compile(modules []RegoModule, settings RegoSettings) (RegoProgram, error)
}

// RuleCriteria are the paid criteria of one YAML rule: the fields a Pro or Team license adds to the
// one plain hold. The Community core validates and stores them but cannot evaluate them.
type RuleCriteria struct {
	// Effect is what the rule does to a matched run, deny among them.
	Effect string
	// MinRisk is the risk floor, empty for none.
	MinRisk string
	// Reversibility is the reversibility floor, empty for none.
	Reversibility string
	// Actor scopes the rule to one actor name.
	Actor string
	// Account scopes the rule to the account a credential acts for.
	Account string
	// ActorKind scopes the rule to human or agent actors.
	ActorKind string
	// RequireDistinctApprover demands an approver other than the requester.
	RequireDistinctApprover bool
}

// RuleEvaluator decides the paid criteria of a YAML rule against one run.
type RuleEvaluator interface {
	// Matches reports whether r meets every paid criterion in c. A Community build returns
	// ErrRequiresTier, and the caller holds the run rather than passing it.
	Matches(r *run.Run, c RuleCriteria) (bool, error)
}

// RegisterSource is where a change register is drawn from: the period's runs, the chain, the
// install identity the chain's tree leaves bind to, and the verifier that holds each approval to
// its seal.
type RegisterSource struct {
	// Runs reads the period's runs.
	Runs run.Store
	// Audits reads the chain the register is drawn from and verified against.
	Audits audit.Store
	// Producer is the install identity the tree profile's leaves bind to.
	Producer audit.Identity
	// Binder holds each approval to its seal. Nil marks every approval unchecked.
	Binder *binding.Verifier
}

// RegisterPeriod bounds one register: From inclusive, To exclusive, collected at Now, carrying at
// most Limit changes.
type RegisterPeriod struct {
	// From and To bound the period, half open.
	From, To time.Time
	// Now is when the register is collected.
	Now time.Time
	// Limit caps how many changes the document carries, the default cap when not positive.
	Limit int
}

// EmitterConfig configures a register emitter: where packs are written, how often, and who hears
// about each one.
type EmitterConfig struct {
	// Source is what each pack is drawn from.
	Source RegisterSource
	// Dir is where packs are written and where the emitter reads its own progress from.
	Dir string
	// Cadence is how long each pack covers and how often one is written.
	Cadence time.Duration
	// Log records generation activity.
	Log *zap.Logger
	// Notify, when set, is called with the path of each pack written.
	Notify func(path string, from, to time.Time)
	// Now reads the clock, replaced in tests. Nil means time.Now.
	Now func() time.Time
	// MaxChanges caps how many changes one pack carries, the default cap when not positive.
	MaxChanges int
}

// Emitter writes a change register covering each elapsed period.
type Emitter interface {
	// Start launches the loop, failing when the directory cannot be used.
	Start() error
	// Close stops the loop and waits for it.
	Close()
}

// ChangeRegister produces the period change register. A Community build refuses naming Team.
type ChangeRegister interface {
	// NewEmitter returns an emitter writing a pack per cadence.
	NewEmitter(cfg EmitterConfig) (Emitter, error)
	// Render collects the period and writes it as a self-contained HTML document.
	Render(ctx context.Context, src RegisterSource, p RegisterPeriod) ([]byte, error)
	// Build collects the period and returns the register as a value for JSON encoding, the same
	// rows the document renders.
	Build(ctx context.Context, src RegisterSource, p RegisterPeriod) (any, error)
}

// WorkerConfig is what a worker daemon starts from: the flags the worker command took.
type WorkerConfig struct {
	// DB is the shared database the worker executes against.
	DB string
	// Server is the control node to dial over the relay instead of a database.
	Server string
	// Name is the worker's owner name on the runs it claims.
	Name string
	// Queues are the named queues the worker serves, the default queue when empty.
	Queues []string
	// Workers is how many runs execute at once.
	Workers int
	// PolicyFile is the policy file a relay worker enforces locally.
	PolicyFile string
	// DeliveryKeys are the private keys that open secrets a control node sealed to this worker.
	DeliveryKeys []string
	// PluginsDir is where tool plugins load from.
	PluginsDir string
	// AllowContainerEE allows containerized execution environments.
	AllowContainerEE bool
	// DefaultImage is the image a containerized run uses when it names none.
	DefaultImage string
	// RequireImageDigest refuses images not pinned by digest.
	RequireImageDigest bool
	// FactsInterval is how often facts are refreshed.
	FactsInterval time.Duration
	// RetainFacts is how many facts snapshots are kept per host.
	RetainFacts int
	// RunTimeout bounds one run, no bound when zero.
	RunTimeout time.Duration
	// Log records the daemon's activity.
	Log *zap.Logger
}

// Worker is the distributed worker daemon. A Community build refuses naming Team before it touches
// the database.
type Worker interface {
	// Run starts the daemon and returns when ctx is done.
	Run(ctx context.Context, cfg WorkerConfig) error
}

// ReconcileRequest is what a reconcile proposal is built from: the drift check that observed the
// drift, the drifted host, and the sealed plan a Terraform or OpenTofu check kept.
type ReconcileRequest struct {
	// Check is the drift check run.
	Check *run.Run
	// Host is the drifted host.
	Host string
	// PlanFile is the sealed plan the check saved, empty for an Ansible check.
	PlanFile string
}

// Reconciler builds one-click reconcile proposals. A Community build refuses naming Team.
//
// The reconciler owns only the run spec that turns the check into the change. The caller holds
// the proposal for approval and records who proposed it after the spec is applied, so no
// reconciler can release its own proposal.
type Reconciler interface {
	// Propose returns the submit options that reconcile the drift the check observed.
	Propose(ctx context.Context, req ReconcileRequest) ([]run.SubmitOption, error)
}

// The registry: one slot per capability, each answering with its stub until the paid module's
// init registers the implementation.
var (
	directorySlot      = NewSlot[Directory]("directory", stubDirectory{})
	regoCompilerSlot   = NewSlot[RegoCompiler]("rego compiler", stubRegoCompiler{})
	ruleEvaluatorSlot  = NewSlot[RuleEvaluator]("rule evaluator", stubRuleEvaluator{})
	changeRegisterSlot = NewSlot[ChangeRegister]("change register", stubChangeRegister{})
	workerSlot         = NewSlot[Worker]("worker", stubWorker{})
	reconcilerSlot     = NewSlot[Reconciler]("reconciler", stubReconciler{})
)

// RegisterDirectory installs the paid directory sign-in. It panics on a second call.
func RegisterDirectory(d Directory) { directorySlot.Set(d) }

// CurrentDirectory returns the directory sign-in, the Community stub when none is registered.
func CurrentDirectory() Directory { return directorySlot.Get() }

// RegisterRegoCompiler installs the paid Rego compiler. It panics on a second call.
func RegisterRegoCompiler(c RegoCompiler) { regoCompilerSlot.Set(c) }

// CurrentRegoCompiler returns the Rego compiler, the Community stub when none is registered.
func CurrentRegoCompiler() RegoCompiler { return regoCompilerSlot.Get() }

// RegisterRuleEvaluator installs the paid rule evaluator. It panics on a second call.
func RegisterRuleEvaluator(e RuleEvaluator) { ruleEvaluatorSlot.Set(e) }

// CurrentRuleEvaluator returns the rule evaluator, the Community stub when none is registered.
func CurrentRuleEvaluator() RuleEvaluator { return ruleEvaluatorSlot.Get() }

// RegisterChangeRegister installs the paid change register. It panics on a second call.
func RegisterChangeRegister(r ChangeRegister) { changeRegisterSlot.Set(r) }

// CurrentChangeRegister returns the change register, the Community stub when none is registered.
func CurrentChangeRegister() ChangeRegister { return changeRegisterSlot.Get() }

// RegisterWorker installs the paid worker daemon. It panics on a second call.
func RegisterWorker(w Worker) { workerSlot.Set(w) }

// CurrentWorker returns the worker daemon, the Community stub when none is registered.
func CurrentWorker() Worker { return workerSlot.Get() }

// RegisterReconciler installs the paid reconciler. It panics on a second call.
func RegisterReconciler(r Reconciler) { reconcilerSlot.Set(r) }

// CurrentReconciler returns the reconciler, the Community stub when none is registered.
func CurrentReconciler() Reconciler { return reconcilerSlot.Get() }

// Registered reports whether every capability has a paid implementation, which is true only in
// the paid build with the module present.
func Registered() bool {
	return directorySlot.Registered() && regoCompilerSlot.Registered() &&
		ruleEvaluatorSlot.Registered() && changeRegisterSlot.Registered() &&
		workerSlot.Registered() && reconcilerSlot.Registered()
}
