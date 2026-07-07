package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"go.uber.org/zap"

	"github.com/dcadolph/yardmaster/internal/run"
	"github.com/dcadolph/yardmaster/internal/template"
	"github.com/dcadolph/yardmaster/internal/trigger"
)

// createTriggerRequest is the JSON body accepted by POST /triggers.
type createTriggerRequest struct {
	// Name labels the trigger. Required.
	Name string `json:"name"`
	// TemplateID is the template the trigger launches. Required.
	TemplateID string `json:"template_id"`
}

// createTriggerResponse returns the trigger and its webhook path, shown once.
type createTriggerResponse struct {
	// Trigger is the stored record.
	Trigger *trigger.Trigger `json:"trigger"`
	// WebhookPath is where a git host posts to fire the trigger. Point the remote at this path
	// on your server; the secret token in it is not recoverable later.
	WebhookPath string `json:"webhook_path"`
}

// listTriggersResponse wraps the trigger list.
type listTriggersResponse struct {
	// Triggers is the ordered list.
	Triggers []*trigger.Trigger `json:"triggers"`
	// Count is the number returned.
	Count int `json:"count"`
}

// createTriggerHandler mints a trigger and returns its webhook path once.
func createTriggerHandler(triggers trigger.Store, templates template.Store, log *zap.Logger) http.HandlerFunc {
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
		if err := triggers.Save(r.Context(), tg); err != nil {
			log.Error("server: save trigger: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not create trigger")
			return
		}
		respondJSON(w, log, http.StatusCreated,
			createTriggerResponse{Trigger: tg, WebhookPath: "/hooks/" + plain}, wantsPretty(r))
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

// hookHandler fires a trigger from an inbound webhook. The secret token in the path is the only
// authentication, so this endpoint is public; an unknown token is a plain not found. The launched
// template syncs its project fresh, so the run executes the commit that was just pushed.
func hookHandler(triggers trigger.Store, templates template.Store, submitter Submitter, log *zap.Logger) http.HandlerFunc {
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
		t, err := templates.Get(r.Context(), tg.TemplateID)
		if err != nil {
			respondError(w, log, http.StatusConflict, "trigger template is gone")
			return
		}

		opts := []run.SubmitOption{run.WithCredentialIDs(t.CredentialIDs), run.WithExtraVars(t.ExtraVars)}
		if t.ProjectID != "" {
			opts = append(opts, run.WithProject(t.ProjectID))
		}
		if t.Queue != "" {
			opts = append(opts, run.WithQueue(t.Queue))
		}
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
