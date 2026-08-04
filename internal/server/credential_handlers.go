package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/credential"
)

// sealableSecret builds the plaintext to seal, folding an ssh_key passphrase into a structured
// secret. It rejects a passphrase on anything but a locally stored ssh_key, where the passphrase
// would otherwise be silently ignored at run time.
func sealableSecret(kind credential.Kind, source, secret, passphrase string) (string, error) {
	if passphrase == "" {
		return secret, nil
	}
	if kind != credential.KindSSHKey {
		return "", errors.New("passphrase applies only to an ssh_key credential")
	}
	if credential.NormalizeSource(source) != credential.SourceLocal {
		return "", errors.New("passphrase applies only to a locally stored ssh_key, not an external source")
	}
	return credential.BuildSSHKeySecret(secret, passphrase), nil
}

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
	// Passphrase unlocks a passphrase protected ssh_key at run time. It applies only to a locally
	// stored ssh_key and is sealed alongside the key. Optional, never echoed back.
	Passphrase string `json:"passphrase,omitempty"`
	// VaultID labels an Ansible Vault password for --vault-id; only meaningful on the
	// vault_password kind.
	VaultID string `json:"vault_id"`
	// OrgID names the owning organization. Empty leaves the credential unowned and global. Optional.
	OrgID string `json:"org_id,omitempty"`
	// TypeID names a custom credential type. When set, Fields carries the type's field values and
	// Kind and Secret are not used.
	TypeID string `json:"type_id,omitempty"`
	// Fields holds a typed credential's field values, sealed together as one object. Every value is
	// secret at rest; whether it is masked in run output is decided by the type's field definitions.
	Fields map[string]string `json:"fields,omitempty"`
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
func createCredentialHandler(store credential.Store, types credential.TypeStore,
	sealer *credential.Sealer, authz *authorizer, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if store == nil || sealer == nil {
			respondError(w, log, http.StatusNotFound, "credentials not enabled")
			return
		}
		if !sealer.Enabled() {
			respondError(w, log, http.StatusConflict,
				"credentials disabled: set SWITCHTENDER_ENCRYPTION_KEY and SWITCHTENDER_ENCRYPTION_SALT on the server")
			return
		}
		var req createCredentialRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			respondError(w, log, http.StatusBadRequest, "invalid request body")
			return
		}
		if req.Name == "" {
			respondError(w, log, http.StatusBadRequest, "name is required")
			return
		}
		// A typed credential takes a different shape: its field values are sealed as one JSON object
		// and its type drives injection, so kind and a single secret do not apply. It is handled
		// here and the built-in path below is skipped.
		if req.TypeID != "" {
			createTypedCredential(w, r, store, types, sealer, authz, &req, log)
			return
		}
		if req.Secret == "" {
			respondError(w, log, http.StatusBadRequest, "secret is required")
			return
		}
		if !credential.ValidKind(req.Kind) {
			respondError(w, log, http.StatusBadRequest,
				"kind must be one of: "+credential.KindList()+" (or a registered custom type)")
			return
		}
		if !credential.ValidSource(req.Source) {
			respondError(w, log, http.StatusBadRequest,
				"source must be one of: "+credential.SourceList())
			return
		}
		// Trimmed the same way the update handler trims, so one spelling of vault_id is not
		// accepted by create and rejected by update, or the reverse.
		vaultLabel := strings.TrimSpace(req.VaultID)
		if vaultLabel != "" && req.Kind != credential.KindVaultPassword {
			respondError(w, log, http.StatusBadRequest, "vault_id applies only to vault_password credentials")
			return
		}
		if !credential.ValidVaultID(vaultLabel) {
			respondError(w, log, http.StatusBadRequest,
				"vault_id must be letters, digits, underscores, or hyphens")
			return
		}

		secretPlain, err := sealableSecret(req.Kind, req.Source, req.Secret, req.Passphrase)
		if err != nil {
			respondError(w, log, http.StatusBadRequest, err.Error())
			return
		}
		sealed, err := sealer.Seal(secretPlain)
		req.Secret = ""
		req.Passphrase = ""
		if err != nil {
			log.Error("server: seal credential: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not store credential")
			return
		}
		// Putting a credential in an organization grants every member of it use of that credential,
		// so entering one is checked by membership. It is checked here rather than as an object,
		// because an organization is not one.
		if authz.denyForeignOrg(w, r, log, req.OrgID) {
			return
		}
		c := &credential.Credential{
			ID: credential.NewID(), Name: req.Name, Kind: req.Kind,
			Source: credential.NormalizeSource(req.Source), Secret: sealed, OrgID: req.OrgID,
			VaultID: vaultLabel, CreatedAt: time.Now(),
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
	// Passphrase unlocks a passphrase protected ssh_key. It applies only when a new secret is sent for
	// a locally stored ssh_key and is sealed alongside the key. Optional, never echoed back.
	Passphrase string `json:"passphrase,omitempty"`
	// OrgID names the owning organization, replacing the stored owner. Empty leaves the credential
	// unowned and global. Optional.
	OrgID string `json:"org_id,omitempty"`
	// VaultID relabels an Ansible Vault password; only meaningful on the vault_password kind. A
	// pointer so an omitted field keeps the stored label rather than clearing it: a rename that
	// sends only the name must not wipe the vault id a multi-vault run depends on. A present empty
	// string clears the label.
	VaultID *string `json:"vault_id"`
}

