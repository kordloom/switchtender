package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/dispatch"
	"github.com/kordloom/switchtender/internal/grant"
	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/review"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/template"
	"github.com/kordloom/switchtender/internal/trigger"
)

// maxHookBody caps the inbound webhook body buffered to verify its HMAC signature. GitHub caps
// delivery payloads at 25 MiB, matched here so a signed push is never rejected for size.
const maxHookBody = 25 << 20

// createTriggerRequest is the JSON body accepted by POST /triggers.
type createTriggerRequest struct {
	// Name labels the trigger. Required.
	Name string `json:"name"`
	// TemplateID is the template the trigger launches. Required.
	TemplateID string `json:"template_id"`
	// RequireSignature enforces HMAC signature verification on inbound webhooks. It needs the server
	// to have an encryption key so the signing secret can be sealed at rest.
	RequireSignature bool `json:"require_signature"`
	// Review, when set, makes the trigger plan pull requests instead of firing the template. A
	// review trigger always verifies its deliveries, so it needs the encryption key too.
	Review *trigger.Review `json:"review,omitempty"`
}

// createTriggerResponse returns the trigger and its webhook path, shown once.
type createTriggerResponse struct {
	// Trigger is the stored record.
	Trigger *trigger.Trigger `json:"trigger"`
	// WebhookPath is where a git host posts to fire the trigger. Point the remote at this path
	// on your server; the secret token in it is not recoverable later.
	WebhookPath string `json:"webhook_path"`
	// SigningSecret is the HMAC secret to set on the git host's webhook, shown once and never
	// recoverable later. Empty when the server has no encryption key configured.
	SigningSecret string `json:"signing_secret,omitempty"`
}

// listTriggersResponse wraps the trigger list.
type listTriggersResponse struct {
	// Triggers is the ordered list.
	Triggers []*trigger.Trigger `json:"triggers"`
	// Count is the number returned.
	Count int `json:"count"`
	// Total is how many rows exist before the response cap, so a caller shown a prefix knows it is
	// one. Equal to Count for every ordinary install.
	Total int `json:"total"`
}

