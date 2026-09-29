package server

import (
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"slices"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/dispatch"
	"github.com/kordloom/switchtender/internal/grant"
	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/project"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/template"
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
	// Image names a container image every launch executes inside. Works for every tool.
	Image string `json:"image,omitempty"`
	// PullCredentialID names a registry credential for pulling a private Image.
	PullCredentialID string `json:"pull_credential_id,omitempty"`
	// CredentialIDs names stored credentials for launches.
	CredentialIDs []string `json:"credential_ids,omitempty"`
	// SelectableCredentialIDs names credentials a launch may choose from, applied on top of
	// CredentialIDs. A launch that picks outside this set is rejected. Optional.
	SelectableCredentialIDs []string `json:"selectable_credential_ids,omitempty"`
	// ExtraVars are injected into every launch.
	ExtraVars map[string]any `json:"extra_vars,omitempty"`
	// Survey prompts the launcher for typed values that become extra vars.
	Survey []template.SurveyField `json:"survey,omitempty"`
	// Notifications route every launch's terminal state to specific channels beyond the server-wide
	// ones. Optional.
	Notifications []run.NotifyTarget `json:"notifications,omitempty"`
	// OrgID names the owning organization. Empty leaves the template unowned and global. Optional.
	OrgID string `json:"org_id,omitempty"`
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
// Every tool may pin an execution image, since the container runner builds a plan for all seven.
func templateToolError(req createTemplateRequest) string {
	if req.Name == "" {
		return "name is required"
	}
	if !run.ValidTool(req.Tool) {
		return "tool must be ansible, bash, terraform, opentofu, python, powershell, or go"
	}
	for _, n := range req.Notifications {
		if !run.ValidNotifyKind(n.Kind) || n.URL == "" {
			return "each notification needs a url and a kind of webhook, slack, mattermost, " +
				"rocketchat, discord, teams, or ntfy"
		}
	}
	if run.NormalizeTool(req.Tool) == run.ToolAnsible {
		if req.Playbook == "" {
			return "playbook is required"
		}
		return ""
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
			CredentialIDs: req.CredentialIDs, SelectableCredentialIDs: req.SelectableCredentialIDs,
			ExtraVars: req.ExtraVars, Survey: req.Survey,
			Notifications: req.Notifications,
			Queue:         req.Queue, Image: req.Image, PullCredentialID: req.PullCredentialID,
			OrgID:     req.OrgID,
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
			CredentialIDs: req.CredentialIDs, SelectableCredentialIDs: req.SelectableCredentialIDs,
			ExtraVars: req.ExtraVars, Survey: req.Survey,
			Notifications: req.Notifications,
			Queue:         req.Queue, Image: req.Image, PullCredentialID: req.PullCredentialID,
			OrgID: req.OrgID,
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
		visible, err := filterReadable(r.Context(), authz, list,
			func(t *template.Template) string { return t.ID },
			func(t *template.Template) string { return t.OrgID })
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

// launchTemplateRequest is the optional JSON body for a template launch: survey answers and a chosen
// subset of the template's selectable credentials. Both are optional, so an empty body is valid.
type launchTemplateRequest struct {
	// Answers are survey field values, keyed by the field's var name.
	Answers map[string]any `json:"answers,omitempty"`
	// CredentialIDs is the chosen subset of the template's selectable credentials.
	CredentialIDs []string `json:"credential_ids,omitempty"`
}

// mergeCredentialIDs returns base followed by any extra ids not already present, dropping blanks, so
// a launch's chosen credentials apply on top of the template's always-on set without duplication.
func mergeCredentialIDs(base, extra []string) []string {
	seen := make(map[string]bool, len(base)+len(extra))
	out := make([]string, 0, len(base)+len(extra))
	for _, id := range slices.Concat(base, extra) {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
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

		// Decode the optional launch body: survey answers and a chosen credential subset. An empty
		// body is valid and means no answers and no selection.
		var launchReq launchTemplateRequest
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&launchReq)
		}

		// A launch may choose only from the template's selectable set, so it cannot pull an arbitrary
		// credential. The chosen subset applies on top of the always-on CredentialIDs.
		selectable := make(map[string]bool, len(t.SelectableCredentialIDs))
		for _, sid := range t.SelectableCredentialIDs {
			selectable[sid] = true
		}
		for _, cid := range launchReq.CredentialIDs {
			if !selectable[cid] {
				respondError(w, log, http.StatusBadRequest,
					"credential "+cid+" is not selectable for this template")
				return
			}
		}
		credIDs := mergeCredentialIDs(t.CredentialIDs, launchReq.CredentialIDs)

		// Use on the template is not enough. Authorize every object the run will touch, so a launch
		// cannot borrow a project, inventory, or credential the actor was never granted, including a
		// credential chosen at launch.
		objects := append([]string{t.ProjectID, t.InventoryID, t.PullCredentialID}, credIDs...)
		if denyOnAuthzError(w, log, authz.authorizeAll(r.Context(), grant.AccessUse, objects...)) {
			return
		}

		vars := map[string]any{}
		maps.Copy(vars, t.ExtraVars)
		if len(t.Survey) > 0 {
			resolved, err := template.ResolveSurvey(t.Survey, launchReq.Answers)
			if err != nil {
				respondError(w, log, http.StatusBadRequest, err.Error())
				return
			}
			maps.Copy(vars, resolved)
		}

		opts := []run.SubmitOption{
			run.WithCredentialIDs(credIDs),
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
		if len(t.Notifications) > 0 {
			opts = append(opts, run.WithNotifications(t.Notifications))
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
