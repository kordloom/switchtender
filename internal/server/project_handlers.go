package server

import (
	"errors"
	"net/http"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/grant"
	"github.com/kordloom/switchtender/internal/project"
)

// createProjectRequest is the JSON body accepted by POST /projects.
type createProjectRequest struct {
	// Name labels the project. Required.
	Name string `json:"name"`
	// RepoURL is the git remote. Required.
	RepoURL string `json:"repo_url"`
	// Branch is the branch to sync. Optional.
	Branch string `json:"branch,omitempty"`
	// CredentialID names an ssh_key credential for private remotes. Optional.
	CredentialID string `json:"credential_id,omitempty"`
	// InstallDeps installs the project's Ansible requirements on each sync. Optional, defaults to
	// true when omitted; set it false to skip dependency installation.
	InstallDeps *bool `json:"install_deps,omitempty"`
	// Image names a container image the project's runs execute inside. Optional.
	Image string `json:"image,omitempty"`
	// PullCredentialID names a registry credential for pulling a private Image. Optional.
	PullCredentialID string `json:"pull_credential_id,omitempty"`
	// OrgID names the owning organization. A pointer so an omitted field keeps the stored owner on
	// an update rather than un-owning the record, and leaves a create unowned. A present empty
	// string is the explicit "move this out of its organization".
	OrgID *string `json:"org_id,omitempty"`
}

// listProjectsResponse wraps the project list.
type listProjectsResponse struct {
	// Projects is the ordered list.
	Projects []*project.Project `json:"projects"`
	// Count is the number returned.
	Count int `json:"count"`
}

// createProjectHandler stores a new project.
func createProjectHandler(store project.Store, authz *authorizer, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if store == nil {
			respondError(w, log, http.StatusNotFound, "projects not enabled")
			return
		}
		var req createProjectRequest
		if !decodeStrict(w, log, r.Body, &req) {
			return
		}
		if req.Name == "" || req.RepoURL == "" {
			respondError(w, log, http.StatusBadRequest, "name and repo_url are required")
			return
		}
		if err := project.ValidateRepoURL(req.RepoURL); err != nil {
			respondError(w, log, http.StatusBadRequest, err.Error())
			return
		}
		// A project names the credential its clone authenticates with, so writing one has to
		// authorize that credential the same way a template does. Checking only at launch is the
		// wrong order and, for a project, there was no check at either point: a caller who may
		// manage a project could point it at a repository of their choosing and clone it with a
		// credential they were never granted, then read the result back through the file browser.
		// The organization is checked by membership rather than as an object, because it is not one.
		if authz.denyForeignOrg(w, r, log, orgForCreate(req.OrgID)) {
			return
		}
		if denyOnAuthzError(w, log, authz.authorizeAll(r.Context(), grant.AccessUse,
			req.CredentialID, req.PullCredentialID)) {
			return
		}
		p := &project.Project{
			ID: project.NewID(), Name: req.Name, RepoURL: req.RepoURL,
			Branch: req.Branch, CredentialID: req.CredentialID,
			InstallDeps: req.InstallDeps == nil || *req.InstallDeps,
			Image:       req.Image, PullCredentialID: req.PullCredentialID, OrgID: orgForCreate(req.OrgID),
			CreatedAt: time.Now(),
		}
		if err := store.Save(r.Context(), p); err != nil {
			log.Error("server: save project: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not store project")
			return
		}
		respondJSON(w, log, http.StatusCreated, p, wantsPretty(r))
	}
}