// createTriggerHandler mints a trigger and returns its webhook path once. When the server has an
// encryption key it also mints a sealed HMAC signing secret and returns the plaintext once, so the
// operator can configure the git host and later enforce signatures.
func createTriggerHandler(triggers trigger.Store, templates template.Store, creds credential.Store,
	sealer *credential.Sealer, authz *authorizer, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if triggers == nil || templates == nil {
			respondError(w, log, http.StatusNotFound, "triggers not enabled")
			return
		}
		var req createTriggerRequest
		if !decodeStrict(w, log, r.Body, &req) {
			return
		}
		if req.Name == "" || req.TemplateID == "" {
			respondError(w, log, http.StatusBadRequest, "name and template_id are required")
			return
		}
		if req.Review != nil && (sealer == nil || !sealer.Enabled()) {
			respondError(w, log, http.StatusConflict,
				"review needs an encryption key, because a review trigger always verifies its "+
					"deliveries: set SWITCHTENDER_ENCRYPTION_KEY and SWITCHTENDER_ENCRYPTION_SALT on "+
					"the server")
			return
		}
		if req.RequireSignature && (sealer == nil || !sealer.Enabled()) {
			respondError(w, log, http.StatusConflict,
				"require_signature needs an encryption key: set SWITCHTENDER_ENCRYPTION_KEY and SWITCHTENDER_ENCRYPTION_SALT on the server")
			return
		}
		// A webhook fires a template with nobody present, so writing one has to authorize the
		// template it will fire. Schedules already check this for the same reason; triggers did not,
		// so an operator refused a template could wrap it in a webhook and run it anyway. The hook
		// itself carries no identity at fire time, which makes the trigger a durable re-entry point
		// into that template: it survives its author's demotion, and there is nothing to revoke.
		if denyOnAuthzError(w, log,
			authz.authorizeAll(r.Context(), grant.AccessUse, req.TemplateID)) {
			return
		}
		tpl, err := templates.Get(r.Context(), req.TemplateID)
		if errors.Is(err, template.ErrNotFound) {
			respondError(w, log, http.StatusBadRequest, "template not found")
			return
		}
		if err != nil {
			log.Error("server: get template: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not create trigger")
			return
		}
		if req.Review != nil && !checkReviewConfig(w, r, req.Review, tpl, creds, authz, log) {
			return
		}

		plain, tg, err := trigger.New(req.Name, req.TemplateID)
		if err != nil {
			log.Error("server: mint trigger: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not create trigger")
			return
		}
		tg.RequireSignature = req.RequireSignature
		if req.Review != nil {
			// A review trigger fetches and runs whatever ref its event names, so an unverified
			// delivery is never acceptable on one, whatever the request asked for.
			tg.Review = req.Review
			tg.RequireSignature = true
		}
		// Recorded so an offboarding review can find the webhooks somebody set up. A trigger's token
		// is a bearer credential belonging to the trigger rather than to a person, so removing an
		// account does not revoke it, and the record is what makes those findable and rotatable.
		tg.CreatedBy = actorName(r)

		var secret string
		if sealer != nil && sealer.Enabled() {
			secret, err = sealNewSigningSecret(sealer, tg)
			if err != nil {
				log.Error("server: seal signing secret: " + err.Error())
				respondError(w, log, http.StatusInternalServerError, "could not create trigger")
				return
			}
		}
		if err := triggers.Save(r.Context(), tg); err != nil {
			log.Error("server: save trigger: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not create trigger")
			return
		}
		respondJSON(w, log, http.StatusCreated,
			createTriggerResponse{Trigger: tg, WebhookPath: "/hooks/" + plain, SigningSecret: secret}, wantsPretty(r))
	}
}

// sealNewSigningSecret mints a signing secret, seals it into tg.SigningSecret, and returns the
// plaintext to show the caller once. The sealer must be enabled.
func sealNewSigningSecret(sealer *credential.Sealer, tg *trigger.Trigger) (string, error) {
	secret, err := trigger.NewSigningSecret()
	if err != nil {
		return "", err
	}
	sealed, err := sealer.Seal(secret)
	if err != nil {
		return "", err
	}
	tg.SigningSecret = sealed
	return secret, nil
}

// updateTriggerRequest is the JSON body accepted by PUT /triggers/{id}.
type updateTriggerRequest struct {
	// Name labels the trigger. Required.
	Name string `json:"name"`
	// RequireSignature toggles HMAC enforcement. Enabling it needs a signing secret to already
	// exist on the trigger, so rotate one first if the trigger was created without encryption.
	// It is a pointer because an update that omits it must leave the setting alone. As a plain
	// bool, a client doing the rename the API documents sent a body with no require_signature and
	// turned webhook signature verification off, silently, on a trigger that had it on.
	RequireSignature *bool `json:"require_signature,omitempty"`
	// Review replaces a review trigger's configuration when set, and is left alone when omitted. It
	// cannot turn a push trigger into a review trigger or back, since the two answer the same
	// webhook differently and the forge was configured for one of them.
	Review *trigger.Review `json:"review,omitempty"`
}

