package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/dispatch"
	"github.com/kordloom/switchtender/internal/grant"
	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/secretsource"
	"github.com/kordloom/switchtender/internal/template"
	"github.com/kordloom/switchtender/internal/util"
)

// callbackSource is the run source a provisioning callback stamps, so the runs view, the pending
// check, the stores' guard, and an auditor all name the same thing.
const callbackSource = run.SourceCallback

// callbackActorType is the actor type a callback's run and chain entry carry. A host asked for the
// run, not a person and not a forge, and a policy scoped to people or agents must not mistake it
// for either.
const callbackActorType = "host"

// maxCallbackBody bounds a callback request. It carries a key and nothing else worth reading.
const maxCallbackBody = 64 << 10

// The per-address provisioning callback limits a server applies unless it is told otherwise. Both
// count one minute at a time.
const (
	// DefaultCallbackRateLimit is how many provisioning callbacks one client address may make in a
	// minute. A host calls back once at boot, and a fleet booting together still comes from many
	// addresses, so the bound only ever meets a caller doing something other than provisioning.
	DefaultCallbackRateLimit = 30
	// DefaultCallbackKeyFailureLimit is how many wrong host config keys one client address may
	// present in a minute, so a key cannot be guessed at any useful rate even by a caller who stays
	// under the request bound.
	DefaultCallbackKeyFailureLimit = 10
)

// The server flags that set the two limits, named in every refusal so an operator reading one knows
// what to change.
const (
	// callbackRateFlag sets how many callbacks one address may make in a minute.
	callbackRateFlag = "--callback-rate-limit"
	// callbackKeyFailureFlag sets how many wrong keys one address may present in a minute.
	callbackKeyFailureFlag = "--callback-key-failure-limit"
)

// Bounds on the forward lookups host matching makes. A name is looked up only when no host matched
// by address alone, the lookups run a few at a time, and the whole pass gives up after a few
// seconds, so a large inventory of names costs a callback seconds rather than minutes.
const (
	// maxCallbackLookups is the most inventory names one callback resolves.
	maxCallbackLookups = 4096
	// callbackLookupWorkers is how many lookups run at once.
	callbackLookupWorkers = 16
	// callbackLookupBudget bounds the whole matching pass.
	callbackLookupBudget = 5 * time.Second
)

// callbackResolver looks addresses and names up for host matching. net.Resolver satisfies it, and a
// test substitutes a fixed table so matching is exercised without a network.
type callbackResolver interface {
	// LookupAddr returns the names an address reverse resolves to.
	LookupAddr(ctx context.Context, addr string) ([]string, error)
	// LookupHost returns the addresses a name resolves to.
	LookupHost(ctx context.Context, host string) ([]string, error)
}

// callbackRequest is what a host posts to its template's callback URL.
type callbackRequest struct {
	// HostConfigKey is the template's provisioning callback key.
	HostConfigKey string `json:"host_config_key"`
	// ExtraVars is read only to refuse it. AWX accepts it only from a template that prompts for
	// variables at launch, and a template here has no such prompt, so a host cannot change what the
	// template runs.
	ExtraVars json.RawMessage `json:"extra_vars,omitempty"`
}

// callbackResponse answers a callback that launched a run.
type callbackResponse struct {
	// Template is the template that launched.
	Template string `json:"template"`
	// Host is the inventory host the caller was matched to, which the run is limited to.
	Host string `json:"host"`
	// Run is the run the callback created.
	Run string `json:"run"`
	// Status is the run's status, pending_approval when an approval policy held it.
	Status run.Status `json:"status"`
}

// callbackKeyResponse returns a freshly minted callback key, once.
type callbackKeyResponse struct {
	// Template is the template the key belongs to.
	Template string `json:"template"`
	// HostConfigKey is the key to give the hosts that call back. It is shown once and never again.
	HostConfigKey string `json:"host_config_key"`
	// CallbackPath is where a host posts the key. Prefix it with the server's address.
	CallbackPath string `json:"callback_path"`
}

// callbackPath is the path a host posts to for a template.
func callbackPath(templateID string) string {
	return "/v1/templates/" + templateID + "/callback"
}

