package server

import (
	"errors"
	"net/http"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/grant"
	"github.com/kordloom/switchtender/internal/project"
)

// projectBrowseTarget resolves and authorizes the project behind a file request, writing the
// response and reporting false when the caller may not browse it.
func projectBrowseTarget(w http.ResponseWriter, r *http.Request, store project.Store,
	syncer *project.Syncer, authz *authorizer, log *zap.Logger) (string, bool) {
	if store == nil || syncer == nil {
		respondError(w, log, http.StatusNotFound, "project files are not enabled")
		return "", false
	}
	id := r.PathValue("id")
	if _, err := store.Get(r.Context(), id); errors.Is(err, project.ErrNotFound) {
		respondError(w, log, http.StatusNotFound, "project not found")
		return "", false
	} else if err != nil {
		log.Error("server: project files: " + err.Error())
		respondError(w, log, http.StatusInternalServerError, "could not read the project")
		return "", false
	}
	// Browsing a checkout reveals its contents, so it needs the same grant a run does.
	if denyOnAuthzError(w, log, authz.authorizeAll(r.Context(), grant.AccessUse, id)) {
		return "", false
	}
	return id, true
}

// respondBrowseError maps a browse failure to its status, keeping refusals indistinguishable so a
// caller cannot map the host filesystem by probing paths.
func respondBrowseError(w http.ResponseWriter, log *zap.Logger, err error) {
	switch {
	case errors.Is(err, project.ErrNoCheckout):
		respondError(w, log, http.StatusNotFound, "this project has not been synced yet, so no files are cached")
	case errors.Is(err, project.ErrNotAFile), errors.Is(err, project.ErrOutsideCheckout):
		respondError(w, log, http.StatusNotFound, "file not found in this project")
	default:
		log.Error("server: project files: " + err.Error())
		respondError(w, log, http.StatusInternalServerError, "could not read project files")
	}
}

// projectTreeHandler lists the files cached in a project's checkout.
func projectTreeHandler(store project.Store, syncer *project.Syncer, authz *authorizer,
	log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := projectBrowseTarget(w, r, store, syncer, authz, log)
		if !ok {
			return
		}
		entries, err := syncer.Tree(id)
		if err != nil {
			respondBrowseError(w, log, err)
			return
		}
		respondJSON(w, log, http.StatusOK, map[string]any{"files": entries}, wantsPretty(r))
	}
}

// projectFileHandler returns one file's content from a project's checkout, read only.
func projectFileHandler(store project.Store, syncer *project.Syncer, authz *authorizer,
	log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := projectBrowseTarget(w, r, store, syncer, authz, log)
		if !ok {
			return
		}
		path := r.URL.Query().Get("path")
		if path == "" {
			respondError(w, log, http.StatusBadRequest, "path is required")
			return
		}
		file, err := syncer.File(id, path)
		if err != nil {
			respondBrowseError(w, log, err)
			return
		}
		respondJSON(w, log, http.StatusOK, file, wantsPretty(r))
	}
}
