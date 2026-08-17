package server

import (
	"context"
	"errors"
	"net/http"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/grant"
	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/invsource"
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
	// UpdateOnLaunch refreshes the source before a run targeting its inventory. A pointer so an
	// omitted field keeps the stored cadence on an update rather than turning refreshing off: no
	// edit dialog renders these two fields, so a rename silently stopped the source syncing while
	// the table went on claiming it synced. Optional.
	UpdateOnLaunch *bool `json:"update_on_launch,omitempty"`
	// SyncIntervalSeconds sets the background sync cadence and the update-on-launch staleness window.
	// Zero disables scheduled sync. Optional.
	// A pointer for the same reason as UpdateOnLaunch: absent means keep what is stored.
	SyncIntervalSeconds *int `json:"sync_interval_seconds,omitempty"`
}

// listSourcesResponse wraps the source list.
type listSourcesResponse struct {
	// Sources is the ordered list.
	Sources []*invsource.Source `json:"sources"`
	// Count is the number returned.
	Count int `json:"count"`
}

// createSourceHandler stores a new source and creates the inventory it maintains. A source runs a
// project's config and an env credential, so the actor must hold use access on each referenced
// object before it is stored.
func createSourceHandler(sources invsource.Store, inventories inventory.Store, authz *authorizer, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if sources == nil || inventories == nil {
			respondError(w, log, http.StatusNotFound, "inventory sources not enabled")
			return
		}
		var req createSourceRequest
		if !decodeStrict(w, log, r.Body, &req) {
			return
		}
		if req.Name == "" || req.Source == "" {
			respondError(w, log, http.StatusBadRequest, "name and source are required")
			return
		}
		if denyOnAuthzError(w, log,
			authz.authorizeAll(r.Context(), grant.AccessUse, req.ProjectID, req.CredentialID)) {
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
			UpdateOnLaunch: req.UpdateOnLaunch != nil && *req.UpdateOnLaunch,
			SyncIntervalSeconds: intOrZero(req.SyncIntervalSeconds),
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

// updateSourceHandler changes an existing source's editable fields, keeping its backing inventory
// and sync state. Like create, it authorizes the referenced project and credential.
// authorizeStoredSource reads the source named in the path and confirms the caller may use it and
// everything it reaches. It writes the response and returns nil when access is refused.
//
// The rule these three handlers kept breaking is that a handler acting on a stored object must
// authorize the stored object, not the request body. Checking only the body meant a caller could
// take over somebody else's source by omitting its references: nothing was named, so nothing was
// checked. A source decides which hosts a run targets and carries the credential used to fetch
// them, so taking one over redirects production work and borrows a secret in the same step.
func authorizeStoredSource(w http.ResponseWriter, r *http.Request, sources invsource.Store,
	authz *authorizer, log *zap.Logger) *invsource.Source {
	src, err := sources.Get(r.Context(), r.PathValue("id"))
	if errors.Is(err, invsource.ErrNotFound) {
		respondError(w, log, http.StatusNotFound, "source not found")
		return nil
	}
	if err != nil {
		log.Error("server: read inventory source: " + err.Error())
		respondError(w, log, http.StatusInternalServerError, "could not read inventory source")
		return nil
	}
	if denyOnAuthzError(w, log, authz.authorizeAll(r.Context(), grant.AccessUse,
		src.ProjectID, src.CredentialID, src.InventoryID)) {
		return nil
	}
	return src
}

func updateSourceHandler(sources invsource.Store, authz *authorizer, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if sources == nil {
			respondError(w, log, http.StatusNotFound, "inventory sources not enabled")
			return
		}
		var req createSourceRequest
		if !decodeStrict(w, log, r.Body, &req) {
			return
		}
		if req.Name == "" || req.Source == "" {
			respondError(w, log, http.StatusBadRequest, "name and source are required")
			return
		}
		if denyOnAuthzError(w, log,
			authz.authorizeAll(r.Context(), grant.AccessUse, req.ProjectID, req.CredentialID)) {
			return
		}
		// The stored source is authorized too, so omitting the references cannot skip the check.
		if authorizeStoredSource(w, r, sources, authz, log) == nil {
			return
		}
		id := r.PathValue("id")
		stored, gerr := sources.Get(r.Context(), id)
		if errors.Is(gerr, invsource.ErrNotFound) {
			respondError(w, log, http.StatusNotFound, "source not found")
			return
		}
		if gerr != nil {
			log.Error("server: read inventory source: " + gerr.Error())
			respondError(w, log, http.StatusInternalServerError, "could not read source")
			return
		}
		onLaunch := stored.UpdateOnLaunch
		if req.UpdateOnLaunch != nil {
			onLaunch = *req.UpdateOnLaunch
		}
		interval := stored.SyncIntervalSeconds
		if req.SyncIntervalSeconds != nil {
			interval = *req.SyncIntervalSeconds
		}
		err := sources.Update(r.Context(), &invsource.Source{
			ID: id, Name: req.Name, Source: req.Source,
			CredentialID: req.CredentialID, ProjectID: req.ProjectID,
			UpdateOnLaunch: onLaunch, SyncIntervalSeconds: interval,
		})
		if errors.Is(err, invsource.ErrNotFound) {
			respondError(w, log, http.StatusNotFound, "source not found")
			return
		}
		if err != nil {
			log.Error("server: update source: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not update source")
			return
		}
		updated, err := sources.Get(r.Context(), id)
		if err != nil {
			log.Error("server: read updated source: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not read source")
			return
		}
		respondJSON(w, log, http.StatusOK, updated, wantsPretty(r))
	}
}

// listSourcesHandler returns all sources.
func listSourcesHandler(sources invsource.Store, authz *authorizer, log *zap.Logger) http.HandlerFunc {
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
		// A source is visible on the same objects that govern writing one, its project and the
		// credential it borrows. Reading was unauthorized, and a source carries last_error, which is
		// verbatim plugin output and routinely names cloud accounts and endpoints.
		restricted, err := grantsEnforced(r.Context(), authz)
		if err != nil {
			log.Error("server: read filter: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not list sources")
			return
		}
		if restricted {
			kept := make([]*invsource.Source, 0, len(list))
			for _, src := range list {
				if authz.authorizeAll(r.Context(), grant.AccessUse,
					src.ProjectID, src.CredentialID, src.InventoryID) == nil {
					kept = append(kept, src)
				}
			}
			list = kept
		}
		respondJSON(w, log, http.StatusOK,
			listSourcesResponse{Sources: list, Count: len(list)}, wantsPretty(r))
	}
}

