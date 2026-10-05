package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/dispatch"
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
	// Kind is empty for an inventory with hosts of its own, smart for a host filter over other
	// inventories, or constructed for the constructed plugin over input inventories.
	Kind string `json:"kind,omitempty"`
	// HostFilter is a smart inventory's filter, in AWX host_filter syntax.
	HostFilter string `json:"host_filter,omitempty"`
	// InputIDs are a constructed inventory's input inventories, in order.
	InputIDs []string `json:"input_inventory_ids,omitempty"`
	// SourceVars are a constructed inventory's plugin options as YAML.
	SourceVars string `json:"source_vars,omitempty"`
	// Limit narrows a constructed inventory to the hosts an Ansible pattern matches.
	Limit string `json:"limit,omitempty"`
}

// composed reports whether the request defines a smart or constructed inventory.
func (req createInventoryRequest) composed() bool {
	return req.Kind == inventory.KindSmart || req.Kind == inventory.KindConstructed
}

// applyComposition copies the request's composition onto i and validates the result, returning a
// message and status to answer with, or empty when the inventory is consistent. A constructed
// inventory's inputs must exist and hold hosts of their own.
func applyComposition(ctx context.Context, store inventory.Store, req createInventoryRequest,
	i *inventory.Inventory) (string, int) {
	i.Kind, i.HostFilter, i.SourceVars, i.Limit = req.Kind, strings.TrimSpace(req.HostFilter),
		req.SourceVars, strings.TrimSpace(req.Limit)
	i.InputIDs = append([]string(nil), req.InputIDs...)
	if err := inventory.Validate(i); err != nil {
		return err.Error(), http.StatusBadRequest
	}
	for _, id := range i.InputIDs {
		in, err := store.Get(ctx, id)
		if errors.Is(err, inventory.ErrNotFound) {
			return "input inventory " + id + " not found", http.StatusBadRequest
		}
		if err != nil {
			return "could not read input inventory", http.StatusInternalServerError
		}
		if in.Composed() {
			return "input inventory " + in.Name + " is itself a " + in.Kind +
					" inventory; a constructed inventory reads inventories that hold hosts of their own",
				http.StatusBadRequest
		}
	}
	return "", 0
}

