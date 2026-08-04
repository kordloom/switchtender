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
func runEvidenceHandler(store run.Store, audits audit.Store, authz *authorizer, log *zap.Logger) http.HandlerFunc {
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
		in, err := dossier.Collect(r.Context(), store, audits, got.ID, time.Now())
		if err != nil {
			log.Error("server: collect run evidence: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not collect the evidence")
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