// mintCallbackKeyHandler mints a new provisioning callback key for a template, replacing any key it
// had, and returns the plaintext once. It needs the server encryption key, because the key is kept
// sealed and a host's presented key is checked against the opened value.
func mintCallbackKeyHandler(templates template.Store, sealer *credential.Sealer, authz *authorizer,
	log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if templates == nil {
			respondError(w, log, http.StatusNotFound, "templates not enabled")
			return
		}
		if sealer == nil || !sealer.Enabled() {
			respondError(w, log, http.StatusConflict,
				"callback keys need an encryption key: set SWITCHTENDER_ENCRYPTION_KEY and "+
					"SWITCHTENDER_ENCRYPTION_SALT on the server")
			return
		}
		t, ok := callbackKeyTemplate(w, r, templates, authz, log)
		if !ok {
			return
		}
		plain, err := template.NewHostConfigKey()
		if err != nil {
			log.Error("server: mint callback key: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not mint a callback key")
			return
		}
		sealed, err := sealer.Seal(plain)
		if err != nil {
			log.Error("server: seal callback key: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not mint a callback key")
			return
		}
		if err := templates.SetHostConfigKey(r.Context(), t.ID, sealed); err != nil {
			log.Error("server: save callback key: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not mint a callback key")
			return
		}
		respondJSON(w, log, http.StatusCreated, callbackKeyResponse{
			Template: t.ID, HostConfigKey: plain, CallbackPath: callbackPath(t.ID),
		}, wantsPretty(r))
	}
}

// revokeCallbackKeyHandler removes a template's provisioning callback key, so every host holding it
// is refused from then on.
func revokeCallbackKeyHandler(templates template.Store, authz *authorizer, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if templates == nil {
			respondError(w, log, http.StatusNotFound, "templates not enabled")
			return
		}
		t, ok := callbackKeyTemplate(w, r, templates, authz, log)
		if !ok {
			return
		}
		if err := templates.SetHostConfigKey(r.Context(), t.ID, ""); err != nil {
			log.Error("server: revoke callback key: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not revoke the callback key")
			return
		}
		respondJSON(w, log, http.StatusOK, map[string]string{"revoked": t.ID}, wantsPretty(r))
	}
}

// callbackKeyTemplate reads the template a key request names and authorizes the caller on it. A
// callback key is a durable way into the template that needs no account, the way a webhook trigger
// is, so managing one takes the same use access on the template that a trigger does.
func callbackKeyTemplate(w http.ResponseWriter, r *http.Request, templates template.Store,
	authz *authorizer, log *zap.Logger) (*template.Template, bool) {
	id := r.PathValue("id")
	if denyOnAuthzError(w, log, authz.authorizeAll(r.Context(), grant.AccessUse, id)) {
		return nil, false
	}
	t, err := templates.Get(r.Context(), id)
	if errors.Is(err, template.ErrNotFound) {
		respondError(w, log, http.StatusNotFound, "template not found")
		return nil, false
	}
	if err != nil {
		log.Error("server: read template: " + err.Error())
		respondError(w, log, http.StatusInternalServerError, "could not read template")
		return nil, false
	}
	return t, true
}

// callbackSettings are the provisioning callback limits and the limit check a server was given.
type callbackSettings struct {
	// perMinute bounds callbacks from one address in a minute. Zero or less keeps the default.
	perMinute int
	// wrongKeysPerMinute bounds wrong keys from one address in a minute. Zero or less keeps the
	// default.
	wrongKeysPerMinute int
	// limits evaluates a template's limit for a callback that keeps it. Nil on a server that
	// cannot run ansible-inventory, which then refuses such a callback and says why.
	limits CallbackLimitMatcher
}

// WithCallbackRateLimits sets how many provisioning callbacks, and how many wrong host config keys,
// one client address may send in a minute. A value below one keeps its default.
func WithCallbackRateLimits(perMinute, wrongKeysPerMinute int) Option {
	return func(s *Server) {
		s.callbackCfg.perMinute = perMinute
		s.callbackCfg.wrongKeysPerMinute = wrongKeysPerMinute
	}
}

// WithCallbackLimitMatcher lets a provisioning callback check the calling host against its
// template's own limit, which a template does unless it sets callback_limit to replace.
func WithCallbackLimitMatcher(m CallbackLimitMatcher) Option {
	return func(s *Server) { s.callbackCfg.limits = m }
}