// updateTriggerHandler renames a trigger, toggles signature enforcement, and replaces a review
// trigger's configuration.
func updateTriggerHandler(triggers trigger.Store, templates template.Store, creds credential.Store,
	authz *authorizer, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if triggers == nil {
			respondError(w, log, http.StatusNotFound, "triggers not enabled")
			return
		}
		var req updateTriggerRequest
		if !decodeStrict(w, log, r.Body, &req) {
			return
		}
		if req.Name == "" {
			respondError(w, log, http.StatusBadRequest, "name is required")
			return
		}
		tg, err := triggers.Get(r.Context(), r.PathValue("id"))
		if errors.Is(err, trigger.ErrNotFound) {
			respondError(w, log, http.StatusNotFound, "trigger not found")
			return
		}
		if err != nil {
			log.Error("server: get trigger: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not read trigger")
			return
		}
		// Changing a trigger is changing what a webhook fires and how it is checked, so the caller
		// has to be somebody who may use the template behind it. Without this any operator could
		// turn signature enforcement off on somebody else's trigger, rotate its secret out from
		// under the git host, or delete it.
		if denyOnAuthzError(w, log,
			authz.authorizeAll(r.Context(), grant.AccessUse, tg.TemplateID)) {
			return
		}
		if req.RequireSignature != nil && *req.RequireSignature && tg.SigningSecret == "" {
			respondError(w, log, http.StatusConflict,
				"cannot require signatures without a signing secret: rotate one first")
			return
		}
		if tg.Review != nil && req.RequireSignature != nil && !*req.RequireSignature {
			respondError(w, log, http.StatusConflict,
				"a review trigger always verifies its deliveries, so its signature cannot be turned off")
			return
		}
		if req.Review != nil {
			if tg.Review == nil {
				respondError(w, log, http.StatusConflict,
					"a push trigger cannot become a review trigger: create a review trigger instead")
				return
			}
			tpl, err := templates.Get(r.Context(), tg.TemplateID)
			if err != nil {
				respondError(w, log, http.StatusConflict, "trigger template is gone")
				return
			}
			if !checkReviewConfig(w, r, req.Review, tpl, creds, authz, log) {
				return
			}
			tg.Review = req.Review
		}
		tg.Name = req.Name
		if req.RequireSignature != nil {
			tg.RequireSignature = *req.RequireSignature
		}
		if err := triggers.Save(r.Context(), tg); err != nil {
			log.Error("server: update trigger: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not update trigger")
			return
		}
		respondJSON(w, log, http.StatusOK, tg, wantsPretty(r))
	}
}

// rotateTriggerSecretResponse returns the freshly minted signing secret once.
type rotateTriggerSecretResponse struct {
	// Trigger is the updated record.
	Trigger *trigger.Trigger `json:"trigger"`
	// SigningSecret is the new HMAC secret to set on the git host, shown once.
	SigningSecret string `json:"signing_secret"`
}

// rotateTriggerSecretHandler mints and seals a new signing secret for a trigger, returning the
// plaintext once. It needs the server encryption key.
func rotateTriggerSecretHandler(triggers trigger.Store, sealer *credential.Sealer,
	authz *authorizer, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if triggers == nil {
			respondError(w, log, http.StatusNotFound, "triggers not enabled")
			return
		}
		if sealer == nil || !sealer.Enabled() {
			respondError(w, log, http.StatusConflict,
				"signing secrets need an encryption key: set SWITCHTENDER_ENCRYPTION_KEY and SWITCHTENDER_ENCRYPTION_SALT on the server")
			return
		}
		tg, err := triggers.Get(r.Context(), r.PathValue("id"))
		if errors.Is(err, trigger.ErrNotFound) {
			respondError(w, log, http.StatusNotFound, "trigger not found")
			return
		}
		if err != nil {
			log.Error("server: get trigger: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not read trigger")
			return
		}
		// The same rule as writing one: rotating a secret breaks every delivery the git host is
		// configured for, and deleting one silently stops a deployment path.
		if denyOnAuthzError(w, log,
			authz.authorizeAll(r.Context(), grant.AccessUse, tg.TemplateID)) {
			return
		}
		secret, err := sealNewSigningSecret(sealer, tg)
		if err != nil {
			log.Error("server: seal signing secret: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not rotate secret")
			return
		}
		if err := triggers.Save(r.Context(), tg); err != nil {
			log.Error("server: save trigger: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not rotate secret")
			return
		}
		respondJSON(w, log, http.StatusOK,
			rotateTriggerSecretResponse{Trigger: tg, SigningSecret: secret}, wantsPretty(r))
	}
}

