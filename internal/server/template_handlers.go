package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"go.uber.org/zap"

	"github.com/dcadolph/yardmaster/internal/run"
	"github.com/dcadolph/yardmaster/internal/template"
)

// createTemplateRequest is the JSON body accepted by POST /templates.
type createTemplateRequest struct {
	// Name labels the template. Required.
	Name string `json:"name"`
	// ProjectID sources the playbook from a git project. Optional.
	ProjectID string `json:"project_id,omitempty"`
	// Playbook is the playbook path. Required.
	Playbook string `json:"playbook"`
	// Inventory is the inventory path. Optional.
	Inventory string `json:"inventory,omitempty"`
	// Shards, when two or more, splits launches across that many slices.
	Shards int `json:"shards,omitempty"`
	// CredentialIDs names stored credentials for launches.
	CredentialIDs []string `json:"credential_ids,omitempty"`
	// ExtraVars are injected into every launch.
	ExtraVars map[string]any `json:"extra_vars,omitempty"`
}

// listTemplatesResponse wraps the template list.
type listTemplatesResponse struct {
	// Templates is the ordered list.
	Templates []*template.Template `json:"templates"`
	// Count is the number returned.
	Count int `json:"count"`
}

// createTemplateHandler stores a new template.
func createTemplateHandler(store template.Store, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if store == nil {
			respondError(w, log, http.StatusNotFound, "templates not enabled")
			return
		}
		var req createTemplateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			respondError(w, log, http.StatusBadRequest, "invalid request body")
			return
		}
		if req.Name == "" || req.Playbook == "" {
			respondError(w, log, http.StatusBadRequest, "name and playbook are required")
			return
		}
		t := &template.Template{
			ID: template.NewID(), Name: req.Name, ProjectID: req.ProjectID,
			Playbook: req.Playbook, Inventory: req.Inventory, Shards: req.Shards,
			CredentialIDs: req.CredentialIDs, ExtraVars: req.ExtraVars, CreatedAt: time.Now(),
		}
		if err := store.Save(r.Context(), t); err != nil {
			log.Error("server: save template: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not store template")
			return
		}
		respondJSON(w, log, http.StatusCreated, t, wantsPretty(r))
	}
}

// listTemplatesHandler returns all templates.
func listTemplatesHandler(store template.Store, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if store == nil {
			respondError(w, log, http.StatusNotFound, "templates not enabled")
			return
		}
		list, err := store.List(r.Context())
		if err != nil {
			log.Error("server: list templates: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not list templates")
			return
		}
		respondJSON(w, log, http.StatusOK,
			listTemplatesResponse{Templates: list, Count: len(list)}, wantsPretty(r))
	}
}

// deleteTemplateHandler removes a template.
func deleteTemplateHandler(store template.Store, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if store == nil {
			respondError(w, log, http.StatusNotFound, "templates not enabled")
			return
		}
		err := store.Delete(r.Context(), r.PathValue("id"))
		if errors.Is(err, template.ErrNotFound) {
			respondError(w, log, http.StatusNotFound, "template not found")
			return
		}
		if err != nil {
			log.Error("server: delete template: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not delete template")
			return
		}
		respondJSON(w, log, http.StatusOK, map[string]string{"deleted": r.PathValue("id")}, wantsPretty(r))
	}
}

// launchTemplateHandler submits a run from a saved template in one action.
func launchTemplateHandler(store template.Store, submitter Submitter, log *zap.Logger) http.HandlerFunc {
	if submitter == nil {
		panic("server: launchTemplateHandler: Submitter required")
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if store == nil {
			respondError(w, log, http.StatusNotFound, "templates not enabled")
			return
		}
		t, err := store.Get(r.Context(), r.PathValue("id"))
		if errors.Is(err, template.ErrNotFound) {
			respondError(w, log, http.StatusNotFound, "template not found")
			return
		}
		if err != nil {
			log.Error("server: launch template: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not launch template")
			return
		}

		opts := []run.SubmitOption{
			run.WithCredentialIDs(t.CredentialIDs),
			run.WithExtraVars(t.ExtraVars),
		}
		if t.ProjectID != "" {
			opts = append(opts, run.WithProject(t.ProjectID))
		}
		var created *run.Run
		if t.Shards >= 2 {
			created, err = submitter.SubmitSplit(r.Context(), t.Playbook, t.Inventory, t.Shards, opts...)
		} else {
			created, err = submitter.Submit(r.Context(), t.Playbook, t.Inventory, opts...)
		}
		if err != nil {
			log.Error("server: launch template: " + err.Error())
			respondError(w, log, http.StatusBadRequest, err.Error())
			return
		}
		w.Header().Set("Location", "/runs/"+created.ID)
		respondJSON(w, log, http.StatusAccepted, created, wantsPretty(r))
	}
}