// updateProjectHandler changes an existing project's fields, keeping its id and creation time.
func updateProjectHandler(store project.Store, authz *authorizer, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if store == nil {
			respondError(w, log, http.StatusNotFound, "projects not enabled")
			return
		}
		var req createProjectRequest
		if !decodeStrict(w, log, r.Body, &req) {
			return
		}
		if req.Name == "" || req.RepoURL == "" {
			respondError(w, log, http.StatusBadRequest, "name and repo_url are required")
			return
		}
		if err := project.ValidateRepoURL(req.RepoURL); err != nil {
			respondError(w, log, http.StatusBadRequest, err.Error())
			return
		}
		// A project names the credential its clone authenticates with, so writing one has to
		// authorize that credential the same way a template does. Checking only at launch is the
		// wrong order and, for a project, there was no check at either point: a caller who may
		// manage a project could point it at a repository of their choosing and clone it with a
		// credential they were never granted, then read the result back through the file browser.
		// The organization is checked by membership rather than as an object, because it is not one.
		id := r.PathValue("id")
		// The owner an omitted field resolves to is the stored one, so it has to be read before the
		// organization is checked at all.
		// Only a change of organization is a placement. Asking on every edit refused a manage-delegated
		// caller who is not a member even a rename, while delete asked nothing and succeeded. A project
		// this handler is creating has no stored organization to compare against, so the check applies.
		orgID := orgForCreate(req.OrgID)
		placing := true
		if existing, gerr := store.Get(r.Context(), id); gerr == nil {
			orgID = orgForUpdate(req.OrgID, existing.OrgID)
			placing = existing.OrgID != orgID
			// Moving a project out of an organization is as much a change of who controls it as
			// moving one in, so the organization it leaves is checked too.
			if placing && authz.denyForeignOrg(w, r, log, existing.OrgID) {
				return
			}
		}
		if placing && authz.denyForeignOrg(w, r, log, orgID) {
			return
		}
		if denyOnAuthzError(w, log, authz.authorizeAll(r.Context(), grant.AccessUse,
			req.CredentialID, req.PullCredentialID)) {
			return
		}
		p := &project.Project{
			ID: id, Name: req.Name, RepoURL: req.RepoURL,
			Branch: req.Branch, CredentialID: req.CredentialID,
			InstallDeps: req.InstallDeps == nil || *req.InstallDeps,
			Image:       req.Image, PullCredentialID: req.PullCredentialID, OrgID: orgID,
		}
		err := store.Update(r.Context(), p)
		if errors.Is(err, project.ErrNotFound) {
			respondError(w, log, http.StatusNotFound, "project not found")
			return
		}
		if err != nil {
			log.Error("server: update project: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not update project")
			return
		}
		updated, err := store.Get(r.Context(), id)
		if err != nil {
			log.Error("server: read updated project: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not read project")
			return
		}
		respondJSON(w, log, http.StatusOK, updated, wantsPretty(r))
	}
}

// listProjectsHandler returns the projects the actor may read. Under strict grants a non-admin sees
// only projects a grant lets them read; otherwise the global role governs and all are returned.
func listProjectsHandler(store project.Store, authz *authorizer, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if store == nil {
			respondError(w, log, http.StatusNotFound, "projects not enabled")
			return
		}
		list, err := store.List(r.Context())
		if err != nil {
			log.Error("server: list projects: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not list projects")
			return
		}
		visible, err := filterReadable(r.Context(), authz, list,
			func(p *project.Project) string { return p.ID },
			func(p *project.Project) string { return p.OrgID })
		if err != nil {
			log.Error("server: list projects: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not list projects")
			return
		}
		respondJSON(w, log, http.StatusOK,
			listProjectsResponse{Projects: visible, Count: len(visible)}, wantsPretty(r))
	}
}

// deleteProjectHandler removes a project.
func deleteProjectHandler(store project.Store, refs *refChecker, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if store == nil {
			respondError(w, log, http.StatusNotFound, "projects not enabled")
			return
		}
		id := r.PathValue("id")
		if refs != nil {
			used, err := refs.projectRefs(r.Context(), id)
			if err != nil {
				log.Error("server: project references: " + err.Error())
				respondError(w, log, http.StatusInternalServerError, "could not check project references")
				return
			}
			if !used.empty() {
				respondInUse(w, log, "project in use", used, wantsPretty(r))
				return
			}
		}
		err := store.Delete(r.Context(), id)
		if errors.Is(err, project.ErrNotFound) {
			respondError(w, log, http.StatusNotFound, "project not found")
			return
		}
		if err != nil {
			log.Error("server: delete project: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not delete project")
			return
		}
		respondJSON(w, log, http.StatusOK, map[string]string{"deleted": r.PathValue("id")}, wantsPretty(r))
	}
}
