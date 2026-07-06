package server

import (
	"net/http"
	"strconv"

	"go.uber.org/zap"

	"github.com/dcadolph/yardmaster/internal/audit"
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
