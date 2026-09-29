package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/grant"
	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/user"
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
	// ContentSource selects where the content comes from: local, or any registered secret source such
	// as command, vault, gsm, aws, or azure. Empty means local, the stored content.
	ContentSource string `json:"content_source,omitempty"`
	// ContentConfig is the source config for a non-local content source: the command, or the JSON
	// address, path, and field for vault, or project, secret, and version for gsm. It is sealed at
	// rest. On update, a blank value keeps the stored config.
	ContentConfig string `json:"content_config,omitempty"`
	// Queue pins every run that targets this inventory to workers serving the queue, unless the run
	// or its template names its own. Empty uses the default pool.
	Queue string `json:"queue,omitempty"`
	// OrgID names the owning organization. A pointer so an omitted field keeps the stored owner on
	// an update rather than un-owning the record, and leaves a create unowned. A present empty
	// string is the explicit "move this out of its organization".
	OrgID *string `json:"org_id,omitempty"`
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
		return "", "", "content sources need encryption: set SWITCHTENDER_ENCRYPTION_KEY and SWITCHTENDER_ENCRYPTION_SALT", http.StatusConflict
	}
	s, err := sealer.Seal(req.ContentConfig)
	if err != nil {
		return "", "", "could not seal content source", http.StatusInternalServerError
	}
	return source, s, "", 0
}

