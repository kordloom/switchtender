package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/grant"
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
}

// createTriggerHandler mints a trigger and returns its webhook path once. When the server has an
// encryption key it also mints a sealed HMAC signing secret and returns the plaintext once, so the
// operator can configure the git host and later enforce signatures.
func createTriggerHandler(triggers trigger.Store, templates template.Store, sealer *credential.Sealer,
	authz *authorizer, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if triggers == nil || templates == nil {
			respondError(w, log, http.StatusNotFound, "triggers not enabled")
			return
		}
		var req createTriggerRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			respondError(w, log, http.StatusBadRequest, "invalid request body")
			return
		}
		if req.Name == "" || req.TemplateID == "" {
			respondError(w, log, http.StatusBadRequest, "name and template_id are required")
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
		if _, err := templates.Get(r.Context(), req.TemplateID); errors.Is(err, template.ErrNotFound) {
			respondError(w, log, http.StatusBadRequest, "template not found")
			return
		}

		plain, tg, err := trigger.New(req.Name, req.TemplateID)
		if err != nil {
			log.Error("server: mint trigger: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not create trigger")
			return
		}
		tg.RequireSignature = req.RequireSignature

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
	RequireSignature bool `json:"require_signature"`
}

// updateTriggerHandler renames a trigger and toggles signature enforcement.
func updateTriggerHandler(triggers trigger.Store, templates template.Store, authz *authorizer,
	log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if triggers == nil {
			respondError(w, log, http.StatusNotFound, "triggers not enabled")
			return
		}
		var req updateTriggerRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			respondError(w, log, http.StatusBadRequest, "invalid request body")
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
		if req.RequireSignature && tg.SigningSecret == "" {
			respondError(w, log, http.StatusConflict,
				"cannot require signatures without a signing secret: rotate one first")
			return
		}
		tg.Name = req.Name
		tg.RequireSignature = req.RequireSignature
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
		restricted, err := grantsEnforced(r.Context(), authz)
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
		respondJSON(w, log, http.StatusOK,
			listTriggersResponse{Triggers: list, Count: len(list)}, wantsPretty(r))
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
	store run.Store, sealer *credential.Sealer, audits audit.Store, log *zap.Logger) http.HandlerFunc {
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
		if tg.RequireSignature && !verifyHookSignature(w, r, tg, sealer, log) {
			return
		}
		t, err := templates.Get(r.Context(), tg.TemplateID)
		if err != nil {
			respondError(w, log, http.StatusConflict, "trigger template is gone")
			return
		}

		opts := t.LaunchOptions()
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
		existing, key, err := resolveHookDedupe(r.Context(), store, tg.ID, hookDelivery(r), time.Now())
		if err != nil {
			log.Error("server: resolve trigger dedupe: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not fire the trigger")
			return
		}
		if existing != nil {
			// A sender redelivers precisely because it lost the first response, so this is the one
			// delivery that most needs the receipt. The run already holds the receipt of the fire
			// that created it, so the header answers with that rather than with nothing.
			if existing.AuditReceipt != "" {
				w.Header().Set(AuditReceiptHeader, existing.AuditReceipt)
			}
			respondJSON(w, log, http.StatusAccepted, existing, wantsPretty(r))
			return
		}

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
				ID: audit.NewID(), At: time.Now(), Actor: "webhook:" + tg.ID,
				Method: http.MethodPost, Path: "/hooks/" + tg.ID + "/fired",
			}
			if aerr := audits.Append(ctx, entry); aerr != nil {
				log.Error("server: record webhook fire: " + aerr.Error())
				respondError(w, log, http.StatusServiceUnavailable,
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
			run.WithSource("trigger", tg.ID), run.WithActor("trigger "+tg.Name))
		var created *run.Run
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
		if err != nil {
			log.Error("server: fire trigger: " + err.Error())
			respondError(w, log, http.StatusBadGateway, "could not launch the template")
			return
		}
		now := time.Now()
		tg.LastFiredAt = &now
		if err := triggers.Save(r.Context(), tg); err != nil {
			log.Error("server: stamp trigger: " + err.Error())
		}
		respondJSON(w, log, http.StatusAccepted,
			map[string]string{"trigger": tg.ID, "run": created.ID}, wantsPretty(r))
	}
}

// hookWindowMax bounds webhook deliveries per client address per minute. It is far looser than the
// sign-in limit because a forge legitimately delivers in bursts, and far tighter than unbounded,
// which is what an endpoint reachable without a credential had.
const hookWindowMax = 120

// hookDelivery returns a suffix identifying this delivery, or empty when the sender supplies none.
//
// The headers are the ones the common forges send: GitHub and Gitea use X-GitHub-Delivery, GitLab
// uses X-Gitlab-Event-UUID, and Bitbucket uses X-Request-UUID. A value is hashed rather than used
// directly, so a sender cannot steer the key into another trigger's bucket by choosing what to send.
func hookDelivery(r *http.Request) string {
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
// signature is missing, wrong, or cannot be checked; on success it returns true. A trigger that
// requires a signature but has no usable secret is a server misconfiguration, not a client error.
func verifyHookSignature(w http.ResponseWriter, r *http.Request, tg *trigger.Trigger, sealer *credential.Sealer, log *zap.Logger) bool {
	if sealer == nil || !sealer.Enabled() || tg.SigningSecret == "" {
		log.Error("server: trigger requires a signature but no signing secret is available: " + tg.ID)
		respondError(w, log, http.StatusInternalServerError, "signature verification unavailable")
		return false
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxHookBody))
	if err != nil {
		respondError(w, log, http.StatusRequestEntityTooLarge, "webhook body too large")
		return false
	}
	secret, err := sealer.Open(tg.SigningSecret)
	if err != nil {
		log.Error("server: open signing secret: " + err.Error())
		respondError(w, log, http.StatusInternalServerError, "signature verification unavailable")
		return false
	}
	if !trigger.VerifySignature(secret, body, r.Header.Get("X-Hub-Signature-256")) {
		respondError(w, log, http.StatusUnauthorized, "invalid webhook signature")
		return false
	}
	return true
}