// listTriggersHandler returns all triggers without their tokens.
func listTriggersHandler(triggers trigger.Store, authz *authorizer, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if triggers == nil {
			respondError(w, log, http.StatusNotFound, "triggers not enabled")
			return
		}
		list, err := triggers.List(r.Context())
		if err != nil {
			log.Error("server: list triggers: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not list triggers")
			return
		}
		// A trigger is visible on the same test that governs writing and deleting one, its template.
		// Reading was unauthorized, so any operator could enumerate every webhook launch point on
		// the install and the template each one fires.
		restricted, err := restrictedReader(r.Context(), authz)
		if err != nil {
			log.Error("server: read filter: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not list triggers")
			return
		}
		if restricted {
			kept := make([]*trigger.Trigger, 0, len(list))
			for _, tg := range list {
				if authz.authorizeAll(r.Context(), grant.AccessUse, tg.TemplateID) == nil {
					kept = append(kept, tg)
				}
			}
			list = kept
		}
		capped, total := cappedList(list)
		respondJSON(w, log, http.StatusOK,
			listTriggersResponse{Triggers: capped, Count: len(capped), Total: total}, wantsPretty(r))
	}
}

// deleteTriggerHandler removes a trigger, revoking its webhook.
func deleteTriggerHandler(triggers trigger.Store, authz *authorizer, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if triggers == nil {
			respondError(w, log, http.StatusNotFound, "triggers not enabled")
			return
		}
		// Read first so the template behind it can be authorized. Deleting a trigger silently stops
		// a deployment path, so it is not something any operator may do to anybody's webhook.
		tg, gerr := triggers.Get(r.Context(), r.PathValue("id"))
		if errors.Is(gerr, trigger.ErrNotFound) {
			respondError(w, log, http.StatusNotFound, "trigger not found")
			return
		}
		if gerr != nil {
			log.Error("server: get trigger: " + gerr.Error())
			respondError(w, log, http.StatusInternalServerError, "could not read trigger")
			return
		}
		if denyOnAuthzError(w, log,
			authz.authorizeAll(r.Context(), grant.AccessUse, tg.TemplateID)) {
			return
		}
		err := triggers.Delete(r.Context(), r.PathValue("id"))
		if errors.Is(err, trigger.ErrNotFound) {
			respondError(w, log, http.StatusNotFound, "trigger not found")
			return
		}
		if err != nil {
			log.Error("server: delete trigger: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not delete trigger")
			return
		}
		respondJSON(w, log, http.StatusOK, map[string]string{"deleted": r.PathValue("id")}, wantsPretty(r))
	}
}

