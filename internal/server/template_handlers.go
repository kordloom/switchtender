package server

import (
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"time"

	"go.uber.org/zap"

	"github.com/dcadolph/switchtender/internal/credential"
	"github.com/dcadolph/switchtender/internal/dispatch"
	"github.com/dcadolph/switchtender/internal/grant"
	"github.com/dcadolph/switchtender/internal/inventory"
	"github.com/dcadolph/switchtender/internal/project"
	"github.com/dcadolph/switchtender/internal/run"
	"github.com/dcadolph/switchtender/internal/template"
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
	// InventoryID names a stored inventory, taking precedence over the path. Optional.
	InventoryID string `json:"inventory_id,omitempty"`
	// Tool selects the execution engine: ansible (default), bash, terraform, or python.
	Tool string `json:"tool,omitempty"`
	// Command is the tool's input for non-Ansible tools: the script for bash and python, the working
	// directory for terraform.
	Command string `json:"command,omitempty"`
	// DryRun runs the tool in its no-change mode when the template launches.
	DryRun bool `json:"dry_run,omitempty"`
	// Shards, when two or more, splits launches across that many slices.
	Shards int `json:"shards,omitempty"`
	// Queue restricts launches to workers serving the queue.
	Queue string `json:"queue,omitempty"`
	// Image names a container image every launch executes inside. Ansible only.
	Image string `json:"image,omitempty"`
	// PullCredentialID names a registry credential for pulling a private Image.
	PullCredentialID string `json:"pull_credential_id,omitempty"`
	// CredentialIDs names stored credentials for launches.
	CredentialIDs []string `json:"credential_ids,omitempty"`
	// ExtraVars are injected into every launch.
	ExtraVars map[string]any `json:"extra_vars,omitempty"`
	// Survey prompts the launcher for typed values that become extra vars.
	Survey []template.SurveyField `json:"survey,omitempty"`
}

// listTemplatesResponse wraps the template list.
type listTemplatesResponse struct {
	// Templates is the ordered list.
	Templates []*template.Template `json:"templates"`
	// Count is the number returned.
	Count int `json:"count"`
}

// templateToolError returns a client message when a template request lacks the input its tool
// needs, or empty when the request is valid. Ansible needs a playbook; other tools need a command.
func templateToolError(req createTemplateRequest) string {
	if req.Name == "" {
		return "name is required"
	}
	if !run.ValidTool(req.Tool) {
		return "tool must be ansible, bash, terraform, opentofu, python, powershell, or go"
	}
	if run.NormalizeTool(req.Tool) == run.ToolAnsible {
		if req.Playbook == "" {
			return "playbook is required"
		}
		return ""
	}
	if req.Image != "" {
		return "an execution image is only supported for the ansible tool"
	}
	if req.Command == "" {
		return "command is required for the " + req.Tool + " tool"
	}
	return ""
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
		if msg := templateToolError(req); msg != "" {
			respondError(w, log, http.StatusBadRequest, msg)
			return
		}
		t := &template.Template{
			ID: template.NewID(), Name: req.Name, ProjectID: req.ProjectID,
			Playbook: req.Playbook, Inventory: req.Inventory, InventoryID: req.InventoryID,
			Tool: req.Tool, Command: req.Command, DryRun: req.DryRun,
			Shards:        req.Shards,
			CredentialIDs: req.CredentialIDs, ExtraVars: req.ExtraVars, Survey: req.Survey,
			Queue: req.Queue, Image: req.Image, PullCredentialID: req.PullCredentialID,
			CreatedAt: time.Now(),
		}
		if err := store.Save(r.Context(), t); err != nil {
			log.Error("server: save template: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not store template")
			return
		}
		respondJSON(w, log, http.StatusCreated, t, wantsPretty(r))
	}
}

// updateTemplateHandler changes an existing template's fields, keeping its id and creation time.
func updateTemplateHandler(store template.Store, log *zap.Logger) http.HandlerFunc {
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
		if msg := templateToolError(req); msg != "" {
			respondError(w, log, http.StatusBadRequest, msg)
			return
		}
		id := r.PathValue("id")
		t := &template.Template{
			ID: id, Name: req.Name, ProjectID: req.ProjectID,
			Playbook: req.Playbook, Inventory: req.Inventory, InventoryID: req.InventoryID,
			Tool: req.Tool, Command: req.Command, DryRun: req.DryRun,
			Shards:        req.Shards,
			CredentialIDs: req.CredentialIDs, ExtraVars: req.ExtraVars, Survey: req.Survey,
			Queue: req.Queue, Image: req.Image, PullCredentialID: req.PullCredentialID,
		}
		err := store.Update(r.Context(), t)
		if errors.Is(err, template.ErrNotFound) {
			respondError(w, log, http.StatusNotFound, "template not found")
			return
		}
		if err != nil {
			log.Error("server: update template: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not update template")
			return
		}
		updated, err := store.Get(r.Context(), id)
		if err != nil {
			log.Error("server: read updated template: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not read template")
			return
		}
		respondJSON(w, log, http.StatusOK, updated, wantsPretty(r))
	}
}