// createTypedCredential stores a credential of a custom type: its field values are validated against
// the type, sealed together as one JSON object, and injection is driven by the type at run time.
func createTypedCredential(w http.ResponseWriter, r *http.Request, store credential.Store,
	types credential.TypeStore, sealer *credential.Sealer, authz *authorizer,
	req *createCredentialRequest, log *zap.Logger) {
	if types == nil {
		respondError(w, log, http.StatusNotFound, "credential types not enabled")
		return
	}
	typ, err := types.Get(r.Context(), req.TypeID)
	if errors.Is(err, credential.ErrNotFound) {
		respondError(w, log, http.StatusBadRequest, "no such credential type")
		return
	}
	if err != nil {
		log.Error("server: read credential type: " + err.Error())
		respondError(w, log, http.StatusInternalServerError, "could not read credential type")
		return
	}
	// Only fields the type declares are accepted, so a caller cannot smuggle a value under a name
	// the injectors do not read and would not expect to be stored.
	declared := make(map[string]bool, len(typ.Fields))
	for _, f := range typ.Fields {
		declared[f.Name] = true
	}
	for name := range req.Fields {
		if !declared[name] {
			respondError(w, log, http.StatusBadRequest,
				"field "+name+" is not declared by this credential type")
			return
		}
	}
	values, err := json.Marshal(req.Fields)
	if err != nil {
		log.Error("server: encode credential fields: " + err.Error())
		respondError(w, log, http.StatusInternalServerError, "could not store credential")
		return
	}
	sealed, err := sealer.Seal(string(values))
	req.Fields = nil
	if err != nil {
		log.Error("server: seal credential: " + err.Error())
		respondError(w, log, http.StatusInternalServerError, "could not store credential")
		return
	}
	if authz.denyForeignOrg(w, r, log, req.OrgID) {
		return
	}
	c := &credential.Credential{
		ID: credential.NewID(), Name: req.Name, TypeID: req.TypeID,
		Secret: sealed, OrgID: req.OrgID, CreatedAt: time.Now(),
	}
	if err := store.Save(r.Context(), c); err != nil {
		log.Error("server: save credential: " + err.Error())
		respondError(w, log, http.StatusInternalServerError, "could not store credential")
		return
	}
	respondJSON(w, log, http.StatusCreated, c, wantsPretty(r))
}