// deleteSourceHandler removes a source. Its backing inventory is left in place.
func deleteSourceHandler(sources invsource.Store, authz *authorizer, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if sources == nil {
			respondError(w, log, http.StatusNotFound, "inventory sources not enabled")
			return
		}
		// Deleting a source stops the refresh that keeps an inventory current, which quietly leaves
		// runs targeting a stale host list, so it asks the same question editing one does.
		if authorizeStoredSource(w, r, sources, authz, log) == nil {
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
func refreshSourceHandler(refresher SourceRefresher, sources invsource.Store, authz *authorizer,
	log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if refresher == nil {
			respondError(w, log, http.StatusNotFound, "inventory sources not enabled")
			return
		}
		// A refresh runs the source's plugin, decrypts its credential, and rewrites the hosts the
		// backing inventory names, so it is at least as consequential as editing one.
		if authorizeStoredSource(w, r, sources, authz, log) == nil {
			return
		}
		src, err := refresher.RefreshSource(r.Context(), r.PathValue("id"))
		if errors.Is(err, invsource.ErrNotFound) {
			respondError(w, log, http.StatusNotFound, "source not found")
			return
		}
		if err != nil {
			// The refresh failure detail is recorded on the source as its LastError, which an admin
			// reads back from the source, so the response stays generic and leaks no plugin internals.
			log.Error("server: refresh source: " + err.Error())
			respondError(w, log, http.StatusBadGateway, "inventory refresh failed")
			return
		}
		respondJSON(w, log, http.StatusOK, src, wantsPretty(r))
	}
}
