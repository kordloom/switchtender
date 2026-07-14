package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"go.uber.org/zap"

	"github.com/dcadolph/railwarden/internal/credential"
)

// createCredentialRequest is the JSON body accepted by POST /credentials. The secret arrives in
// plaintext over the API and is sealed before it touches the store.
type createCredentialRequest struct {
	// Name labels the credential. Required.
	Name string `json:"name"`
	// Kind is ssh_key or vault_password. Required.
	Kind credential.Kind `json:"kind"`
	// Source is local (default) for a value stored here, or command for a command whose stdout is the
	// secret, fetched from an external store at run time.
	Source string `json:"source,omitempty"`
	// Secret is the material itself, or the command for a command source. Required, never echoed back.
	Secret string `json:"secret"`
}

// listCredentialsResponse wraps the credential list, secrets excluded by the model's json tags.
type listCredentialsResponse struct {
	// Credentials is the ordered list.
	Credentials []credentialView `json:"credentials"`
	// Count is the number returned.
	Count int `json:"count"`
}

// credentialView is a credential in a list response, adding whether it still needs a secret so the
// UI can flag imported shells and offer to fill them. The embedded credential's sealed secret stays
// hidden by its json tag.
type credentialView struct {
	*credential.Credential
	// NeedsSecret reports that the credential has no sealed secret yet, as imported credential shells
	// do until their secret is set.
	NeedsSecret bool `json:"needs_secret"`
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
				"credentials disabled: set RAILWARDEN_ENCRYPTION_KEY and RAILWARDEN_ENCRYPTION_SALT on the server")
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
				"kind must be ssh_key, vault_password, env, token, become_password, or registry")
			return
		}
		if !credential.ValidSource(req.Source) {
			respondError(w, log, http.StatusBadRequest, "source must be local, command, vault, or gsm")
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
			Source: credential.NormalizeSource(req.Source), Secret: sealed, CreatedAt: time.Now(),
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
	// Source is applied only when a new secret is sent: local for a stored value, command for a
	// command. Blank keeps the current source. Optional.
	Source string `json:"source,omitempty"`
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
					"credentials disabled: set RAILWARDEN_ENCRYPTION_KEY and RAILWARDEN_ENCRYPTION_SALT on the server")
				return
			}
			kind := c.Kind
			if req.Kind != "" {
				kind = req.Kind
			}
			if !credential.ValidKind(kind) {
				respondError(w, log, http.StatusBadRequest,
					"kind must be ssh_key, vault_password, env, token, become_password, or registry")
				return
			}
			source := c.Source
			if req.Source != "" {
				source = req.Source
			}
			if !credential.ValidSource(source) {
				respondError(w, log, http.StatusBadRequest, "source must be local, command, vault, or gsm")
				return
			}
			sealed, err := sealer.Seal(secret)
			if err != nil {
				log.Error("server: seal credential: " + err.Error())
				respondError(w, log, http.StatusInternalServerError, "could not update credential")
				return
			}
			c.Kind = kind
			c.Source = credential.NormalizeSource(source)
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
		views := make([]credentialView, 0, len(list))
		for _, c := range list {
			views = append(views, credentialView{Credential: c, NeedsSecret: c.Secret == ""})
		}
		respondJSON(w, log, http.StatusOK,
			listCredentialsResponse{Credentials: views, Count: len(views)}, wantsPretty(r))
	}
}

// deleteCredentialHandler removes a credential.
func deleteCredentialHandler(store credential.Store, refs *refChecker, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if store == nil {
			respondError(w, log, http.StatusNotFound, "credentials not enabled")
			return
		}
		id := r.PathValue("id")
		if refs != nil {
			used, err := refs.credentialRefs(r.Context(), id)
			if err != nil {
				log.Error("server: credential references: " + err.Error())
				respondError(w, log, http.StatusInternalServerError, "could not check credential references")
				return
			}
			if !used.empty() {
				respondInUse(w, log, "credential in use", used, wantsPretty(r))
				return
			}
		}
		err := store.Delete(r.Context(), id)
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