// listTemplatesHandler returns the templates the actor may read. Under strict grants a non-admin sees
// only templates a grant lets them read; otherwise the global role governs and all are returned.
func listTemplatesHandler(store template.Store, authz *authorizer, log *zap.Logger) http.HandlerFunc {
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
		visible, err := filterReadable(r.Context(), authz, list, func(t *template.Template) string { return t.ID })
		if err != nil {
			log.Error("server: list templates: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not list templates")
			return
		}
		respondJSON(w, log, http.StatusOK,
			listTemplatesResponse{Templates: visible, Count: len(visible)}, wantsPretty(r))
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
func launchTemplateHandler(store template.Store, submitter Submitter, authz *authorizer, log *zap.Logger) http.HandlerFunc {
	if submitter == nil {
		panic("server: launchTemplateHandler: Submitter required")
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if store == nil {
			respondError(w, log, http.StatusNotFound, "templates not enabled")
			return
		}
		id := r.PathValue("id")
		if err := authz.authorize(r.Context(), id, grant.AccessUse); err != nil {
			if errors.Is(err, errForbiddenGrant) {
				forbidden(w)
				return
			}
			log.Error("server: authorize launch: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not authorize launch")
			return
		}
		t, err := store.Get(r.Context(), id)
		if errors.Is(err, template.ErrNotFound) {
			respondError(w, log, http.StatusNotFound, "template not found")
			return
		}
		if err != nil {
			log.Error("server: launch template: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not launch template")
			return
		}

		// Use on the template is not enough. Authorize every object the run will touch, so a launch
		// cannot borrow a project, inventory, or credentials the actor was never granted.
		objects := append([]string{t.ProjectID, t.InventoryID, t.PullCredentialID}, t.CredentialIDs...)
		if denyOnAuthzError(w, log, authz.authorizeAll(r.Context(), grant.AccessUse, objects...)) {
			return
		}

		vars := map[string]any{}
		maps.Copy(vars, t.ExtraVars)
		if len(t.Survey) > 0 {
			answers := map[string]any{}
			// Answers are optional in the body; an empty body still validates required-free surveys.
			if r.Body != nil {
				_ = json.NewDecoder(r.Body).Decode(&answers)
			}
			resolved, err := template.ResolveSurvey(t.Survey, answers)
			if err != nil {
				respondError(w, log, http.StatusBadRequest, err.Error())
				return
			}
			maps.Copy(vars, resolved)
		}

		opts := []run.SubmitOption{
			run.WithCredentialIDs(t.CredentialIDs),
			run.WithExtraVars(vars),
			run.WithTool(t.Tool), run.WithCommand(t.Command), run.WithDryRun(t.DryRun),
		}
		if t.ProjectID != "" {
			opts = append(opts, run.WithProject(t.ProjectID))
		}
		if t.InventoryID != "" {
			opts = append(opts, run.WithInventory(t.InventoryID))
		}
		if t.Queue != "" {
			opts = append(opts, run.WithQueue(t.Queue))
		}
		if t.Image != "" {
			opts = append(opts, run.WithImage(t.Image, t.PullCredentialID))
		}
		var created *run.Run
		if t.Shards >= 2 {
			created, err = submitter.SubmitSplit(r.Context(), t.Playbook, t.Inventory, t.Shards, opts...)
		} else {
			created, err = submitter.Submit(r.Context(), t.Playbook, t.Inventory, opts...)
		}
		switch {
		case errors.Is(err, credential.ErrNotFound), errors.Is(err, credential.ErrNoKey),
			errors.Is(err, project.ErrNotFound), errors.Is(err, inventory.ErrNotFound),
			errors.Is(err, dispatch.ErrNoPlaybook), errors.Is(err, dispatch.ErrNoCommand),
			errors.Is(err, dispatch.ErrUnknownTool):
			respondError(w, log, http.StatusBadRequest, err.Error())
			return
		case err != nil:
			log.Error("server: launch template: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not launch template")
			return
		}
		w.Header().Set("Location", "/v1/runs/"+created.ID)
		respondJSON(w, log, http.StatusAccepted, created, wantsPretty(r))
	}
}