// hookHandler fires a trigger from an inbound webhook. The secret token in the path identifies the
// trigger, so this endpoint is public; an unknown token is a plain not found. When the trigger
// requires a signature, the body's X-Hub-Signature-256 must verify against the sealed per-trigger
// secret before anything launches. The launched template syncs its project fresh, so the run
// executes the commit that was just pushed.
func hookHandler(triggers trigger.Store, templates template.Store, submitter Submitter,
	store run.Store, sealer *credential.Sealer, audits audit.Store, reviews *review.Reporter,
	hooks *hookFlights, log *zap.Logger) http.HandlerFunc {
	if hooks == nil {
		panic("hookHandler: hook flights required")
	}
	// The hook endpoint carries no credential but the path, so anybody who can reach the port can
	// present a guess, and a wrong token answers differently from a right token with a bad
	// signature. That is a usable oracle at unlimited rate. The window is generous, because a busy
	// forge legitimately delivers in bursts, and it is per address so one noisy sender cannot stop
	// another's deliveries.
	limiter := &loginLimiter{windows: make(map[string]*loginWindow), max: hookWindowMax}
	return func(w http.ResponseWriter, r *http.Request) {
		if !limiter.allow("hook:" + clientAddr(r)) {
			respondError(w, log, http.StatusTooManyRequests, "too many webhook deliveries, slow down")
			return
		}
		if triggers == nil || templates == nil {
			respondError(w, log, http.StatusNotFound, "triggers not enabled")
			return
		}
		tg, err := triggers.FindByTokenHash(r.Context(), trigger.HashToken(r.PathValue("token")))
		if err != nil {
			respondError(w, log, http.StatusNotFound, "unknown webhook")
			return
		}
		if tg.Review != nil {
			serveReviewHook(w, r, tg, reviewHookDeps{
				templates: templates, triggers: triggers, submitter: submitter, store: store,
				sealer: sealer, audits: audits, reviews: reviews, hooks: hooks, log: log,
			})
			return
		}
		var signed []byte
		if tg.RequireSignature {
			var ok bool
			if signed, ok = verifyHookSignature(w, r, tg, sealer, log); !ok {
				return
			}
		}
		// refuse answers an authenticated delivery that starts no run and records why on the trigger,
		// so an operator reading the trigger sees what the sender was told. A delivery that failed its
		// signature is not recorded: anybody holding the URL could send one.
		refuse := func(status int, msg string) {
			noteTriggerError(r.Context(), triggers, tg.ID, msg, log)
			respondError(w, log, status, msg)
		}
		t, err := templates.Get(r.Context(), tg.TemplateID)
		if err != nil {
			refuse(http.StatusConflict, "trigger template is gone")
			return
		}

		opts := t.LaunchOptions()
		// The run belongs to the template's organization. A hook is unauthenticated, so there is no
		// actor to stamp the org from the way an authenticated launch does, and a run left with no
		// org is scoped by nothing: under strict grants the tenant that owns the trigger cannot see
		// the runs their own webhook fired, while nothing else can either.
		if t.OrgID != "" {
			opts = append(opts, run.WithOrgID(t.OrgID))
		}
		// A webhook is delivered at least once. GitHub and its peers redeliver on a timeout or a
		// non-2xx, and without a key a redelivery of the same event fires a second real run.
		//
		// The delivery identifies the event, not the trigger. When it carries a delivery id the
		// dedupe keys on a stable hash of the trigger and that id with no time component, so a
		// redelivery or a captured replay collapses onto the first run no matter how much later it
		// lands. Keying on a time bucket was wrong in both directions: two different pushes seconds
		// apart collapsed into one run, so the second commit silently never deployed and the git host
		// was told it had, while a replay outside the bucket fired a fresh run every time because the
		// bucket had moved on. A sender that supplies no delivery id falls back to the bounded time
		// bucket keyed on the trigger, the old behavior and the best that can be done without one.
		existing, key, err := resolveHookDedupe(r.Context(), store, tg.ID,
			hookDelivery(r, signed), time.Now())
		if err != nil {
			log.Error("server: resolve trigger dedupe: " + err.Error())
			refuse(http.StatusInternalServerError, "could not fire the trigger")
			return
		}
		if existing != nil {
			// A sender redelivers precisely because it lost the first response, so this is the one
			// delivery that most needs the receipt. The run already holds the receipt of the fire
			// that created it, so the header answers with that rather than with nothing.
			if existing.AuditReceipt != "" {
				w.Header().Set(AuditReceiptHeader, existing.AuditReceipt)
			}
			respondRun(w, r, log, http.StatusAccepted, existing)
			return
		}

		// Nobody answers a webhook's survey, so every question takes its default and a required one
		// with no usable default refuses the delivery. It is recorded in place of a fire, on the
		// chain and on the trigger, and the sender is told why, rather than a run starting without
		// an answer its template says it needs.
		survey, err := t.UnattendedOptions()
		if err != nil {
			reason, aerr := recordUnansweredHook(w, r, audits, tg, t, err)
			if aerr != nil {
				log.Error("server: record webhook refusal: " + aerr.Error())
				refuse(http.StatusServiceUnavailable,
					"refused: the webhook could not be recorded in the audit trail")
				return
			}
			refuse(http.StatusConflict, reason)
			return
		}
		opts = append(opts, survey...)

		// Recorded before anything launches and fail-closed, the same ordering the authenticated
		// middleware uses for a mutation: a webhook fire that cannot be written to the tamper-evident
		// chain does not happen. Recording after Submit left a launched run with no chain entry
		// whenever the audit store was unhealthy, the exact condition the middleware fail-closes on
		// with 503. The trigger is known here, so the entry says which webhook fired; the run does not
		// exist yet, and a pre-execution entry needs no run id.
		// A hook bypasses the gate, so this is the only place its fire is recorded, and the run it
		// creates has to carry that entry's receipt like any other. Without it an unattended,
		// webhook-driven change is the one kind of run whose evidence names no authorization, which
		// is exactly the kind an auditor asks about first.
		ctx := r.Context()
		if audits != nil {
			entry := &audit.Entry{
				ID: audit.NewID(), Actor: "webhook:" + tg.ID,
				Method: http.MethodPost, Path: "/hooks/" + tg.ID + "/fired",
			}
			if aerr := audits.Append(ctx, entry); aerr != nil {
				log.Error("server: record webhook fire: " + aerr.Error())
				refuse(http.StatusServiceUnavailable,
					"refused: the webhook fire could not be recorded in the audit trail")
				return
			}
			// The fire is a recorded mutation like any other, so the sender gets its receipt
			// back and can later prove the delivery was recorded.
			receipt := audit.Receipt(entry)
			w.Header().Set(AuditReceiptHeader, receipt)
			ctx = run.WithAuditReceipt(ctx, receipt)
		}

		opts = append(opts, run.WithIdempotencyKey(key),
			run.WithSource("trigger", tg.ID), run.WithActor("trigger "+tg.Name),
			run.WithActorType("webhook"))
		// The launch can take longer than a sender waits for an answer, since the gate may download
		// the modules a Terraform or OpenTofu configuration calls before it records the run. It runs
		// apart from this request: the sender is answered when it finishes or when the bound runs
		// out, whichever comes first. The fire is already on the chain, a redelivery joins the launch
		// in progress or collapses onto the run it made, and a failure after the sender was answered
		// is recorded on the chain, since nobody was told.
		ctx = context.WithoutCancel(ctx)
		answer, done := hooks.run(tg.ID+"\x00"+key, log,
			func() hookAnswer { return fireTemplate(ctx, triggers, submitter, audits, t, tg, opts, log) },
			func(a hookAnswer) { recordUnsent(ctx, audits, log, tg, "/hooks/"+tg.ID+"/failed", a) })
		if !done {
			hookAnswer{status: http.StatusAccepted, body: map[string]string{
				"trigger":  tg.ID,
				"accepted": "the run is being prepared and launches once the gate has read it",
			}}.write(w, r, log)
			return
		}
		answer.write(w, r, log)
	}
}