// callbacks serves AWX's provisioning callback: a host posts its template's host config key, and
// the server matches the address the request came from to one host in the template's stored
// inventory, by name, ansible_host, reverse lookup of the address, and forward lookup of the
// inventory's names, and launches the template limited to that host.
//
// It answers on the template's own address and, for a template an import bound to its AWX job
// template id, on the address AWX gave it. The two share one request budget and one wrong-key
// budget per client address and one launch lock, so a caller gains nothing by alternating them.
//
// The endpoint carries no account, so the key is the credential. A wrong key, a template without
// callbacks, and a template that does not exist are refused with the same answer, so the endpoint
// says nothing about which templates exist.
//
// The launch goes through the same submit every other launch does, so approval policies hold it and
// deny rules refuse it exactly as they would a person's launch. It is written to the audit chain
// before it launches, naming the host and the address it called from, and refused if it cannot be.
// While a callback run for the same host and template is still pending or running, a second
// callback is refused rather than stacked, which is AWX's replay protection.
type callbacks struct {
	// templates holds the templates and their AWX bindings.
	templates template.Store
	// inventories holds the host lists a caller is matched against.
	inventories inventory.Store
	// submitter launches the run through the gate.
	submitter Submitter
	// store is where the pending check looks for an unfinished callback run.
	store run.Store
	// sealer opens a template's sealed key.
	sealer *credential.Sealer
	// audits records the callback before it launches.
	audits audit.Store
	// resolver looks names and addresses up for host matching.
	resolver callbackResolver
	// limits evaluates a template's limit for a callback that keeps it. Nil when it cannot.
	limits *limitCache
	// requests is the per-address callback budget this process keeps, used when the store keeps no
	// shared budgets.
	requests *loginLimiter
	// failures is the per-address wrong-key budget this process keeps, used likewise.
	failures *loginLimiter
	// budgets is the store's shared allowances, which every replica spends from, so an address gets
	// its budgets once across the install rather than once per process and again after each restart.
	// Nil when the store keeps none, and then requests and failures apply.
	budgets run.Budgets
	// perMinute is the callback budget's size.
	perMinute int
	// wrongKeys is the wrong-key budget's size.
	wrongKeys int
	// refusals decides which budget refusals reach the log.
	refusals *refusalLog
	// launching serializes the pending check and the submit on this process, so two callbacks from
	// one host arriving together cannot both find nothing pending. Across replicas the store closes
	// the same race: it refuses a second unfinished callback run for one template and host.
	launching sync.Mutex
	// log records refusals and failures.
	log *zap.Logger
}

// newCallbacks builds the callback service from the server's stores and settings.
func (s *Server) newCallbacks() *callbacks {
	resolver := s.callbackResolver
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	perMinute := s.callbackCfg.perMinute
	if perMinute < 1 {
		perMinute = DefaultCallbackRateLimit
	}
	wrongKeys := s.callbackCfg.wrongKeysPerMinute
	if wrongKeys < 1 {
		wrongKeys = DefaultCallbackKeyFailureLimit
	}
	c := &callbacks{
		templates: s.templates, inventories: s.inventories, submitter: s.submitter, store: s.store,
		sealer: s.sealer, audits: s.audits, resolver: resolver,
		requests:  &loginLimiter{windows: make(map[string]*loginWindow), max: perMinute},
		failures:  &loginLimiter{windows: make(map[string]*loginWindow)},
		perMinute: perMinute, wrongKeys: wrongKeys,
		refusals: &refusalLog{last: make(map[string]time.Time)}, log: s.log,
	}
	if budgets, ok := s.store.(run.Budgets); ok {
		c.budgets = budgets
	}
	if s.callbackCfg.limits != nil {
		c.limits = newLimitCache(s.callbackCfg.limits)
	}
	return c
}

// native serves POST /v1/templates/{id}/callback, the template's own callback address.
func (c *callbacks) native() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		addr := clientAddr(r)
		if !c.admit(r.Context(), w, addr) {
			return
		}
		if c.templates == nil || c.inventories == nil {
			respondError(w, c.log, http.StatusNotFound, "provisioning callbacks not enabled")
			return
		}
		req, ok := decodeCallback(w, r, c.log)
		if !ok {
			return
		}
		t, ok := callbackTemplate(r.Context(), c.templates, c.sealer, r.PathValue("id"),
			req.HostConfigKey, c.log)
		if !ok {
			c.refuseKey(r.Context(), w, addr)
			return
		}
		c.launch(w, r, t, req, addr, 0)
	}
}

