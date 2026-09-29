package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/credential"
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
func createTriggerHandler(triggers trigger.Store, templates template.Store, sealer *credential.Sealer, log *zap.Logger) http.HandlerFunc {
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
func updateTriggerHandler(triggers trigger.Store, log *zap.Logger) http.HandlerFunc {
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
func rotateTriggerSecretHandler(triggers trigger.Store, sealer *credential.Sealer, log *zap.Logger) http.HandlerFunc {
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
func listTriggersHandler(triggers trigger.Store, log *zap.Logger) http.HandlerFunc {
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
		respondJSON(w, log, http.StatusOK,
			listTriggersResponse{Triggers: list, Count: len(list)}, wantsPretty(r))
	}
}

// deleteTriggerHandler removes a trigger, revoking its webhook.
func deleteTriggerHandler(triggers trigger.Store, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if triggers == nil {
			respondError(w, log, http.StatusNotFound, "triggers not enabled")
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
	store run.Store, sealer *credential.Sealer, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
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
		// non-2xx, and without a key a redelivery of the same event fires a second real run. The
		// trigger is the thing being repeated, so it names the action.
		existing, key, err := run.ResolveDedupe(r.Context(), store, "trigger", tg.ID, time.Now())
		if err != nil {
			log.Error("server: resolve trigger dedupe: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not fire the trigger")
			return
		}
		if existing != nil {
			respondJSON(w, log, http.StatusAccepted, existing, wantsPretty(r))
			return
		}
		opts = append(opts, run.WithIdempotencyKey(key),
			run.WithSource("trigger", tg.ID), run.WithActor("trigger "+tg.Name))
		var created *run.Run
		if t.Shards >= 2 {
			created, err = submitter.SubmitSplit(r.Context(), t.Playbook, t.Inventory, t.Shards, opts...)
		} else {
			created, err = submitter.Submit(r.Context(), t.Playbook, t.Inventory, opts...)
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
