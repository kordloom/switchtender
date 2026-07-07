package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"go.uber.org/zap"

	"github.com/dcadolph/yardmaster/internal/inventory"
	"github.com/dcadolph/yardmaster/internal/invsource"
)

// SourceRefresher renders an inventory source into its backing inventory. The dispatcher
// satisfies it.
type SourceRefresher interface {
	RefreshSource(ctx context.Context, id string) (*invsource.Source, error)
}

// createSourceRequest is the JSON body accepted by POST /inventory-sources.
type createSourceRequest struct {
	// Name labels the source. Required.
	Name string `json:"name"`
	// Source is the ansible-inventory argument: a plugin config, script, or directory. Required.
	Source string `json:"source"`
	// CredentialID names an env credential for the plugin. Optional.
	CredentialID string `json:"credential_id,omitempty"`
	// ProjectID sources the config from a git project. Optional.
	ProjectID string `json:"project_id,omitempty"`
}

// listSourcesResponse wraps the source list.
type listSourcesResponse struct {
	// Sources is the ordered list.
	Sources []*invsource.Source `json:"sources"`
	// Count is the number returned.
	Count int `json:"count"`
}

// createSourceHandler stores a new source and creates the inventory it maintains.
func createSourceHandler(sources invsource.Store, inventories inventory.Store, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if sources == nil || inventories == nil {
			respondError(w, log, http.StatusNotFound, "inventory sources not enabled")
			return
		}
		var req createSourceRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			respondError(w, log, http.StatusBadRequest, "invalid request body")
			return
		}
		if req.Name == "" || req.Source == "" {
			respondError(w, log, http.StatusBadRequest, "name and source are required")
			return
		}
		inv := &inventory.Inventory{
			ID: inventory.NewID(), Name: req.Name + " (dynamic)", Content: "{}", CreatedAt: time.Now(),
		}
		if err := inventories.Save(r.Context(), inv); err != nil {
			log.Error("server: create source inventory: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not store source")
			return
		}
		src := &invsource.Source{
			ID: invsource.NewID(), Name: req.Name, Source: req.Source,
			CredentialID: req.CredentialID, ProjectID: req.ProjectID,
			InventoryID: inv.ID, CreatedAt: time.Now(),
		}
		if err := sources.Save(r.Context(), src); err != nil {
			log.Error("server: save source: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not store source")
			return
		}
		respondJSON(w, log, http.StatusCreated, src, wantsPretty(r))
	}
}

// listSourcesHandler returns all sources.
func listSourcesHandler(sources invsource.Store, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if sources == nil {
			respondError(w, log, http.StatusNotFound, "inventory sources not enabled")
			return
		}
		list, err := sources.List(r.Context())
		if err != nil {
			log.Error("server: list sources: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not list sources")
			return
		}
		respondJSON(w, log, http.StatusOK,
			listSourcesResponse{Sources: list, Count: len(list)}, wantsPretty(r))
	}
}

// deleteSourceHandler removes a source. Its backing inventory is left in place.
func deleteSourceHandler(sources invsource.Store, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if sources == nil {
			respondError(w, log, http.StatusNotFound, "inventory sources not enabled")
			return
		}
		err := sources.Delete(r.Context(), r.PathValue("id"))
		if errors.Is(err, invsource.ErrNotFound) {
			respondError(w, log, http.StatusNotFound, "source not found")
			return
		}
		if err != nil {
			log.Error("server: delete source: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not delete source")
			return
		}
		respondJSON(w, log, http.StatusOK, map[string]string{"deleted": r.PathValue("id")}, wantsPretty(r))
	}
}

// refreshSourceHandler runs the source now and returns its updated sync state.
func refreshSourceHandler(refresher SourceRefresher, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if refresher == nil {
			respondError(w, log, http.StatusNotFound, "inventory sources not enabled")
			return
		}
		src, err := refresher.RefreshSource(r.Context(), r.PathValue("id"))
		if errors.Is(err, invsource.ErrNotFound) {
			respondError(w, log, http.StatusNotFound, "source not found")
			return
		}
		if err != nil {
			// A refresh failure is recorded on the source; report it without a 500 so the caller
			// sees the plugin error.
			respondError(w, log, http.StatusBadGateway, err.Error())
			return
		}
		respondJSON(w, log, http.StatusOK, src, wantsPretty(r))
	}
}
