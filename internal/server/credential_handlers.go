package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"go.uber.org/zap"

	"github.com/dcadolph/yardmaster/internal/credential"
)

// createCredentialRequest is the JSON body accepted by POST /credentials. The secret arrives in
// plaintext over the API and is sealed before it touches the store.
type createCredentialRequest struct {
	// Name labels the credential. Required.
	Name string `json:"name"`
	// Kind is ssh_key or vault_password. Required.
	Kind credential.Kind `json:"kind"`
	// Secret is the material itself. Required, never echoed back.
	Secret string `json:"secret"`
}

// listCredentialsResponse wraps the credential list, secrets excluded by the model's json tags.
type listCredentialsResponse struct {
	// Credentials is the ordered list.
	Credentials []*credential.Credential `json:"credentials"`
	// Count is the number returned.
	Count int `json:"count"`
}

// createCredentialHandler seals and stores a new credential.
func createCredentialHandler(store credential.Store, sealer *credential.Sealer, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if store == nil || sealer == nil {
			respondError(w, log, http.StatusNotFound, "credentials not enabled")
			return
		}
		if !sealer.Enabled() {
			respondError(w, log, http.StatusConflict,
				"credentials disabled: set YARDMASTER_ENCRYPTION_KEY and YARDMASTER_ENCRYPTION_SALT on the server")
			return
		}
		var req createCredentialRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			respondError(w, log, http.StatusBadRequest, "invalid request body")
			return
		}
		if req.Name == "" || req.Secret == "" {
			respondError(w, log, http.StatusBadRequest, "name and secret are required")
			return
		}
		if !credential.ValidKind(req.Kind) {
			respondError(w, log, http.StatusBadRequest,
				"kind must be ssh_key, vault_password, env, become_password, or registry")
			return
		}

		sealed, err := sealer.Seal(req.Secret)
		req.Secret = ""
		if err != nil {
			log.Error("server: seal credential: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not store credential")
			return
		}
		c := &credential.Credential{
			ID: credential.NewID(), Name: req.Name, Kind: req.Kind,
			Secret: sealed, CreatedAt: time.Now(),
		}
		if err := store.Save(r.Context(), c); err != nil {
			log.Error("server: save credential: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not store credential")
			return
		}
		respondJSON(w, log, http.StatusCreated, c, wantsPretty(r))
	}
}

// updateCredentialRequest is the JSON body accepted by PUT /credentials/{id}. A blank secret keeps
// the current sealed value, so a credential can be renamed without re-sending its secret.
type updateCredentialRequest struct {
	// Name labels the credential. Required.
	Name string `json:"name"`
	// Kind is applied only when a new secret is sent; blank keeps the current kind. Optional.
	Kind credential.Kind `json:"kind,omitempty"`
	// Secret, when non-empty, replaces the stored material; blank keeps it. Never echoed back.
	Secret string `json:"secret,omitempty"`
}

// updateCredentialHandler renames a credential and, only when a new secret is supplied, reseals it
// with the new material and kind. Renaming never requires re-sending the secret.
func updateCredentialHandler(store credential.Store, sealer *credential.Sealer, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if store == nil || sealer == nil {
			respondError(w, log, http.StatusNotFound, "credentials not enabled")
			return
		}
		var req updateCredentialRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			respondError(w, log, http.StatusBadRequest, "invalid request body")
			return
		}
		secret := req.Secret
		req.Secret = ""
		if req.Name == "" {
			respondError(w, log, http.StatusBadRequest, "name is required")
			return
		}
		id := r.PathValue("id")
		c, err := store.Get(r.Context(), id)
		if errors.Is(err, credential.ErrNotFound) {
			respondError(w, log, http.StatusNotFound, "credential not found")
			return
		}
		if err != nil {
			log.Error("server: get credential: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not read credential")
			return
		}

		c.Name = req.Name
		if secret != "" {
			if !sealer.Enabled() {
				respondError(w, log, http.StatusConflict,
					"credentials disabled: set YARDMASTER_ENCRYPTION_KEY and YARDMASTER_ENCRYPTION_SALT on the server")
				return
			}
			kind := c.Kind
			if req.Kind != "" {
				kind = req.Kind
			}
			if !credential.ValidKind(kind) {
				respondError(w, log, http.StatusBadRequest,
					"kind must be ssh_key, vault_password, env, become_password, or registry")
				return
			}
			sealed, err := sealer.Seal(secret)
			secret = ""
			if err != nil {
				log.Error("server: seal credential: " + err.Error())
				respondError(w, log, http.StatusInternalServerError, "could not update credential")
				return
			}
			c.Kind = kind
			c.Secret = sealed
		}
		if err := store.Update(r.Context(), c); err != nil {
			log.Error("server: update credential: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not update credential")
			return
		}
		respondJSON(w, log, http.StatusOK, c, wantsPretty(r))
	}
}

// listCredentialsHandler returns all credentials without secret material.
func listCredentialsHandler(store credential.Store, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if store == nil {
			respondError(w, log, http.StatusNotFound, "credentials not enabled")
			return
		}
		list, err := store.List(r.Context())
		if err != nil {
			log.Error("server: list credentials: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not list credentials")
			return
		}
		respondJSON(w, log, http.StatusOK,
			listCredentialsResponse{Credentials: list, Count: len(list)}, wantsPretty(r))
	}
}

// deleteCredentialHandler removes a credential.
func deleteCredentialHandler(store credential.Store, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if store == nil {
			respondError(w, log, http.StatusNotFound, "credentials not enabled")
			return
		}
		err := store.Delete(r.Context(), r.PathValue("id"))
		if errors.Is(err, credential.ErrNotFound) {
			respondError(w, log, http.StatusNotFound, "credential not found")
			return
		}
		if err != nil {
			log.Error("server: delete credential: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not delete credential")
			return
		}
		respondJSON(w, log, http.StatusOK, map[string]string{"deleted": r.PathValue("id")}, wantsPretty(r))
	}
}
