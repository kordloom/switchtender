package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/user"
)

// denyNonAdminCommandSource refuses a non-admin request to give what the command source. The command
// source runs a shell command on the executor when the value resolves, so it is code execution on the
// run host, an admin capability. A manage grant delegates rotating and renaming an object, never
// turning it into a shell payload, and the PUT routes admit a manage-delegated non-admin, so the role
// rather than object delegation is what must bound this. It is enforced on create and update alike,
// and reports true when it has already answered the request.
//
// what names the object in the refusal, since a credential and an inventory both carry a command
// source and both reach the same executor.
func denyNonAdminCommandSource(w http.ResponseWriter, r *http.Request, log *zap.Logger,
	source, what string) bool {
	if credential.NormalizeSource(source) != credential.SourceCommand {
		return false
	}
	// No actor in context means the API is serving unauthenticated (no tokens, no SSO), where every
	// caller is the local admin, the same way denyForeignOrg and the manage checks treat it. The
	// guard bites only when auth is enforced and the caller is a real non-admin, which is the
	// manage-delegated operator the escalation would otherwise reach.
	actor, ok := actorFrom(r.Context())
	if ok && actor.Role != user.RoleAdmin {
		respondError(w, log, http.StatusForbidden,
			"only an admin may set "+what+" to the command source, since it runs a command on the executor")
		return true
	}
	return false
}

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
	// OrgID names the owning organization. Absent or empty leaves the credential unowned and
	// global. Optional.
	OrgID *string `json:"org_id,omitempty"`
	// TypeID names a custom credential type. When set, Fields carries the type's field values and
	// Kind and Secret are not used.
	TypeID string `json:"type_id,omitempty"`
	// Fields holds a typed credential's field values, sealed together as one object. Every value is
	// secret at rest; whether it is masked in run output is decided by the type's field definitions.
	Fields map[string]string `json:"fields,omitempty"`
	// Settings carries the credential's non-secret fields, such as the connection user or a become
	// method. Unlike the secret they return from the API and show in the interface. Optional, and
	// only for built-in kinds; a custom type declares its own non-secret fields.
	Settings map[string]string `json:"settings,omitempty"`
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
	// UsedBy names the configuration objects that reference this credential, keyed by kind, from the
	// same reading the delete guard uses. Absent when nothing references it.
	UsedBy map[string][]string `json:"used_by,omitempty"`
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
		if !decodeStrict(w, log, r.Body, &req) {
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
		if denyNonAdminCommandSource(w, r, log, req.Source, "a credential") {
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

		if err := credential.ValidateSettings(req.Settings); err != nil {
			respondError(w, log, http.StatusBadRequest, err.Error())
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
		if authz.denyForeignOrg(w, r, log, orgForCreate(req.OrgID)) {
			return
		}
		c := &credential.Credential{
			ID: credential.NewID(), Name: req.Name, Kind: req.Kind,
			Source: credential.NormalizeSource(req.Source), Secret: sealed, OrgID: orgForCreate(req.OrgID),
			VaultID: vaultLabel, Settings: req.Settings, CreatedAt: time.Now(),
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
	// OrgID names the owning organization, replacing the stored owner. A pointer so an omitted
	// field keeps the stored owner rather than un-owning the record: every edit dialog sends the
	// fields it renders, and none of them renders an organization. A present empty string is the
	// explicit "move this out of its organization".
	OrgID *string `json:"org_id,omitempty"`
	// VaultID relabels an Ansible Vault password; only meaningful on the vault_password kind. A
	// pointer so an omitted field keeps the stored label rather than clearing it: a rename that
	// sends only the name must not wipe the vault id a multi-vault run depends on. A present empty
	// string clears the label.
	VaultID *string `json:"vault_id"`
	// Settings replaces the credential's non-secret fields when present. An omitted field keeps the
	// stored settings, and a present empty object clears them. Optional.
	Settings map[string]string `json:"settings,omitempty"`
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
	if len(req.Settings) > 0 {
		respondError(w, log, http.StatusBadRequest,
			"settings apply to built-in credential kinds; a custom type declares its own non-secret fields")
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
	if authz.denyForeignOrg(w, r, log, orgForCreate(req.OrgID)) {
		return
	}
	c := &credential.Credential{
		ID: credential.NewID(), Name: req.Name, TypeID: req.TypeID,
		Secret: sealed, OrgID: orgForCreate(req.OrgID), CreatedAt: time.Now(),
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
		if !decodeStrict(w, log, r.Body, &req) {
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
		// Only a change of organization is a placement. Asking the question on every edit refused a
		// manage-delegated caller who is not a member every change, including a rename that moves
		// nothing, while delete asked nothing and succeeded, so the delegation forbade the safe
		// operation and allowed the unrecoverable one.
		orgID := orgForUpdate(req.OrgID, c.OrgID)
		if c.OrgID != orgID {
			if authz.denyForeignOrg(w, r, log, orgID) {
				return
			}
			if authz.denyForeignOrg(w, r, log, c.OrgID) {
				return
			}
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
		// A nil map means the field was omitted and the stored settings stay; a present map, empty
		// included, replaces them, so clearing is a deliberate act rather than a side effect.
		if req.Settings != nil {
			if err := credential.ValidateSettings(req.Settings); err != nil {
				respondError(w, log, http.StatusBadRequest, err.Error())
				return
			}
			c.Settings = req.Settings
			if len(c.Settings) == 0 {
				c.Settings = nil
			}
		} else if finalKind != c.Kind {
			// The same settings key means different things across kinds: user is the connection user
			// on ssh_password but the escalation target on become. So a kind change that carries no
			// new settings drops the stored ones rather than silently reinterpreting them, the same
			// way the vault label is dropped when the kind leaves vault_password.
			c.Settings = nil
		}
		c.Name = req.Name
		c.OrgID = orgForUpdate(req.OrgID, c.OrgID)
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
			// The command source is code execution on the executor, so setting or rewriting a
			// command credential is admin-only even for a manage-delegated caller. This sits inside
			// the secret block because a command credential's secret is the command itself: rewriting
			// it requires a new secret, so guarding here covers both flipping a credential to command
			// and changing the command an existing command credential runs.
			if denyNonAdminCommandSource(w, r, log, source, "a credential") {
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
func listCredentialsHandler(store credential.Store, refs *refChecker, authz *authorizer, log *zap.Logger) http.HandlerFunc {
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
		// The same reading the delete guard uses, so the column and the guard cannot disagree. A
		// failed lookup leaves the field absent rather than asserting emptiness.
		var refMap map[string]usedBy
		if refs != nil {
			if m, err := refs.allCredentialRefs(r.Context()); err == nil {
				refMap = m
			} else {
				log.Error("server: credential references: " + err.Error())
			}
		}
		views := make([]credentialView, 0, len(visible))
		for _, c := range visible {
			v := credentialView{Credential: c, NeedsSecret: c.Secret == ""}
			if u, ok := refMap[c.ID]; ok {
				v.UsedBy = u
			}
			views = append(views, v)
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