// fireTemplate launches a push trigger's template and returns what the sender is told. It runs
// apart from the request that delivered the event, so it writes nothing to that request.
func fireTemplate(ctx context.Context, triggers trigger.Store, submitter Submitter, audits audit.Store,
	t *template.Template, tg *trigger.Trigger, opts []run.SubmitOption, log *zap.Logger) hookAnswer {
	// A delivery that starts no run says why on the trigger, which is what an operator reads to learn
	// why a webhook stopped launching, whether or not the sender was still waiting to be told.
	refuse := func(status int, msg string) hookAnswer {
		noteTriggerError(ctx, triggers, tg.ID, msg, log)
		return hookAnswer{status: status, message: msg}
	}
	var created *run.Run
	var err error
	switch {
	case len(t.Steps) > 0:
		// A webhook that fires a workflow template runs its graph. The idempotency key still
		// applies, so a redelivered webhook does not fire the workflow twice.
		created, err = submitter.SubmitPipeline(ctx, t.Name, t.Inventory, t.Steps, opts...)
	case t.Shards >= 2:
		created, err = submitter.SubmitSplit(ctx, t.Playbook, t.Inventory, t.Shards, opts...)
	default:
		created, err = submitter.Submit(ctx, t.Playbook, t.Inventory, opts...)
	}
	// A fire whose composed inventory matched no hosts is skipped, not failed, and the sender is
	// told so with a success: there is nothing to retry.
	if errors.Is(err, inventory.ErrNoHosts) {
		return hookSkipAnswer(ctx, log, audits, tg, t)
	}
	if errors.Is(err, dispatch.ErrPolicyDenied) ||
		errors.Is(err, dispatch.ErrQueueUnlicensed) {
		return refuse(http.StatusForbidden, err.Error())
	}
	// The sender cannot fix a credential, but whoever reads its delivery log can, and "could not
	// launch the template" sent them to the server log to find out which one and why.
	if errors.Is(err, credential.ErrNoSecret) || errors.Is(err, credential.ErrUnreadable) {
		log.Warn("server: fire trigger: " + err.Error())
		return refuse(http.StatusConflict, err.Error())
	}
	if err != nil {
		log.Error("server: fire trigger: " + err.Error())
		return refuse(http.StatusBadGateway, "could not launch the template")
	}
	// The stamp is an update by id, never a whole-row save. This handler holds a snapshot
	// loaded before the launch, and writing it back resurrected triggers deleted mid-flight
	// and reverted secret rotations that raced a fire: deletion is revocation, and a
	// revocation a stale fire can undo is not one.
	if err := triggers.TouchFired(ctx, tg.ID, time.Now()); err != nil {
		log.Error("server: stamp trigger: " + err.Error())
	}
	return hookAnswer{status: http.StatusAccepted,
		body: map[string]string{"trigger": tg.ID, "run": created.ID}}
}