// denyInputs refuses a constructed inventory naming an input the caller may not use, reporting
// whether it answered the request. Naming an inventory as an input reaches its hosts at every
// launch, the same reach a run naming it directly has, so it asks the same question.
func denyInputs(w http.ResponseWriter, r *http.Request, authz *authorizer, log *zap.Logger,
	i *inventory.Inventory) bool {
	return denyOnAuthzError(w, log, authz.authorizeAll(r.Context(), grant.AccessUse, i.InputIDs...))
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
		source, sealed, msg, status := credential.SourceLocal, "", "", 0
		if !req.composed() {
			source, sealed, msg, status = inventorySource(req, "", sealer)
		}
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
		// A queue only means anything if a worker serves it, and every worker is Team. Saving one
		// on Community stored a source whose refreshes could never be claimed.
		if qerr := allowQueue(req.Queue); qerr != nil {
			respondError(w, log, http.StatusForbidden, qerr.Error())
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
		if msg, status := applyComposition(r.Context(), store, req, i); msg != "" {
			respondError(w, log, status, msg)
			return
		}
		if denyInputs(w, r, authz, log, i) {
			return
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
		source, sealed, msg, status := credential.SourceLocal, "", "", 0
		if !req.composed() {
			source, sealed, msg, status = inventorySource(req, existing.ContentConfig, sealer)
		}
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
		// A queue only means anything if a worker serves it, and every worker is Team. Saving one
		// on Community stored a source whose refreshes could never be claimed.
		if qerr := allowQueue(req.Queue); qerr != nil {
			respondError(w, log, http.StatusForbidden, qerr.Error())
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
		if source == credential.SourceLocal && !req.composed() {
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
		if msg, status := applyComposition(r.Context(), store, req, inv); msg != "" {
			respondError(w, log, status, msg)
			return
		}
		if denyInputs(w, r, authz, log, inv) {
			return
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

// errNotComposed answers a preview of an inventory that holds hosts of its own.
const errNotComposed = "only a smart or constructed inventory has hosts to preview"

// InventoryPreviewer resolves a smart or constructed inventory for the actor on the context, the
// way a launch by that actor would.
type InventoryPreviewer interface {
	// PreviewInventory resolves inv, saved or not, drawing only on inputs the actor may use.
	PreviewInventory(ctx context.Context, inv *inventory.Inventory) (*dispatch.Composition, error)
}

// previewInput names one input inventory a preview drew hosts from.
type previewInput struct {
	// ID is the input inventory's id.
	ID string `json:"id"`
	// Name is the input inventory's name.
	Name string `json:"name"`
}

// inventoryPreviewResponse is what a composed inventory resolves to right now. It names hosts and
// inputs and never carries the hosts' variables, which can hold secrets.
type inventoryPreviewResponse struct {
	// Kind is smart or constructed.
	Kind string `json:"kind"`
	// Hosts are the hosts it resolves to, sorted.
	Hosts []string `json:"hosts"`
	// Count is how many hosts it resolves to.
	Count int `json:"count"`
	// Inputs are the inventories it drew hosts from.
	Inputs []previewInput `json:"inputs"`
	// Engine is what resolved it: native, or ansible when Ansible read any of it.
	Engine string `json:"engine,omitempty"`
	// AnsibleCore is the ansible-core version that resolved it, when Ansible did.
	AnsibleCore string `json:"ansible_core,omitempty"`
}

// previewInventoryHandler resolves a composed inventory definition that has not been saved, so the
// form that builds one can show which hosts it would reach before anyone saves it. The caller's
// access bounds the answer exactly as it bounds a launch.
func previewInventoryHandler(store inventory.Store, previewer InventoryPreviewer, authz *authorizer,
	log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if store == nil || previewer == nil {
			respondError(w, log, http.StatusNotFound, "inventory previews not enabled")
			return
		}
		var req createInventoryRequest
		if !decodeStrict(w, log, r.Body, &req) {
			return
		}
		if !req.composed() {
			respondError(w, log, http.StatusBadRequest, errNotComposed)
			return
		}
		inv := &inventory.Inventory{Name: req.Name}
		if req.OrgID != nil {
			inv.OrgID = *req.OrgID
		}
		if msg, status := applyComposition(r.Context(), store, req, inv); msg != "" {
			respondError(w, log, status, msg)
			return
		}
		if denyInputs(w, r, authz, log, inv) {
			return
		}
		respondPreview(w, r, store, previewer, inv, log)
	}
}

// previewSavedInventoryHandler resolves a stored composed inventory as a launch by the caller
// would, so a person can see which hosts a run against it would reach right now.
func previewSavedInventoryHandler(store inventory.Store, previewer InventoryPreviewer, authz *authorizer,
	log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if store == nil || previewer == nil {
			respondError(w, log, http.StatusNotFound, "inventory previews not enabled")
			return
		}
		id := r.PathValue("id")
		if denyOnAuthzError(w, log, authz.authorize(r.Context(), id, grant.AccessUse)) {
			return
		}
		inv, err := store.Get(r.Context(), id)
		if errors.Is(err, inventory.ErrNotFound) {
			respondError(w, log, http.StatusNotFound, "inventory not found")
			return
		}
		if err != nil {
			log.Error("server: read inventory: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not read inventory")
			return
		}
		if !inv.Composed() {
			respondError(w, log, http.StatusBadRequest, errNotComposed)
			return
		}
		respondPreview(w, r, store, previewer, inv, log)
	}
}

// respondPreview resolves inv and answers with its hosts and the inputs it drew them from.
func respondPreview(w http.ResponseWriter, r *http.Request, store inventory.Store, previewer InventoryPreviewer,
	inv *inventory.Inventory, log *zap.Logger) {
	c, err := previewer.PreviewInventory(r.Context(), inv)
	switch {
	case errors.Is(err, inventory.ErrHostFilter), errors.Is(err, inventory.ErrSourceVars),
		errors.Is(err, inventory.ErrComposition), errors.Is(err, inventory.ErrResolve),
		errors.Is(err, inventory.ErrNeedsAnsible), errors.Is(err, inventory.ErrInvalidInventory):
		respondError(w, log, http.StatusUnprocessableEntity, err.Error())
		return
	case err != nil:
		log.Error("server: preview inventory: " + err.Error())
		respondError(w, log, http.StatusInternalServerError, "could not resolve inventory")
		return
	}
	out := inventoryPreviewResponse{
		Kind: c.Resolution.Kind, Hosts: c.Resolution.Hosts, Count: len(c.Resolution.Hosts),
		Inputs: []previewInput{}, Engine: c.Resolution.Engine, AnsibleCore: c.Resolution.AnsibleCore,
	}
	if out.Hosts == nil {
		out.Hosts = []string{}
	}
	for _, id := range c.Resolution.Inputs {
		name := id
		if in, err := store.Get(r.Context(), id); err == nil {
			name = in.Name
		}
		out.Inputs = append(out.Inputs, previewInput{ID: id, Name: name})
	}
	respondJSON(w, log, http.StatusOK, out, wantsPretty(r))
}
