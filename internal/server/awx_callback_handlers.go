package server

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/template"
)

// awxCallbackPrefix is where AWX serves its job templates' callback addresses. It is the only AWX
// API path SwitchTender answers: boot scripts and machine images carry this address and are the
// slowest thing to change in a move off AWX, and nothing else an AWX client calls is aliased.
const awxCallbackPrefix = "/api/v2/job_templates/"

// maxAWXIDDigits bounds the id an AWX callback address may carry, so a path of digits longer than
// any AWX primary key is refused before it is parsed.
const maxAWXIDDigits = 18

// awxLookup is what resolving an AWX job template id for a callback came to.
type awxLookup int

const (
	// awxFound means the id reaches a template that accepts callbacks there with this key.
	awxFound awxLookup = iota
	// awxRefused means the id is unknown, its template does not accept callbacks or answers only
	// its own address, or the key is wrong. All of them answer the caller alike.
	awxRefused
	// awxGone means the id was bound to a template that has since been deleted.
	awxGone
	// awxFailed means the lookup itself failed.
	awxFailed
)

// registerAWX serves the AWX-compatible callback address, POST
// /api/v2/job_templates/{id}/callback with or without its trailing slash, for a template an import
// bound to that AWX job template id.
//
// No GET is served. AWX's GET on this address can return the host config key, and SwitchTender never
// returns a key, so a GET is refused by the mux with 405 and an Allow header naming POST.
func (c *callbacks) registerAWX(mux *http.ServeMux) {
	h := c.awx()
	mux.Handle("POST "+awxCallbackPrefix+"{id}/callback", h)
	mux.Handle("POST "+awxCallbackPrefix+"{id}/callback/{$}", h)
}

// awx serves the AWX-compatible callback address. The rate limits are spent before anything is
// looked up, and an unknown id, a template with callbacks or this address off, and a wrong key all
// answer exactly as a wrong key on the native address does, so the address tells a caller nothing
// about which ids exist. A template that was deleted answers gone, and never falls through to any
// other template. Everything after the key check is the native callback, so the same host
// matching, limits, replay check, gate, and evidence apply, and the evidence names this address.
func (c *callbacks) awx() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		addr := clientAddr(r)
		if !c.admit(r.Context(), w, addr) {
			return
		}
		if c.templates == nil || c.inventories == nil {
			respondError(w, c.log, http.StatusNotFound, "provisioning callbacks not enabled")
			return
		}
		req, ok := decodeCallback(w, r, c.log)
		if !ok {
			return
		}
		awxID, ok := parseAWXID(r.PathValue("id"))
		if !ok {
			c.refuseKey(r.Context(), w, addr)
			return
		}
		t, found := c.awxTemplate(r.Context(), awxID, req.HostConfigKey)
		switch found {
		case awxRefused:
			c.refuseKey(r.Context(), w, addr)
			return
		case awxGone:
			respondError(w, c.log, http.StatusGone, "the template this AWX callback address reached "+
				"was deleted, so the address no longer launches anything. Point the host at a "+
				"template's own callback address")
			return
		case awxFailed:
			respondError(w, c.log, http.StatusInternalServerError, "could not read the template")
			return
		}
		// When images last used this address is what tells an operator whether it is safe to turn
		// off, so a call with the right key is recorded whatever happens to the launch after it.
		if err := c.templates.MarkAWXCalled(r.Context(), awxID, time.Now()); err != nil {
			c.log.Warn("server: record an AWX callback address call: "+err.Error(),
				zap.Int64("awx_job_template_id", awxID))
		}
		c.launch(w, r, t, req, addr, awxID)
	}
}