// The keys an address's two budgets are kept under in the store's shared allowances.
const (
	// callbackRequestBudget prefixes the per-address callback budget.
	callbackRequestBudget = "callback-request:"
	// callbackKeyBudget prefixes the per-address wrong-key budget.
	callbackKeyBudget = "callback-wrong-key:"
)

// admit spends one request from addr's budget and refuses the request when that budget or the
// wrong-key budget is used up. It runs before anything is looked up, so a caller over budget learns
// nothing about which templates or AWX ids exist.
func (c *callbacks) admit(ctx context.Context, w http.ResponseWriter, addr string) bool {
	requests, wrongKeys, err := c.spendRequest(ctx, addr)
	if err != nil {
		c.log.Error("server: provisioning callback budget: " + err.Error())
		respondError(w, c.log, http.StatusServiceUnavailable,
			"refused: this address's callback budget could not be read")
		return false
	}
	if requests > c.perMinute {
		c.refuseBudget(w, addr, "too many callbacks from this address", c.perMinute, callbackRateFlag)
		return false
	}
	if wrongKeys >= c.wrongKeys {
		c.refuseBudget(w, addr, "too many wrong host config keys from this address", c.wrongKeys,
			callbackKeyFailureFlag)
		return false
	}
	return true
}

// spendRequest spends one callback from addr's request budget and returns how many that budget's
// window now holds and how many wrong keys the address has presented in its own window. The store's
// shared allowances are measured on the store's clock, so replicas agree on when a window closes.
func (c *callbacks) spendRequest(ctx context.Context, addr string) (int, int, error) {
	if c.budgets == nil {
		requests := 0
		if !c.requests.allow("callback:" + addr) {
			requests = c.perMinute + 1
		}
		wrongKeys := 0
		if c.failures.spent(addr, c.wrongKeys) {
			wrongKeys = c.wrongKeys
		}
		return requests, wrongKeys, nil
	}
	now, err := c.store.Now(ctx)
	if err != nil {
		return 0, 0, err
	}
	requests, err := c.budgets.SpendBudget(ctx, callbackRequestBudget+addr, loginWindowLength, now)
	if err != nil {
		return 0, 0, err
	}
	wrongKeys, err := c.budgets.BudgetSpent(ctx, callbackKeyBudget+addr, now)
	if err != nil {
		return 0, 0, err
	}
	return requests, wrongKeys, nil
}

// refuseBudget answers a callback over one of its address's budgets. The refusal names the limit
// and the flag that sets it, says when to try again, and is logged once per address and budget a
// minute rather than once per request, so a caller hammering a spent budget cannot flood the log.
func (c *callbacks) refuseBudget(w http.ResponseWriter, addr, what string, limit int, flag string) {
	if c.refusals.first(flag + "\x00" + addr) {
		c.log.Warn("server: provisioning callback refused by a rate limit",
			zap.String("address", addr), zap.Int("limit_per_minute", limit), zap.String("flag", flag))
	}
	w.Header().Set("Retry-After", strconv.Itoa(int(loginWindowLength/time.Second)))
	respondError(w, c.log, http.StatusTooManyRequests, fmt.Sprintf("%s: the limit is %d a minute "+
		"for one address, set with %s on the server. Wait a minute and call back again", what, limit,
		flag))
}

// refuseKey answers a callback whose key does not open a template that accepts it, and spends one
// of the caller's wrong-key budget. A wrong key, an unknown template, an unknown AWX id, and a
// template with callbacks or its AWX address off all answer exactly this. The refusal stands when
// the budget cannot be spent, since the key was wrong either way.
func (c *callbacks) refuseKey(ctx context.Context, w http.ResponseWriter, addr string) {
	if c.budgets == nil {
		c.failures.record(addr)
	} else if err := c.spendWrongKey(ctx, addr); err != nil {
		c.log.Error("server: provisioning callback wrong-key budget: " + err.Error())
	}
	respondError(w, c.log, http.StatusForbidden, "invalid host config key")
}

// spendWrongKey spends one wrong key from addr's shared wrong-key budget, on the store's clock.
func (c *callbacks) spendWrongKey(ctx context.Context, addr string) error {
	now, err := c.store.Now(ctx)
	if err != nil {
		return err
	}
	_, err = c.budgets.SpendBudget(ctx, callbackKeyBudget+addr, loginWindowLength, now)
	return err
}

