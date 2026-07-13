package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"go.uber.org/zap"

	"github.com/dcadolph/yardmaster/internal/credential"
	"github.com/dcadolph/yardmaster/internal/grant"
	"github.com/dcadolph/yardmaster/internal/inventory"
)

// createInventoryRequest is the JSON body accepted by POST /inventories.
type createInventoryRequest struct {
	// Name labels the inventory. Required.
	Name string `json:"name"`
	// Content is the inventory text, INI or YAML. Required when the content source is local.
	Content string `json:"content"`
	// CredentialIDs names stored credentials materialized for every run that targets this inventory,
	// so the inventory can carry its own secret variables.
	CredentialIDs []string `json:"credential_ids,omitempty"`
	// ContentSource selects where the content comes from: local, command, vault, or gsm. Empty means
	// local, the stored content.
	ContentSource string `json:"content_source,omitempty"`
	// ContentConfig is the source config for a non-local content source: the command, or the JSON
	// address, path, and field for vault, or project, secret, and version for gsm. It is sealed at
	// rest. On update, a blank value keeps the stored config.
	ContentConfig string `json:"content_config,omitempty"`
	// Queue pins every run that targets this inventory to workers serving the queue, unless the run
	// or its template names its own. Empty uses the default pool.
	Queue string `json:"queue,omitempty"`
}

// inventorySource validates a request's content source and returns the normalized source and the
// sealed config to store, or a message and status to return. existing is the current sealed config,
// kept when a non-local update omits a new one.
func inventorySource(req createInventoryRequest, existing string, sealer *credential.Sealer) (source, sealed, msg string, status int) {
	source = credential.NormalizeSource(req.ContentSource)
	if !credential.ValidSource(source) {
		return "", "", "content source must be local, command, vault, or gsm", http.StatusBadRequest
	}
	if source == credential.SourceLocal {
		if req.Content == "" {
			return "", "", "content is required for a stored inventory", http.StatusBadRequest
		}
		return credential.SourceLocal, "", "", 0
	}
	if req.ContentConfig == "" {
		if existing == "" {
			return "", "", "content config is required for a " + source + " source", http.StatusBadRequest
		}
		return source, existing, "", 0
	}
	if sealer == nil || !sealer.Enabled() {
		return "", "", "content sources need encryption: set YARDMASTER_ENCRYPTION_KEY and YARDMASTER_ENCRYPTION_SALT", http.StatusConflict
	}
	s, err := sealer.Seal(req.ContentConfig)
	if err != nil {
		return "", "", "could not seal content source", http.StatusInternalServerError
	}
	return source, s, "", 0
}

// listInventoriesResponse wraps the inventory list.
type listInventoriesResponse struct {
	// Inventories is the ordered list.
	Inventories []*inventory.Inventory `json:"inventories"`
	// Count is the number returned.
	Count int `json:"count"`
}

// createInventoryHandler stores a new inventory.
func createInventoryHandler(store inventory.Store, authz *authorizer, sealer *credential.Sealer, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if store == nil {
			respondError(w, log, http.StatusNotFound, "inventories not enabled")
			return
		}
		var req createInventoryRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			respondError(w, log, http.StatusBadRequest, "invalid request body")
			return
		}
		if req.Name == "" {
			respondError(w, log, http.StatusBadRequest, "name is required")
			return
		}
		source, sealed, msg, status := inventorySource(req, "", sealer)
		if msg != "" {
			respondError(w, log, status, msg)
			return
		}
		if denyOnAuthzError(w, log, authz.authorizeAll(r.Context(), grant.AccessUse, req.CredentialIDs...)) {
			return
		}
		i := &inventory.Inventory{
			ID: inventory.NewID(), Name: req.Name, Content: req.Content,
			CredentialIDs: req.CredentialIDs, Queue: req.Queue, CreatedAt: time.Now(),
		}
		if source != credential.SourceLocal {
			i.ContentSource, i.ContentConfig = source, sealed
		}
		if err := store.Save(r.Context(), i); err != nil {
			log.Error("server: save inventory: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not store inventory")
			return
		}
		respondJSON(w, log, http.StatusCreated, i, wantsPretty(r))
	}
}

// updateInventoryHandler changes an existing inventory's name and content, keeping its id and
// creation time.
func updateInventoryHandler(store inventory.Store, authz *authorizer, sealer *credential.Sealer, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if store == nil {
			respondError(w, log, http.StatusNotFound, "inventories not enabled")
			return
		}
		var req createInventoryRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			respondError(w, log, http.StatusBadRequest, "invalid request body")
			return
		}
		if req.Name == "" {
			respondError(w, log, http.StatusBadRequest, "name is required")
			return
		}
		id := r.PathValue("id")
		existing, err := store.Get(r.Context(), id)
		if errors.Is(err, inventory.ErrNotFound) {
			respondError(w, log, http.StatusNotFound, "inventory not found")
			return
		}
		if err != nil {
			log.Error("server: read inventory: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not read inventory")
			return
		}
		source, sealed, msg, status := inventorySource(req, existing.ContentConfig, sealer)
		if msg != "" {
			respondError(w, log, status, msg)
			return
		}
		if denyOnAuthzError(w, log, authz.authorizeAll(r.Context(), grant.AccessUse, req.CredentialIDs...)) {
			return
		}
		inv := &inventory.Inventory{
			ID: id, Name: req.Name, Content: req.Content, CredentialIDs: req.CredentialIDs,
			Queue: req.Queue,
		}
		if source != credential.SourceLocal {
			inv.ContentSource, inv.ContentConfig = source, sealed
		}
		err = store.Update(r.Context(), inv)
		if errors.Is(err, inventory.ErrNotFound) {
			respondError(w, log, http.StatusNotFound, "inventory not found")
			return
		}
		if err != nil {
			log.Error("server: update inventory: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not update inventory")
			return
		}
		updated, err := store.Get(r.Context(), id)
		if err != nil {
			log.Error("server: read updated inventory: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not read inventory")
			return
		}
		respondJSON(w, log, http.StatusOK, updated, wantsPretty(r))
	}
}

// listInventoriesHandler returns all inventories.
func listInventoriesHandler(store inventory.Store, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if store == nil {
			respondError(w, log, http.StatusNotFound, "inventories not enabled")
			return
		}
		list, err := store.List(r.Context())
		if err != nil {
			log.Error("server: list inventories: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not list inventories")
			return
		}
		respondJSON(w, log, http.StatusOK,
			listInventoriesResponse{Inventories: list, Count: len(list)}, wantsPretty(r))
	}
}

// deleteInventoryHandler removes an inventory.
func deleteInventoryHandler(store inventory.Store, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if store == nil {
			respondError(w, log, http.StatusNotFound, "inventories not enabled")
			return
		}
		err := store.Delete(r.Context(), r.PathValue("id"))
		if errors.Is(err, inventory.ErrNotFound) {
			respondError(w, log, http.StatusNotFound, "inventory not found")
			return
		}
		if err != nil {
			log.Error("server: delete inventory: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not delete inventory")
			return
		}
		respondJSON(w, log, http.StatusOK, map[string]string{"deleted": r.PathValue("id")}, wantsPretty(r))
	}
}
