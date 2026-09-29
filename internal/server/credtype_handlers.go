package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/credential"
)

// credTypeResponse wraps a list of credential types.
type credTypeResponse struct {
	// Types are the operator-defined credential types.
	Types []*credential.CredentialType `json:"types"`
	// Count is how many were returned.
	Count int `json:"count"`
}

// createCredTypeHandler defines a new custom credential type.
//
// A type is validated before it is stored: its field names, injector names, and every {{field}}
// reference are checked, so a definition that would inject nothing or reference a field it does not
// declare is refused at creation rather than surfacing as a broken credential later. A type carries
// no secret, so it is management data, admin only to read as well as write.
func createCredTypeHandler(store credential.TypeStore, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if store == nil {
			respondError(w, log, http.StatusNotFound, "credential types not enabled")
			return
		}
		var t credential.CredentialType
		if err := json.NewDecoder(r.Body).Decode(&t); err != nil {
			respondError(w, log, http.StatusBadRequest, "invalid request body")
			return
		}
		t.ID = credential.NewTypeID()
		t.CreatedAt = time.Now()
		if err := t.Validate(); err != nil {
			respondError(w, log, http.StatusBadRequest, err.Error())
			return
		}
		if err := store.Save(r.Context(), &t); err != nil {
			log.Error("server: save credential type: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not store credential type")
			return
		}
		respondJSON(w, log, http.StatusCreated, &t, wantsPretty(r))
	}
}

// updateCredTypeHandler replaces a credential type, keeping its id.
func updateCredTypeHandler(store credential.TypeStore, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if store == nil {
			respondError(w, log, http.StatusNotFound, "credential types not enabled")
			return
		}
		var t credential.CredentialType
		if err := json.NewDecoder(r.Body).Decode(&t); err != nil {
			respondError(w, log, http.StatusBadRequest, "invalid request body")
			return
		}
		id := r.PathValue("id")
		if _, err := store.Get(r.Context(), id); errors.Is(err, credential.ErrNotFound) {
			respondError(w, log, http.StatusNotFound, "credential type not found")
			return
		} else if err != nil {
			log.Error("server: read credential type: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not read credential type")
			return
		}
		t.ID = id
		if err := t.Validate(); err != nil {
			respondError(w, log, http.StatusBadRequest, err.Error())
			return
		}
		if err := store.Save(r.Context(), &t); err != nil {
			log.Error("server: save credential type: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not store credential type")
			return
		}
		respondJSON(w, log, http.StatusOK, &t, wantsPretty(r))
	}
}

// listCredTypesHandler returns every credential type.
func listCredTypesHandler(store credential.TypeStore, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if store == nil {
			respondError(w, log, http.StatusNotFound, "credential types not enabled")
			return
		}
		types, err := store.List(r.Context())
		if err != nil {
			log.Error("server: list credential types: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not list credential types")
			return
		}
		respondJSON(w, log, http.StatusOK,
			credTypeResponse{Types: types, Count: len(types)}, wantsPretty(r))
	}
}

// getCredTypeHandler returns one credential type.
func getCredTypeHandler(store credential.TypeStore, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if store == nil {
			respondError(w, log, http.StatusNotFound, "credential types not enabled")
			return
		}
		t, err := store.Get(r.Context(), r.PathValue("id"))
		if errors.Is(err, credential.ErrNotFound) {
			respondError(w, log, http.StatusNotFound, "credential type not found")
			return
		}
		if err != nil {
			log.Error("server: get credential type: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not read credential type")
			return
		}
		respondJSON(w, log, http.StatusOK, t, wantsPretty(r))
	}
}

// deleteCredTypeHandler removes a credential type.
func deleteCredTypeHandler(store credential.TypeStore, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if store == nil {
			respondError(w, log, http.StatusNotFound, "credential types not enabled")
			return
		}
		err := store.Delete(r.Context(), r.PathValue("id"))
		if errors.Is(err, credential.ErrNotFound) {
			respondError(w, log, http.StatusNotFound, "credential type not found")
			return
		}
		if err != nil {
			log.Error("server: delete credential type: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not delete credential type")
			return
		}
		respondJSON(w, log, http.StatusOK, map[string]string{"deleted": r.PathValue("id")}, wantsPretty(r))
	}
}