// launch carries out a callback once the key has opened t: the template's own refusals, host
// matching, the template's limit, the replay check, the chain entry, and the submit through the
// gate. awxID is the AWX job template id the callback arrived through, zero when it came to the
// template's own address, and the chain entry records which.
func (c *callbacks) launch(w http.ResponseWriter, r *http.Request, t *template.Template,
	req callbackRequest, addr string, awxID int64) {
	if len(req.ExtraVars) > 0 && string(req.ExtraVars) != "null" {
		respondError(w, c.log, http.StatusBadRequest,
			"a provisioning callback launches the template as it is saved, so extra_vars is refused")
		return
	}
	if msg := callbackTemplateRefusal(t); msg != "" {
		respondError(w, c.log, http.StatusConflict, msg)
		return
	}
	vars, sealedVars, err := callbackVars(t)
	if err != nil {
		respondError(w, c.log, http.StatusBadRequest,
			"cannot start automatically, user input required: "+err.Error())
		return
	}
	inv, hosts, status, msg := callbackInventoryHosts(r.Context(), c.inventories, t.InventoryID, c.log)
	if msg != "" {
		respondError(w, c.log, status, msg)
		return
	}
	matches := matchCallbackHosts(r.Context(), hosts, addr, c.resolver)
	switch len(matches) {
	case 0:
		c.log.Warn("server: provisioning callback from an unknown host",
			zap.String("template", t.ID), zap.String("address", addr))
		respondError(w, c.log, http.StatusBadRequest,
			"no host in this template's inventory matches the address "+addr)
		return
	case 1:
	default:
		names := make([]string, 0, len(matches))
		for _, h := range matches {
			names = append(names, h.Name)
		}
		respondError(w, c.log, http.StatusBadRequest, "more than one host in this template's "+
			"inventory matches the address "+addr+": "+strings.Join(names, ", "))
		return
	}
	host := matches[0].Name
	if status, msg := c.limitRefusal(r.Context(), t, inv, host); msg != "" {
		respondError(w, c.log, status, msg)
		return
	}

	c.launching.Lock()
	defer c.launching.Unlock()
	pending, err := pendingCallbackRun(r.Context(), c.store, t.ID, host)
	if err != nil {
		c.log.Error("server: provisioning callback pending check: " + err.Error())
		respondError(w, c.log, http.StatusInternalServerError, "could not launch the template")
		return
	}
	// The template and the host are joined by a NUL byte, which no name contains, and the dedupe
	// key keeps the pair as its digest, so the key is text on every backend whatever the host is
	// named.
	existing, key, err := run.ResolveDedupe(r.Context(), c.store, callbackSource,
		t.ID+"\x00"+host, time.Now())
	if err != nil {
		c.log.Error("server: provisioning callback dedupe: " + err.Error())
		respondError(w, c.log, http.StatusInternalServerError, "could not launch the template")
		return
	}
	switch {
	case pending != nil:
		w.Header().Set("Location", "/v1/runs/"+pending.ID)
		respondError(w, c.log, http.StatusConflict,
			"a callback run for "+host+" is already pending: "+pending.ID)
		return
	case existing != nil:
		// Another replica launched one in the last few seconds, which the pending check above
		// cannot see until that run is saved. Refused the same way.
		w.Header().Set("Location", "/v1/runs/"+existing.ID)
		respondError(w, c.log, http.StatusConflict,
			"a callback for "+host+" launched "+existing.ID+" moments ago")
		return
	}

	// The host's name comes from the inventory, where a YAML escape can put a NUL byte in it. The run
	// stores its actor cleaned, so the chain entry carries the same cleaned text rather than bytes
	// PostgreSQL refuses, which would refuse every callback from that host with a 503.
	actor := "host " + util.SafeText(host) + " from " + addr
	ctx := r.Context()
	if c.audits != nil {
		entry := &audit.Entry{
			ID: audit.NewID(), Actor: actor, ActorType: callbackActorType,
			Method: http.MethodPost, Path: firedPath(t.ID, awxID),
		}
		if aerr := c.audits.Append(ctx, entry); aerr != nil {
			c.log.Error("server: record provisioning callback: " + aerr.Error())
			respondError(w, c.log, http.StatusServiceUnavailable,
				"refused: the callback could not be recorded in the audit trail")
			return
		}
		receipt := audit.Receipt(entry)
		w.Header().Set(AuditReceiptHeader, receipt)
		ctx = run.WithAuditReceipt(ctx, receipt)
	}

	opts := append(t.LaunchOptions(),
		run.WithExactExtraVars(vars),
		run.WithSealedVars(sealedVars),
		// The host, by its inventory name, stands in for the template's own limit: the run reaches
		// the host that asked and nothing else. A template that keeps its limit was checked above to
		// select this host.
		run.WithLimit(host),
		run.WithIdempotencyKey(key),
		run.WithSource(callbackSource, t.ID),
		run.WithActor(actor),
		run.WithActorType(callbackActorType))
	if t.OrgID != "" {
		opts = append(opts, run.WithOrgID(t.OrgID))
	}
	created, err := c.submitter.Submit(ctx, t.Playbook, t.Inventory, opts...)
	switch {
	case errors.Is(err, run.ErrCallbackPending):
		// Another replica's callback for this host saved its run after the pending check above,
		// and the store refused this one rather than run the template on the host twice.
		if live, perr := pendingCallbackRun(r.Context(), c.store, t.ID, host); perr == nil &&
			live != nil {
			w.Header().Set("Location", "/v1/runs/"+live.ID)
		}
		respondError(w, c.log, http.StatusConflict, "a callback run for "+host+" is already pending")
		return
	case errors.Is(err, dispatch.ErrPolicyDenied), errors.Is(err, dispatch.ErrQueueUnlicensed):
		respondError(w, c.log, http.StatusForbidden, err.Error())
		return
	case errors.Is(err, credential.ErrNoSecret), errors.Is(err, credential.ErrUnreadable):
		c.log.Warn("server: provisioning callback: " + err.Error())
		respondError(w, c.log, http.StatusConflict, err.Error())
		return
	case err != nil:
		c.log.Error("server: provisioning callback: " + err.Error())
		respondError(w, c.log, http.StatusBadGateway, "could not launch the template")
		return
	}
	w.Header().Set("Location", "/v1/runs/"+created.ID)
	respondJSON(w, c.log, http.StatusCreated, callbackResponse{
		Template: t.ID, Host: host, Run: created.ID, Status: created.Status,
	}, wantsPretty(r))
}

