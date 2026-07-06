package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"go.uber.org/zap"

	"github.com/dcadolph/yardmaster/internal/project"
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
}

// listProjectsResponse wraps the project list.
type listProjectsResponse struct {
	// Projects is the ordered list.
	Projects []*project.Project `json:"projects"`
	// Count is the number returned.
	Count int `json:"count"`
}

// createProjectHandler stores a new project.
func createProjectHandler(store project.Store, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if store == nil {
			respondError(w, log, http.StatusNotFound, "projects not enabled")
			return
		}
		var req createProjectRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			respondError(w, log, http.StatusBadRequest, "invalid request body")
			return
		}
		if req.Name == "" || req.RepoURL == "" {
			respondError(w, log, http.StatusBadRequest, "name and repo_url are required")
			return
		}
		p := &project.Project{
			ID: project.NewID(), Name: req.Name, RepoURL: req.RepoURL,
			Branch: req.Branch, CredentialID: req.CredentialID, CreatedAt: time.Now(),
		}
		if err := store.Save(r.Context(), p); err != nil {
			log.Error("server: save project: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not store project")
			return
		}
		respondJSON(w, log, http.StatusCreated, p, wantsPretty(r))
	}
}

// listProjectsHandler returns all projects.
func listProjectsHandler(store project.Store, log *zap.Logger) http.HandlerFunc {
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
		respondJSON(w, log, http.StatusOK,
			listProjectsResponse{Projects: list, Count: len(list)}, wantsPretty(r))
	}
}

// deleteProjectHandler removes a project.
func deleteProjectHandler(store project.Store, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if store == nil {
			respondError(w, log, http.StatusNotFound, "projects not enabled")
			return
		}
		err := store.Delete(r.Context(), r.PathValue("id"))
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
