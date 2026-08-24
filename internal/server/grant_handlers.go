package server

import (
	"errors"
	"net/http"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/grant"
)

// createGrantRequest is the JSON body accepted by POST /grants.
type createGrantRequest struct {
	// Subject is a user id (user_...) or a team id (team_...). Required.
	Subject string `json:"subject"`
	// Object is a project, template, inventory, or credential id, or a worker queue named as
	// queue:<name>. Required.
	Object string `json:"object"`
	// Access is use or manage. Required.
	Access grant.Access `json:"access"`
}

// listGrantsResponse wraps the grant list.
type listGrantsResponse struct {
	// Grants is the ordered list.
	Grants []*grant.Grant `json:"grants"`
	// Count is the number returned.
	Count int `json:"count"`
}

// createGrantHandler stores a new per-object access grant.
func createGrantHandler(store grant.Store, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if store == nil {
			respondError(w, log, http.StatusNotFound, "grants not enabled")
			return
		}
		var req createGrantRequest
		if !decodeStrict(w, log, r.Body, &req) {
			return
		}
		if !grant.ValidSubject(req.Subject) {
			respondError(w, log, http.StatusBadRequest, "subject must be a user_ or team_ id")
			return
		}
		if !grant.ValidObject(req.Object) {
			respondError(w, log, http.StatusBadRequest,
				"object must be a proj_, tpl_, inv_, or cred_ id")
			return
		}
		if !grant.ValidAccess(req.Access) {
			respondError(w, log, http.StatusBadRequest, "access must be use or manage")
			return
		}
		g := &grant.Grant{
			ID: grant.NewID(), Subject: req.Subject, Object: req.Object,
			Access: req.Access, CreatedAt: time.Now(),
		}
		if err := store.Save(r.Context(), g); err != nil {
			log.Error("server: save grant: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not store grant")
			return
		}
		respondJSON(w, log, http.StatusCreated, g, wantsPretty(r))
	}
}

// listGrantsHandler returns all grants.
func listGrantsHandler(store grant.Store, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if store == nil {
			respondError(w, log, http.StatusNotFound, "grants not enabled")
			return
		}
		list, err := store.List(r.Context())
		if err != nil {
			log.Error("server: list grants: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not list grants")
			return
		}
		respondJSON(w, log, http.StatusOK,
			listGrantsResponse{Grants: list, Count: len(list)}, wantsPretty(r))
	}
}

// deleteGrantHandler removes a grant.
func deleteGrantHandler(store grant.Store, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if store == nil {
			respondError(w, log, http.StatusNotFound, "grants not enabled")
			return
		}
		err := store.Delete(r.Context(), r.PathValue("id"))
		if errors.Is(err, grant.ErrNotFound) {
			respondError(w, log, http.StatusNotFound, "grant not found")
			return
		}
		if err != nil {
			log.Error("server: delete grant: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not delete grant")
			return
		}
		respondJSON(w, log, http.StatusOK, map[string]string{"deleted": r.PathValue("id")}, wantsPretty(r))
	}
}