// denyNonAdminInventoryCommand refuses a non-admin request to point an inventory at a command, or to
// rewrite the command one already runs, and reports true when it has answered the request.
//
// An inventory's command source runs a shell command on the executor to produce the host list, so it
// is code execution on the run host, the same capability the credential handlers reserve to an admin.
// The route's role floor is admin, but a manage grant on the inventory walks around it, so without
// this an operator holding an ordinary manage grant on one inventory could store a shell payload and
// then launch a run against that inventory to execute it.
//
// Re-saving an inventory that already runs this command, to rename it or move it between
// organizations, supplies no new command and stays delegable, which is what a manage grant is for.
func denyNonAdminInventoryCommand(w http.ResponseWriter, r *http.Request, log *zap.Logger,
	source, newConfig, existingSource string) bool {
	if credential.NormalizeSource(source) != credential.SourceCommand {
		return false
	}
	if newConfig == "" && credential.NormalizeSource(existingSource) == credential.SourceCommand {
		return false
	}
	return denyNonAdminCommandSource(w, r, log, source, "an inventory")
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
		if !decodeStrict(w, log, r.Body, &req) {
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
		if denyNonAdminInventoryCommand(w, r, log, source, req.ContentConfig, "") {
			return
		}
		// The queue is authorized alongside the credentials: pinning an inventory to a queue routes
		// every run that targets it to that queue's workers, so it reaches the same segment a run
		// naming the queue directly would.
		if denyOnAuthzError(w, log, authz.authorizeAll(r.Context(), grant.AccessUse,
			append([]string{queueObject(req.Queue)}, req.CredentialIDs...)...)) {
			return
		}
		// Putting an inventory in an organization gives every member of it use, and taking it out
		// takes that away. Both directions are checked, the same as a template.
		if authz.denyForeignOrg(w, r, log, orgForCreate(req.OrgID)) {
			return
		}
		i := &inventory.Inventory{
			ID: inventory.NewID(), Name: req.Name, Content: req.Content,
			CredentialIDs: req.CredentialIDs, Queue: req.Queue, OrgID: orgForCreate(req.OrgID), CreatedAt: time.Now(),
		}
		if source != credential.SourceLocal {
			i.ContentSource, i.ContentConfig = source, sealed
		}
		if err := store.Save(r.Context(), i); err != nil {
			log.Error("server: save inventory: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not store inventory")
			return
		}
		respondJSON(w, log, http.StatusCreated, redactInventory(r.Context(), i), wantsPretty(r))
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
		if !decodeStrict(w, log, r.Body, &req) {
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
		if denyNonAdminInventoryCommand(w, r, log, source, req.ContentConfig, existing.ContentSource) {
			return
		}
		// The queue is authorized alongside the credentials: pinning an inventory to a queue routes
		// every run that targets it to that queue's workers, so it reaches the same segment a run
		// naming the queue directly would.
		if denyOnAuthzError(w, log, authz.authorizeAll(r.Context(), grant.AccessUse,
			append([]string{queueObject(req.Queue)}, req.CredentialIDs...)...)) {
			return
		}
		// Both directions of an organization change are checked: entering one gives every member
		// use of this inventory, and leaving one takes it away from the members it had.
		// Only a change of organization is a placement. Asking on every edit refused a manage-delegated
		// caller who is not a member even a rename, while delete asked nothing and succeeded.
		orgID := orgForUpdate(req.OrgID, existing.OrgID)
		if existing.OrgID != orgID {
			if authz.denyForeignOrg(w, r, log, orgID) {
				return
			}
			if authz.denyForeignOrg(w, r, log, existing.OrgID) {
				return
			}
		}
		content := req.Content
		if source == credential.SourceLocal {
			restored, refuse := restoreRedactedInventoryContent(req.Content, existing.Content)
			if refuse != "" {
				respondError(w, log, http.StatusBadRequest, refuse)
				return
			}
			content = restored
		}
		inv := &inventory.Inventory{
			ID: id, Name: req.Name, Content: content, CredentialIDs: req.CredentialIDs,
			Queue: req.Queue, OrgID: orgID,
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
		respondJSON(w, log, http.StatusOK, redactInventory(r.Context(), updated), wantsPretty(r))
	}
}

// listInventoriesHandler returns the inventories the actor may read. Under strict grants a non-admin
// sees only inventories a grant lets them read; otherwise the global role governs and all are returned.
func listInventoriesHandler(store inventory.Store, authz *authorizer, log *zap.Logger) http.HandlerFunc {
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
		visible, err := filterReadable(r.Context(), authz, list,
			func(i *inventory.Inventory) string { return i.ID },
			func(i *inventory.Inventory) string { return i.OrgID })
		if err != nil {
			log.Error("server: list inventories: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not list inventories")
			return
		}
		respondJSON(w, log, http.StatusOK,
			listInventoriesResponse{Inventories: redactInventories(r.Context(), visible),
				Count: len(visible)}, wantsPretty(r))
	}
}

// redactInventory returns the inventory with its secret-looking variable values masked for a
// non-admin caller, and unchanged for an admin. The create and update responses return the stored
// record, so without this a manager who was served a redacted list, then saved and got the full
// record back, would read the plaintext the list path deliberately hides. It reuses redactInventories
// so one place decides what a non-admin may see.
func redactInventory(ctx context.Context, inv *inventory.Inventory) *inventory.Inventory {
	return redactInventories(ctx, []*inventory.Inventory{inv})[0]
}

// redactInventories removes secret-looking variable values from inventory content unless the caller
// administers the install.
//
// A host list routinely carries an ansible_password or a become password inline, and those are
// credentials. The credential API never returns secret material to a reader, so an inventory that
// happens to hold the same secret as a host variable should not either. Names and hosts survive, so
// the list still reads as an inventory. An admin already holds every secret on the install, so
// redacting for them would only obstruct the person who maintains the file.
//
// The copy is shallow by intent: the stored objects must not be mutated, since these come straight
// from the store and a memory-backed store hands back live pointers.
func redactInventories(ctx context.Context, list []*inventory.Inventory) []*inventory.Inventory {
	if actor, ok := actorFrom(ctx); ok && actor.Role == user.RoleAdmin {
		return list
	}
	out := make([]*inventory.Inventory, 0, len(list))
	for _, inv := range list {
		if inv == nil {
			continue
		}
		clone := *inv
		clone.Content = inventory.Redact(clone.Content)
		out = append(out, &clone)
	}
	return out
}

// restoreRedactedInventoryContent guards a local-inventory update against a caller who echoes back the
// redacted content they were shown. The list endpoint masks inline secrets to inventory.RedactedValue
// for non-admins, so storing that submission verbatim would replace the real ansible_password and the
// like with the mask, destroying the credential and failing every later run silently. When the
// submission is exactly the redacted view of the stored content, the secrets were not touched, so the
// stored content is kept. When it still carries a mask but differs from that view, which secret each
// mask stands for cannot be told, so it is refused rather than guessed at or blanked. It mirrors
// restoreMaskedNotifications for inventory text. refuse is a message and empty on success.
func restoreRedactedInventoryContent(incoming, stored string) (content, refuse string) {
	if !strings.Contains(incoming, inventory.RedactedValue) {
		return incoming, ""
	}
	if inventory.Redact(stored) == incoming {
		return stored, ""
	}
	return "", "the submitted inventory still contains redacted secrets (" + inventory.RedactedValue +
		"); re-enter the secret value for each masked entry, or ask an admin to edit the raw content"
}

// deleteInventoryHandler removes an inventory.
func deleteInventoryHandler(store inventory.Store, refs *refChecker, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if store == nil {
			respondError(w, log, http.StatusNotFound, "inventories not enabled")
			return
		}
		if refs != nil {
			used, err := refs.inventoryRefs(r.Context(), r.PathValue("id"))
			if err != nil {
				log.Error("server: inventory references: " + err.Error())
				respondError(w, log, http.StatusInternalServerError, "could not check inventory references")
				return
			}
			if !used.empty() {
				respondInUse(w, log, "inventory in use", used, wantsPretty(r))
				return
			}
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
