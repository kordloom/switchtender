package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"go.uber.org/zap"

	"github.com/dcadolph/yardmaster/internal/inventory"
)

// createInventoryRequest is the JSON body accepted by POST /inventories.
type createInventoryRequest struct {
	// Name labels the inventory. Required.
	Name string `json:"name"`
	// Content is the inventory text, INI or YAML. Required.
	Content string `json:"content"`
}

// listInventoriesResponse wraps the inventory list.
type listInventoriesResponse struct {
	// Inventories is the ordered list.
	Inventories []*inventory.Inventory `json:"inventories"`
	// Count is the number returned.
	Count int `json:"count"`
}

// createInventoryHandler stores a new inventory.
func createInventoryHandler(store inventory.Store, log *zap.Logger) http.HandlerFunc {
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
		if req.Name == "" || req.Content == "" {
			respondError(w, log, http.StatusBadRequest, "name and content are required")
			return
		}
		i := &inventory.Inventory{
			ID: inventory.NewID(), Name: req.Name, Content: req.Content, CreatedAt: time.Now(),
		}
		if err := store.Save(r.Context(), i); err != nil {
			log.Error("server: save inventory: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not store inventory")
			return
		}
		respondJSON(w, log, http.StatusCreated, i, wantsPretty(r))
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