// firedPath is the chain path a callback is recorded at: the template's callback address and, for a
// callback that arrived on the AWX-compatible address, the AWX job template id it called, so the
// evidence says which address the host used.
func firedPath(templateID string, awxID int64) string {
	if awxID > 0 {
		return callbackPath(templateID) + "/fired/awx/" + strconv.FormatInt(awxID, 10)
	}
	return callbackPath(templateID) + "/fired"
}

// refusalLog decides which budget refusals reach the log: the first for each key in each window.
type refusalLog struct {
	// mu guards last.
	mu sync.Mutex
	// last maps a key to when a refusal for it was last logged.
	last map[string]time.Time
	// now reads the clock. Nil means time.Now.
	now func() time.Time
}

// first reports whether a refusal for key is the first in its window, and records it when it is.
// Old keys are pruned once the map grows past a bound, so an address sweep cannot grow it forever.
func (l *refusalLog) first(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if l.now != nil {
		now = l.now()
	}
	if len(l.last) > 4096 {
		for k, at := range l.last {
			if now.Sub(at) > loginWindowLength {
				delete(l.last, k)
			}
		}
	}
	if at, ok := l.last[key]; ok && now.Sub(at) <= loginWindowLength {
		return false
	}
	l.last[key] = now
	return true
}

// decodeCallback reads the key from a JSON or form body, the two shapes AWX's documented curl and
// its request_tower_configuration script send. It writes the refusal and reports false when the
// body cannot be read.
func decodeCallback(w http.ResponseWriter, r *http.Request, log *zap.Logger) (callbackRequest, bool) {
	var req callbackRequest
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxCallbackBody))
	if err != nil {
		respondError(w, log, http.StatusRequestEntityTooLarge, "callback body too large")
		return req, false
	}
	mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mediaType == "application/x-www-form-urlencoded" {
		form, err := url.ParseQuery(string(body))
		if err != nil {
			respondError(w, log, http.StatusBadRequest, "callback body is not a readable form")
			return req, false
		}
		req.HostConfigKey = form.Get("host_config_key")
		if v := form.Get("extra_vars"); v != "" {
			req.ExtraVars = json.RawMessage(`"set"`)
		}
		return req, true
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return req, true
	}
	// Foreign rather than strict: a boot script written for AWX sends whatever AWX accepted, and the
	// only field that matters here is the key.
	if err := decodeForeign(body, &req); err != nil {
		respondError(w, log, http.StatusBadRequest, "callback body is not valid JSON")
		return req, false
	}
	return req, true
}

