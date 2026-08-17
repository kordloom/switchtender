package server

import (
	"errors"
	"net/http"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/dossier"
	"github.com/kordloom/switchtender/internal/run"
)

// runEvidenceHandler renders one run's evidence dossier as a self-contained HTML document, so the
// run page answers an auditor's sample request with one export instead of five screenshots.
// installID is the install the tree profile's leaves bind to, which checking a tree anchor
// requires.
func runEvidenceHandler(store run.Store, audits audit.Store, installID string, authz *authorizer,
	log *zap.Logger) http.HandlerFunc {
	if store == nil {
		panic("server: runEvidenceHandler: Store required")
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if audits == nil {
			respondError(w, log, http.StatusNotFound, "audit trail not enabled")
			return
		}
		got, err := store.Get(r.Context(), r.PathValue("id"))
		if err != nil {
			if errors.Is(err, run.ErrNotFound) {
				respondError(w, log, http.StatusNotFound, "run not found")
				return
			}
			log.Error("server: get run: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not get run")
			return
		}
		if authorizeRunAccess(w, r, authz, log, got) {
			return
		}
		// Evidence quotes the audit trail, so it is admin ground with one exception: the actor who asked
		// for this run may read the evidence for it.
		if denyUnlessAdminOrActor(w, r, log, got) {
			return
		}
		in, err := dossier.Collect(r.Context(), store, audits, installID, got.ID, time.Now())
		if err != nil {
			log.Error("server: collect run evidence: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not collect the evidence")
			return
		}
		// The dossier is an HTML page for a person. A caller that asks for JSON gets the same collected
		// record as data, which is what a program, and an AI agent above all, can actually read: the
		// tool that reads a run's evidence used to hand a model a page of markup.
		if r.URL.Query().Get("format") == "json" {
			respondJSON(w, log, http.StatusOK, in, wantsPretty(r))
			return
		}
		doc, err := dossier.Render(in)
		if err != nil {
			log.Error("server: render run evidence: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not render the evidence")
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(doc)
	}
}

// auditRegisterHandler renders the period change register as a self-contained HTML document, the
// change-management evidence a compliance review samples from. installID is the install the tree
// profile's leaves bind to. from and to accept a date or an RFC 3339 time; the period defaults to
// the last 90 days.
func auditRegisterHandler(store run.Store, audits audit.Store, installID string,
	log *zap.Logger) http.HandlerFunc {
	if store == nil {
		panic("server: auditRegisterHandler: Store required")
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if audits == nil {
			respondError(w, log, http.StatusNotFound, "audit trail not enabled")
			return
		}
		to := time.Now()
		if raw := r.URL.Query().Get("to"); raw != "" {
			parsed, err := parseRegisterTime(raw)
			if err != nil {
				respondError(w, log, http.StatusBadRequest, "to must be a date or an RFC 3339 time")
				return
			}
			to = parsed
		}
		from := to.AddDate(0, 0, -90)
		if raw := r.URL.Query().Get("from"); raw != "" {
			parsed, err := parseRegisterTime(raw)
			if err != nil {
				respondError(w, log, http.StatusBadRequest, "from must be a date or an RFC 3339 time")
				return
			}
			from = parsed
		}
		if !from.Before(to) {
			respondError(w, log, http.StatusBadRequest, "from must precede to")
			return
		}
		// The period is caller controlled, so the bound is the store query's and not the reader's
		// good manners. A truncated document says so on its face.
		in, err := dossier.CollectRegister(r.Context(), store, audits, installID, from, to,
			time.Now(), dossier.MaxRegisterRuns)
		if err != nil {
			log.Error("server: collect change register: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not collect the register")
			return
		}
		doc, err := dossier.RenderRegister(in)
		if err != nil {
			log.Error("server: render change register: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not render the register")
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(doc)
	}
}

// parseRegisterTime accepts a date or an RFC 3339 time.
func parseRegisterTime(raw string) (time.Time, error) {
	if t, err := time.Parse("2006-01-02", raw); err == nil {
		return t, nil
	}
	return time.Parse(time.RFC3339, raw)
}
