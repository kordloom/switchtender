package server

import (
	"encoding/json"
	"errors"
	"net/http"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/factcache"
	"github.com/kordloom/switchtender/internal/grant"
	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/run"
)

// WithFactCache enables the per-host fact cache endpoints backed by the given store. It is the
// store the dispatcher fills when a template with the fact cache on runs.
func WithFactCache(store factcache.Store) Option {
	return func(srv *Server) { srv.factCache = store }
}

// WithFactCacheAdminOnly restricts reading cached facts to admins. By default an operator may read
// them too, given read on the inventory and on the run that gathered them, with secret-looking
// values masked. Clearing them takes manage on the inventory either way.
func WithFactCacheAdminOnly(adminOnly bool) Option {
	return func(srv *Server) { srv.factCacheAdminOnly = adminOnly }
}

// factCacheAdminOnlyRefusal is the answer to a caller below admin on a server that restricts
// reading cached facts to admins.
const factCacheAdminOnlyRefusal = "reading cached facts is restricted to admins on this server " +
	"(--fact-cache-admin-only)"

// refuseFactReadBelowAdmin answers a caller below admin when reading cached facts is restricted to
// admins, and reports whether it did.
func refuseFactReadBelowAdmin(w http.ResponseWriter, r *http.Request, adminOnly bool,
	log *zap.Logger) bool {
	if !adminOnly || actorIsAdmin(r) {
		return false
	}
	respondError(w, log, http.StatusForbidden, factCacheAdminOnlyRefusal)
	return true
}

// listFactsResponse is an inventory's cached hosts, without their fact documents.
type listFactsResponse struct {
	// InventoryID is the inventory the hosts belong to.
	InventoryID string `json:"inventory_id"`
	// Hosts lists each cached host with the size of its facts, the run that gathered them, and
	// when. The documents themselves are read one host at a time.
	Hosts []factcache.Entry `json:"hosts"`
	// Count is the number returned.
	Count int `json:"count"`
}

// factsInventory reads the inventory a fact request names and authorizes want access on it,
// answering the caller and returning nil when the request stops here. Cached facts are the
// inventory's data, so reading them takes read on the inventory and clearing them takes manage.
func factsInventory(w http.ResponseWriter, r *http.Request, inventories inventory.Store,
	facts factcache.Store, authz *authorizer, want grant.Access,
	log *zap.Logger) *inventory.Inventory {
	if inventories == nil || facts == nil {
		respondError(w, log, http.StatusNotFound, "fact cache not enabled")
		return nil
	}
	id := r.PathValue("id")
	if denyOnAuthzError(w, log, authz.authorizeAll(r.Context(), want, id)) {
		return nil
	}
	inv, err := inventories.Get(r.Context(), id)
	if errors.Is(err, inventory.ErrNotFound) {
		respondError(w, log, http.StatusNotFound, "inventory not found")
		return nil
	}
	if err != nil {
		log.Error("server: read inventory: " + err.Error())
		respondError(w, log, http.StatusInternalServerError, "could not read inventory")
		return nil
	}
	return inv
}

// listFactsHandler lists the hosts an inventory holds cached facts for. Each entry names the run
// that gathered it, and a host whose run the caller may not read is left out, the same rule the
// gathered facts on the host page follow.
func listFactsHandler(inventories inventory.Store, facts factcache.Store, store run.Store,
	authz *authorizer, adminOnly bool, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if refuseFactReadBelowAdmin(w, r, adminOnly, log) {
			return
		}
		inv := factsInventory(w, r, inventories, facts, authz, grant.AccessRead, log)
		if inv == nil {
			return
		}
		keep, _, err := derivedReadFilter(r.Context(), authz, store)
		if err != nil {
			log.Error("server: read filter: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not read cached facts")
			return
		}
		entries, err := facts.List(r.Context(), inv.ID, false)
		if err != nil {
			log.Error("server: list cached facts: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not read cached facts")
			return
		}
		visible := make([]factcache.Entry, 0, len(entries))
		for _, e := range entries {
			if e.RunID == "" || keep(e.RunID) {
				visible = append(visible, e)
			}
		}
		respondJSON(w, log, http.StatusOK, listFactsResponse{InventoryID: inv.ID, Hosts: visible,
			Count: len(visible)}, wantsPretty(r))
	}
}

// hostCachedFactsHandler returns one host's cached fact document. Below admin, the values of
// secret-looking keys are masked by the same rule that masks them in inventory content, because a
// fact document routinely carries the remote user's environment.
func hostCachedFactsHandler(inventories inventory.Store, facts factcache.Store, store run.Store,
	authz *authorizer, adminOnly bool, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if refuseFactReadBelowAdmin(w, r, adminOnly, log) {
			return
		}
		inv := factsInventory(w, r, inventories, facts, authz, grant.AccessRead, log)
		if inv == nil {
			return
		}
		keep, _, err := derivedReadFilter(r.Context(), authz, store)
		if err != nil {
			log.Error("server: read filter: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not read cached facts")
			return
		}
		e, err := facts.Facts(r.Context(), inv.ID, r.PathValue("host"))
		if errors.Is(err, factcache.ErrNotFound) {
			respondError(w, log, http.StatusNotFound, "no cached facts for this host")
			return
		}
		if err != nil {
			log.Error("server: read cached facts: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not read cached facts")
			return
		}
		if e.RunID != "" && !keep(e.RunID) {
			respondError(w, log, http.StatusNotFound, "no cached facts for this host")
			return
		}
		if !actorIsAdmin(r) {
			e.Facts = json.RawMessage(inventory.Redact(string(e.Facts)))
		}
		respondJSON(w, log, http.StatusOK, e, wantsPretty(r))
	}
}

// clearHostFactsHandler removes one host's cached facts, so the next run gathers them afresh and an
// operator can take back a document that held something it should not have.
func clearHostFactsHandler(inventories inventory.Store, facts factcache.Store, authz *authorizer,
	log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		inv := factsInventory(w, r, inventories, facts, authz, grant.AccessManage, log)
		if inv == nil {
			return
		}
		host := r.PathValue("host")
		n, err := facts.Clear(r.Context(), inv.ID, host)
		if err != nil {
			log.Error("server: clear cached facts: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not clear cached facts")
			return
		}
		if n == 0 {
			respondError(w, log, http.StatusNotFound, "no cached facts for this host")
			return
		}
		respondJSON(w, log, http.StatusOK, map[string]string{"cleared": host}, wantsPretty(r))
	}
}