// callbackTemplate returns the template named by id when it accepts callbacks and presented is its
// key, and false otherwise. Every reason for false answers the caller the same way.
func callbackTemplate(ctx context.Context, templates template.Store, sealer *credential.Sealer, id,
	presented string, log *zap.Logger) (*template.Template, bool) {
	t, err := templates.Get(ctx, id)
	if err != nil {
		if !errors.Is(err, template.ErrNotFound) {
			log.Error("server: provisioning callback: read template: " + err.Error())
		}
		return nil, false
	}
	return t, callbackKeyOpens(t, sealer, presented, log)
}

// callbackKeyOpens reports whether t accepts callbacks and presented is its key.
func callbackKeyOpens(t *template.Template, sealer *credential.Sealer, presented string,
	log *zap.Logger) bool {
	if !t.AllowCallbacks || t.HostConfigKey == "" || sealer == nil || !sealer.Enabled() {
		return false
	}
	key, err := sealer.Open(t.HostConfigKey)
	if err != nil {
		log.Error("server: provisioning callback: open key: " + err.Error())
		return false
	}
	return template.HostConfigKeyMatches(key, presented)
}

// callbackTemplateRefusal returns why a template that accepts callbacks still cannot launch from
// one, or empty when it can. Each is something the template's owner fixes, not the host.
func callbackTemplateRefusal(t *template.Template) string {
	switch {
	case len(t.Steps) > 0:
		return "this template is a workflow, and a provisioning callback launches a single job"
	case run.NormalizeTool(t.Tool) != run.ToolAnsible:
		return "a provisioning callback launches an Ansible template, and this template runs " +
			run.NormalizeTool(t.Tool)
	case t.InventoryID == "":
		return "this template names no stored inventory, so there is no host list to match the " +
			"caller against"
	}
	return ""
}

// callbackVars returns the variables a callback launch carries, resolved the way every launch with
// nobody present to answer the survey resolves them: the template's own and its survey's defaults,
// and, apart from them, the sealed defaults of its secret survey fields, which ride onto the run
// still sealed the way a launch that leaves a secret field blank carries them. A template extra var
// named like a secret field is dropped, so the secret is the only value that variable has. A survey
// with a required question and no default needs a person, so it is refused, as AWX refuses a
// template that cannot start without user input. A required question that has a default takes it,
// the same as on a schedule or a webhook, where this refused it on a callback alone.
func callbackVars(t *template.Template) (map[string]any, map[string]string, error) {
	return t.UnattendedVars()
}

// callbackInventoryHosts reads the inventory and the host list a callback is matched against. On
// failure it returns the status and message to answer with.
func callbackInventoryHosts(ctx context.Context, inventories inventory.Store, id string,
	log *zap.Logger) (*inventory.Inventory, []inventory.Host, int, string) {
	inv, err := inventories.Get(ctx, id)
	if errors.Is(err, inventory.ErrNotFound) {
		return nil, nil, http.StatusConflict, "this template's inventory no longer exists"
	}
	if err != nil {
		log.Error("server: provisioning callback: read inventory: " + err.Error())
		return nil, nil, http.StatusInternalServerError, "could not read the template's inventory"
	}
	// The host list of an inventory resolved from Vault, a secret manager, or a command exists only
	// while a run is starting. Resolving it here would let anybody holding the key make the server
	// run that resolution on demand, so those inventories are refused and say why.
	if secretsource.NormalizeKind(inv.ContentSource) != secretsource.KindLocal {
		return nil, nil, http.StatusConflict, "this template's inventory is resolved from " +
			inv.ContentSource + " when a run starts, so it holds no host list to match a callback " +
			"against. Use a stored or synced inventory"
	}
	// A smart or constructed inventory has no host list of its own until it is resolved from its
	// inputs, which runs ansible-inventory for a constructed one. That is refused for the same
	// reason: a request that holds only the key must not make the server resolve anything.
	if inv.Composed() {
		return nil, nil, http.StatusConflict, "this template's inventory is a " + inv.Kind +
			" inventory, resolved from its inputs when a run starts, so it holds no host list to " +
			"match a callback against. Point the template at a stored or synced inventory"
	}
	hosts, err := inventory.Hosts(inv.Content)
	if err != nil {
		return nil, nil, http.StatusConflict, "this template's inventory could not be read as a " +
			"host list: " + err.Error()
	}
	return inv, hosts, 0, ""
}