// awxTemplate returns the template an AWX job template id reaches when it accepts callbacks there
// and presented is its key, or what stopped the lookup.
func (c *callbacks) awxTemplate(ctx context.Context, awxID int64, presented string) (*template.Template,
	awxLookup) {
	b, err := c.templates.AWXBindingFor(ctx, awxID)
	if errors.Is(err, template.ErrNotFound) {
		return nil, awxRefused
	}
	if err != nil {
		c.log.Error("server: AWX callback address: read binding: " + err.Error())
		return nil, awxFailed
	}
	t, err := c.templates.Get(ctx, b.TemplateID)
	if errors.Is(err, template.ErrNotFound) {
		return nil, awxGone
	}
	if err != nil {
		c.log.Error("server: AWX callback address: read template: " + err.Error())
		return nil, awxFailed
	}
	if !t.AWXCallback || !callbackKeyOpens(t, c.sealer, presented, c.log) {
		return nil, awxRefused
	}
	return t, awxFound
}

// parseAWXID reads an AWX job template id from a callback address: decimal digits only, the shape
// of an AWX primary key, and above zero.
func parseAWXID(s string) (int64, bool) {
	if s == "" || len(s) > maxAWXIDDigits {
		return 0, false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

// isAWXCallbackPath reports whether p is exactly an AWX-compatible callback address: the prefix, an
// id of digits, and callback, with or without one trailing slash. Nothing that only resolves to
// that shape counts, so no spelling a traversal later turns into something else is let past the
// gate on the strength of looking like a callback.
func isAWXCallbackPath(p string) bool {
	rest, ok := strings.CutPrefix(p, awxCallbackPrefix)
	if !ok {
		return false
	}
	rest = strings.TrimSuffix(rest, "/")
	id, tail, ok := strings.Cut(rest, "/")
	if !ok || tail != "callback" {
		return false
	}
	_, valid := parseAWXID(id)
	return valid
}

// withAWXBindings fills in, on each template served, the AWX job template id an import bound to it
// and when a host last called it through the AWX-compatible address. Neither is stored on the
// template: the binding is its own record, which outlives the template.
func withAWXBindings(ctx context.Context, store template.Store, list []*template.Template,
	log *zap.Logger) []*template.Template {
	if store == nil || len(list) == 0 {
		return list
	}
	bindings, err := store.AWXBindings(ctx)
	if err != nil {
		log.Warn("server: read AWX callback bindings: " + err.Error())
		return list
	}
	byTemplate := make(map[string]template.AWXBinding, len(bindings))
	for _, b := range bindings {
		byTemplate[b.TemplateID] = b
	}
	for _, t := range list {
		if t == nil {
			continue
		}
		if b, ok := byTemplate[t.ID]; ok {
			t.AWXJobTemplateID = b.AWXID
			t.AWXCallbackCalledAt = b.LastCalledAt
		}
	}
	return list
}

// awxBindingOf returns the AWX binding that reaches templateID, or nil when none does.
func awxBindingOf(ctx context.Context, store template.Store, templateID string) (*template.AWXBinding,
	error) {
	bindings, err := store.AWXBindings(ctx)
	if err != nil {
		return nil, err
	}
	for _, b := range bindings {
		if b.TemplateID == templateID {
			return &b, nil
		}
	}
	return nil, nil
}

// awxCallbackUnbound is the refusal for turning the AWX-compatible address on for a template no
// import bound an AWX job template id to.
const awxCallbackUnbound = "awx_callback answers on the AWX-compatible address of the AWX job " +
	"template this template was imported from, and only an import binds one: this template has " +
	"none. Hosts reach it at its own callback address"

// awxCallbackRefusal returns why an update may not set the AWX-compatible address switch to on, or
// an empty reason when it may. Turning it off is always allowed, and so is leaving it on. A
// template that does not exist is left to the update, which answers that it was not found.
func awxCallbackRefusal(ctx context.Context, store template.Store, id string,
	existing *template.Template, on bool, log *zap.Logger) (string, int) {
	if !on || existing == nil || existing.AWXCallback {
		return "", 0
	}
	b, err := awxBindingOf(ctx, store, id)
	if err != nil {
		log.Error("server: read AWX callback bindings: " + err.Error())
		return "could not read the template's AWX binding", http.StatusInternalServerError
	}
	if b == nil {
		return awxCallbackUnbound, http.StatusBadRequest
	}
	return "", 0
}
