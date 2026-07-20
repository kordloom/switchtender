package server

import (
	"net/http"
	"strconv"
	"time"

	"go.uber.org/zap"

	"github.com/dcadolph/switchtender/internal/audit"
)

// auditResponse wraps the audit trail.
type auditResponse struct {
	// Entries is the trail, newest first.
	Entries []*audit.Entry `json:"entries"`
	// Count is the number returned.
	Count int `json:"count"`
}

// auditHandler returns recent audit entries.
func auditHandler(store audit.Store, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if store == nil {
			respondError(w, log, http.StatusNotFound, "audit trail not enabled")
			return
		}
		limit := 100
		if v := r.URL.Query().Get("limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				limit = n
			}
		}
		entries, err := store.List(r.Context(), limit)
		if err != nil {
			log.Error("server: list audit entries: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not read the audit trail")
			return
		}
		respondJSON(w, log, http.StatusOK,
			auditResponse{Entries: entries, Count: len(entries)}, wantsPretty(r))
	}
}

// auditVerifyResponse reports whether the audit hash chain is intact.
type auditVerifyResponse struct {
	// OK is true when every entry's hash and link check out.
	OK bool `json:"ok"`
	// Count is the number of entries checked.
	Count int `json:"count"`
	// BrokeAt is the one-based position of the first tampered entry, zero when the chain is intact.
	BrokeAt int `json:"broke_at,omitempty"`
}

// auditVerifyHandler recomputes the audit hash chain and reports whether it is intact, so an
// operator can prove the trail has not been altered.
func auditVerifyHandler(store audit.Store, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if store == nil {
			respondError(w, log, http.StatusNotFound, "audit trail not enabled")
			return
		}
		entries, err := store.Chain(r.Context())
		if err != nil {
			log.Error("server: chain audit entries: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not read the audit trail")
			return
		}
		ok, brokeAt := audit.Verify(entries)
		respondJSON(w, log, http.StatusOK,
			auditVerifyResponse{OK: ok, Count: len(entries), BrokeAt: brokeAt}, wantsPretty(r))
	}
}

// auditExportHandler returns a portable, self-verifying snapshot of the audit chain, signed when an
// audit signer is configured, so the trail can be verified offline.
func auditExportHandler(store audit.Store, signer *audit.Signer, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if store == nil {
			respondError(w, log, http.StatusNotFound, "audit trail not enabled")
			return
		}
		entries, err := store.Chain(r.Context())
		if err != nil {
			log.Error("server: chain audit entries: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not read the audit trail")
			return
		}
		respondJSON(w, log, http.StatusOK, audit.BuildExport(entries, signer, time.Now()), wantsPretty(r))
	}
}