// matchCallbackHosts returns the inventory hosts a request from addr may be, by AWX's rules. The
// caller's names are its address and what the address reverse resolves to, less any .arpa name. A
// host matches when its effective address, ansible_host or else its name, is one of them. When that
// does not single out one host, each inventory name is resolved forward, and a host matches when
// one of its addresses is the caller's.
func matchCallbackHosts(ctx context.Context, hosts []inventory.Host, addr string,
	resolver callbackResolver) []inventory.Host {
	ctx, cancel := context.WithTimeout(ctx, callbackLookupBudget)
	defer cancel()
	remote := map[string]bool{normalizeHostName(addr): true}
	if names, err := resolver.LookupAddr(ctx, addr); err == nil {
		for _, n := range names {
			if n = normalizeHostName(n); n != "" && !strings.HasSuffix(n, ".arpa") {
				remote[n] = true
			}
		}
	}
	byAddress := map[string][]inventory.Host{}
	for _, h := range hosts {
		a := normalizeHostName(h.Address())
		byAddress[a] = append(byAddress[a], h)
	}
	matched := map[string]inventory.Host{}
	for a, hs := range byAddress {
		if remote[a] {
			for _, h := range hs {
				matched[h.Name] = h
			}
		}
	}
	if len(matched) != 1 {
		forwardMatches(ctx, byAddress, remote, resolver, matched)
	}
	out := make([]inventory.Host, 0, len(matched))
	for _, h := range matched {
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// forwardMatches resolves each inventory name that is not already an address and adds the hosts
// behind any name that resolves to one of the caller's addresses. The lookups are bounded in
// number, concurrency, and time by the constants above.
func forwardMatches(ctx context.Context, byAddress map[string][]inventory.Host, remote map[string]bool,
	resolver callbackResolver, matched map[string]inventory.Host) {
	names := make([]string, 0, len(byAddress))
	for a := range byAddress {
		if net.ParseIP(a) == nil {
			names = append(names, a)
		}
	}
	sort.Strings(names)
	if len(names) > maxCallbackLookups {
		names = names[:maxCallbackLookups]
	}
	var (
		mu  sync.Mutex
		wg  sync.WaitGroup
		sem = make(chan struct{}, callbackLookupWorkers)
	)
	for _, name := range names {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			continue
		}
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			defer func() { <-sem }()
			ips, err := resolver.LookupHost(ctx, name)
			if err != nil {
				return
			}
			for _, ip := range ips {
				if ip = normalizeHostName(ip); ip != name && remote[ip] {
					mu.Lock()
					for _, h := range byAddress[name] {
						matched[h.Name] = h
					}
					mu.Unlock()
					return
				}
			}
		}(name)
	}
	wg.Wait()
}

// normalizeHostName puts a name or address in the one form two spellings of it compare equal in:
// an address in its canonical text, a name in lower case without a trailing dot.
func normalizeHostName(s string) string {
	s = strings.TrimSpace(s)
	if ip := net.ParseIP(s); ip != nil {
		return ip.String()
	}
	return strings.TrimSuffix(strings.ToLower(s), ".")
}

// pendingCallbackRun returns a callback run of this template for this host that has not finished,
// or nil when there is none. The host is compared in the form a store keeps a run's limit in, so a
// host whose name a text column cannot hold still finds the run its last callback made.
func pendingCallbackRun(ctx context.Context, store run.Store, templateID, host string) (*run.Run, error) {
	live, err := store.NonTerminal(ctx)
	if err != nil {
		return nil, err
	}
	limit := util.SafeText(host)
	for _, r := range live {
		if run.LiveCallback(r) && r.SourceID == templateID && r.Limit == limit {
			return r, nil
		}
	}
	return nil, nil
}
