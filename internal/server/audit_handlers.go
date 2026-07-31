package server

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/audit"
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
	// Anchored is the number of anchors the chain was held against.
	Anchored int `json:"anchored"`
	// AnchorProblems describes each anchor the chain no longer satisfies, empty when it satisfies
	// all of them. A chain can hash-verify perfectly and still have lost its tail, because a prefix
	// of a valid chain is itself a valid chain. This is the part that catches that.
	AnchorProblems []string `json:"anchor_problems,omitempty"`
}

// anchorsFor returns every anchor the store keeps, or none when the store keeps no anchors.
//
// An install with no anchors is not a failure. It has simply never fixed a link anywhere this
// install cannot rewrite, so nothing here can tell it whether its tail is intact, and saying so is
// more honest than passing it silently.
func anchorsFor(ctx context.Context, store audit.Store) ([]*audit.Anchor, error) {
	anchors, ok := store.(audit.AnchorStore)
	if !ok {
		return nil, nil
	}
	return anchors.Anchors(ctx, 0)
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
		// The hash chain answers "was anything altered". It cannot answer "is anything missing from
		// the end", because dropping the last entries leaves a chain that still verifies. Anchors
		// are what answer that, and reporting a healthy chain without consulting them was reporting
		// on half the question while the other half sat unread in the same database.
		resp := auditVerifyResponse{OK: ok, Count: len(entries), BrokeAt: brokeAt}
		if anchors, aerr := anchorsFor(r.Context(), store); aerr != nil {
			log.Error("server: read audit anchors: " + aerr.Error())
			respondError(w, log, http.StatusInternalServerError, "could not read the audit anchors")
			return
		} else if len(anchors) > 0 {
			resp.Anchored = len(anchors)
			anchorsOK, results := audit.CheckAnchors(entries, anchors)
			if !anchorsOK {
				resp.OK = false
				for _, res := range results {
					if !res.Reached {
						resp.AnchorProblems = append(resp.AnchorProblems, res.Problem)
					}
				}
			}
		}
		respondJSON(w, log, http.StatusOK, resp, wantsPretty(r))
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