// hookWindowMax bounds webhook deliveries per client address per minute. It is far looser than the
// sign-in limit because a forge legitimately delivers in bursts, and far tighter than unbounded,
// which is what an endpoint reachable without a credential had.
const hookWindowMax = 120

// hookDelivery returns a suffix identifying this delivery, or empty when there is nothing to
// identify it by.
//
// When the trigger requires a signature the suffix comes from the verified body, because that is the
// only part of the request the signature covers. Keying on the delivery header instead made the
// signature worth nothing against replay: the header sits outside the signed material, so anyone
// holding one captured delivery, which a repository admin can read straight out of the forge's
// "Recent Deliveries" pane, could resend the same signed body under a new header and launch a real
// deployment again, as many times as they liked. A sender whose body never varies now collapses onto
// one run, which is the honest answer: two deliveries this install cannot tell apart are two
// deliveries it must not treat as separate events.
//
// Without a signature there is nothing trustworthy to key on, so the delivery header is used as
// before. The headers are the ones the common forges send: GitHub and Gitea use X-GitHub-Delivery,
// GitLab uses X-Gitlab-Event-UUID, and Bitbucket uses X-Request-UUID. A value is hashed rather than
// used directly, so a sender cannot steer the key into another trigger's bucket.
func hookDelivery(r *http.Request, signed []byte) string {
	if len(signed) > 0 {
		sum := sha256.Sum256(signed)
		return ":body:" + hex.EncodeToString(sum[:8])
	}
	for _, h := range []string{"X-GitHub-Delivery", "X-Gitlab-Event-UUID", "X-Request-UUID"} {
		if v := r.Header.Get(h); v != "" {
			sum := sha256.Sum256([]byte(v))
			return ":" + hex.EncodeToString(sum[:8])
		}
	}
	return ""
}