// updateCredentialHandler renames a credential and, only when a new secret is supplied, reseals it
// with the new material and kind. Renaming never requires re-sending the secret.
func updateCredentialHandler(store credential.Store, sealer *credential.Sealer, authz *authorizer,
	log *zap.Logger) http.HandlerFunc {
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
		passphrase := req.Passphrase
		req.Secret = ""
		req.Passphrase = ""
		if req.Name == "" {
			respondError(w, log, http.StatusBadRequest, "name is required")
			return
		}
		if passphrase != "" && secret == "" {
			respondError(w, log, http.StatusBadRequest,
				"passphrase requires the ssh_key secret to be sent with it")
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
		// A typed credential is not updated through this path, which speaks kind and a single secret
		// and would clear the type and reinterpret its sealed field object as a raw value. It is
		// recreated instead, so the update cannot silently corrupt it.
		if c.TypeID != "" {
			respondError(w, log, http.StatusConflict,
				"this credential belongs to a custom type; delete and recreate it to change its fields")
			return
		}

		// Moving a credential into an organization grants every member use of it, and moving it out
		// takes that away from the members it had. Both directions change who may use a secret, so
		// both are checked.
		if authz.denyForeignOrg(w, r, log, req.OrgID) {
			return
		}
		if c.OrgID != req.OrgID && authz.denyForeignOrg(w, r, log, c.OrgID) {
			return
		}
		// The kind only changes when a new secret is sent, since a kind change re-seals the secret
		// in the new format. So the vault_id rules must be checked against the kind that will
		// actually persist, not against req.Kind, or a kind change with no secret would land a
		// label on a credential whose stored kind never moved.
		finalKind := c.Kind
		if secret != "" && req.Kind != "" {
			finalKind = req.Kind
		}
		if req.VaultID != nil {
			label := strings.TrimSpace(*req.VaultID)
			if !credential.ValidVaultID(label) {
				respondError(w, log, http.StatusBadRequest,
					"vault_id must be letters, digits, underscores, or hyphens")
				return
			}
			if label != "" && finalKind != credential.KindVaultPassword {
				respondError(w, log, http.StatusBadRequest,
					"vault_id applies only to vault_password credentials")
				return
			}
			c.VaultID = label
		}
		// A label never outlives the vault kind: changing a labeled vault credential to another
		// kind drops the now meaningless label rather than storing the state create forbids.
		if finalKind != credential.KindVaultPassword {
			c.VaultID = ""
		}
		c.Name = req.Name
		c.OrgID = req.OrgID
		if secret != "" {
			if !sealer.Enabled() {
				respondError(w, log, http.StatusConflict,
					"credentials disabled: set SWITCHTENDER_ENCRYPTION_KEY and SWITCHTENDER_ENCRYPTION_SALT on the server")
				return
			}
			kind := c.Kind
			if req.Kind != "" {
				kind = req.Kind
			}
			if !credential.ValidKind(kind) {
				respondError(w, log, http.StatusBadRequest,
					"kind must be one of: "+credential.KindList()+" (or a registered custom type)")
				return
			}
			source := c.Source
			if req.Source != "" {
				source = req.Source
			}
			if !credential.ValidSource(source) {
				respondError(w, log, http.StatusBadRequest,
					"source must be one of: "+credential.SourceList())
				return
			}
			secretPlain, err := sealableSecret(kind, source, secret, passphrase)
			if err != nil {
				respondError(w, log, http.StatusBadRequest, err.Error())
				return
			}
			sealed, err := sealer.Seal(secretPlain)
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
func listCredentialsHandler(store credential.Store, authz *authorizer, log *zap.Logger) http.HandlerFunc {
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
		visible, err := filterReadable(r.Context(), authz, list,
			func(c *credential.Credential) string { return c.ID },
			func(c *credential.Credential) string { return c.OrgID })
		if err != nil {
			log.Error("server: list credentials: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not list credentials")
			return
		}
		views := make([]credentialView, 0, len(visible))
		for _, c := range visible {
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