// stableHookEpoch is the fixed instant a webhook delivery's dedupe key is derived at, so the key
// carries no live time bucket and a redelivery collapses onto the first run however late it lands.
// Only its constancy matters; the value is otherwise arbitrary. Deriving the key through
// run.DedupeKey keeps it inside the server's reserved idempotency namespace, which ClientKey refuses
// to mint, so a caller cannot plant a run under the key a later delivery would compute.
var stableHookEpoch = time.Unix(0, 0).UTC()

// resolveHookDedupe returns the run a repeat of this delivery already created, nil when there is
// none, and the idempotency key a fresh run must carry.
//
// With a delivery id the key is stable, hashing the trigger and the delivery with no time component,
// so a replay is idempotent regardless of timing and the store's unique index collapses a concurrent
// redelivery onto one run. Without a delivery id it falls back to the bounded time-bucketed dedupe
// keyed on the trigger, the old behavior and the best available without an id.
func resolveHookDedupe(ctx context.Context, store run.Store, triggerID, delivery string,
	now time.Time) (*run.Run, string, error) {
	if delivery == "" {
		return run.ResolveDedupe(ctx, store, "trigger", triggerID, now)
	}
	key := run.DedupeKey("trigger", triggerID+delivery, stableHookEpoch)
	existing, err := store.ByIdempotencyKey(ctx, key)
	if errors.Is(err, run.ErrNotFound) {
		return nil, key, nil
	}
	if err != nil {
		return nil, "", err
	}
	return existing, key, nil
}

// verifyHookSignature reads the request body and checks its X-Hub-Signature-256 against the
// trigger's sealed signing secret. It writes the error response and returns false when the
// signature is missing, wrong, or cannot be checked; on success it returns the bytes it verified
// and true. A trigger that requires a signature but has no usable secret is a server
// misconfiguration, not a client error.
//
// The verified bytes are returned because they are the only part of the request the signature
// covers, and the dedupe key has to be derived from them rather than from a header anyone can change.
func verifyHookSignature(w http.ResponseWriter, r *http.Request, tg *trigger.Trigger, sealer *credential.Sealer, log *zap.Logger) ([]byte, bool) {
	if sealer == nil || !sealer.Enabled() || tg.SigningSecret == "" {
		log.Error("server: trigger requires a signature but no signing secret is available: " + tg.ID)
		respondError(w, log, http.StatusInternalServerError, "signature verification unavailable")
		return nil, false
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxHookBody))
	if err != nil {
		respondError(w, log, http.StatusRequestEntityTooLarge, "webhook body too large")
		return nil, false
	}
	secret, err := sealer.Open(tg.SigningSecret)
	if err != nil {
		log.Error("server: open signing secret: " + err.Error())
		respondError(w, log, http.StatusInternalServerError, "signature verification unavailable")
		return nil, false
	}
	if !trigger.VerifySignature(secret, body, r.Header.Get("X-Hub-Signature-256")) {
		respondError(w, log, http.StatusUnauthorized, "invalid webhook signature")
		return nil, false
	}
	return body, true
}

// checkReviewConfig validates a review configuration for template tpl and writes the refusal when
// it does not hold: the configuration itself, a template a plan can run, and a token credential the
// caller may use. Using a credential is authorized like any other object, since the trigger will
// post with it on the caller's behalf long after this request.
func checkReviewConfig(w http.ResponseWriter, r *http.Request, cfg *trigger.Review, tpl *template.Template,
	creds credential.Store, authz *authorizer, log *zap.Logger) bool {
	if err := cfg.Validate(); err != nil {
		respondError(w, log, http.StatusBadRequest, err.Error())
		return false
	}
	if reason := reviewUnsupported(tpl); reason != "" {
		respondError(w, log, http.StatusBadRequest, reason)
		return false
	}
	if denyOnAuthzError(w, log, authz.authorizeAll(r.Context(), grant.AccessUse, cfg.CredentialID)) {
		return false
	}
	if err := review.CheckToken(r.Context(), creds, cfg.CredentialID); err != nil {
		respondError(w, log, http.StatusBadRequest, err.Error())
		return false
	}
	return true
}
