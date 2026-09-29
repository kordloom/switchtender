package server

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/beatfeed"
	"github.com/kordloom/switchtender/internal/audit"
)

// defaultAuditPage is the page size when an audit read names no limit, and maxAuditPage is the
// largest page one request may ask for. The cap bounds the work a single read can demand; the
// has_more flag is what keeps the capped answer honest about being a page rather than the trail.
const (
	defaultAuditPage = 100
	maxAuditPage     = 1000
)

// auditResponse wraps a page of the audit trail.
type auditResponse struct {
	// Entries is the page, newest first.
	Entries []*audit.Entry `json:"entries"`
	// Count is the number of entries on this page, which is not the size of the trail whenever
	// HasMore is set.
	Count int `json:"count"`
	// HasMore reports whether the trail holds older entries beyond this page.
	HasMore bool `json:"has_more"`
}

// auditHandler returns a page of recent audit entries, newest first.
func auditHandler(store audit.Store, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if store == nil {
			respondError(w, log, http.StatusNotFound, "audit trail not enabled")
			return
		}
		limit := defaultAuditPage
		if v := r.URL.Query().Get("limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				limit = min(n, maxAuditPage)
			}
		}
		// Read one entry past the page so a cut trail is reported as cut. A reader handed a
		// truncated trail with a count equal to its length believes it saw every change there was.
		entries, err := store.List(r.Context(), limit+1)
		if err != nil {
			log.Error("server: list audit entries: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not read the audit trail")
			return
		}
		hasMore := len(entries) > limit
		if hasMore {
			entries = entries[:limit]
		}
		respondJSON(w, log, http.StatusOK,
			auditResponse{Entries: entries, Count: len(entries), HasMore: hasMore}, wantsPretty(r))
	}
}

// defaultBeatLimit caps how many beats the feed returns when the caller sets no limit, and stands
// in for a limit that does not parse or falls outside [1, maxBeatLimit].
const defaultBeatLimit = 1000

// maxBeatLimit is the most beats one feed request may ask for. The feed is unauthenticated, so the
// limit is what bounds the work a stranger can demand per request.
const maxBeatLimit = 10000

// beatLimit returns the feed limit for the request: the limit parameter when it is a number within
// [1, maxBeatLimit], the default otherwise.
func beatLimit(r *http.Request) int {
	v := r.URL.Query().Get("limit")
	if v == "" {
		return defaultBeatLimit
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > maxBeatLimit {
		return defaultBeatLimit
	}
	return n
}

// auditBeatsHandler serves the span beat feed: every well-formed span entry, oldest first, so an
// outside watcher sees a missing or duplicate beat. When more beats exist than the limit, the
// newest are kept and the answer stays oldest first within itself, since a watcher cares about
// the present end of the stream. It is served without authentication for the same reason the
// trust document is: the watcher is the party the record is meant to convince, and has no
// account here, which is also why the store filters the beats rather than the handler walking the
// whole chain: an anonymous request must not cost a full table scan.
func auditBeatsHandler(store audit.Store, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if store == nil {
			respondError(w, log, http.StatusNotFound, "audit trail not enabled")
			return
		}
		entries, err := store.SpanBeats(r.Context(), beatLimit(r))
		if err != nil {
			log.Error("server: span beats: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not read the audit trail")
			return
		}
		beats := []beatfeed.Beat{}
		for _, e := range entries {
			beat, _, _, ok := audit.ParseSpanPath(e.Path)
			if !ok {
				continue
			}
			beats = append(beats, beatfeed.Beat{
				Beat: beat, At: e.At.UTC().Format(time.RFC3339Nano), Seq: e.Seq, Head: e.Hash,
			})
		}
		respondJSON(w, log, http.StatusOK, beats, wantsPretty(r))
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
// operator can prove the trail has not been altered. installID is the install the tree profile's
// leaves bind to, which checking a tree anchor requires.
func auditVerifyHandler(store audit.Store, installID string, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if store == nil {
			respondError(w, log, http.StatusNotFound, "audit trail not enabled")
			return
		}
		// The anchors are read before the walk so both checks ride one streaming pass. The hash
		// chain answers "was anything altered". It cannot answer "is anything missing from the
		// end", because dropping the last entries leaves a chain that still verifies. Anchors are
		// what answer that, and reporting a healthy chain without consulting them was reporting on
		// half the question while the other half sat unread in the same database.
		anchors, aerr := anchorsFor(r.Context(), store)
		if aerr != nil {
			log.Error("server: read audit anchors: " + aerr.Error())
			respondError(w, log, http.StatusInternalServerError, "could not read the audit anchors")
			return
		}
		// The chain streams past both scanners one entry at a time, so verifying years of trail
		// holds one entry in memory rather than all of them, however many clients ask at once.
		chainScan := audit.NewChainScanner(true)
		anchorScan := audit.NewAnchorScanner(anchors, installID)
		err := store.ChainScan(r.Context(), 0, func(e *audit.Entry) error {
			chainScan.Feed(e)
			anchorScan.Feed(e)
			return nil
		})
		if err != nil {
			log.Error("server: chain audit entries: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not read the audit trail")
			return
		}
		ok, brokeAt, count := chainScan.Result()
		resp := auditVerifyResponse{OK: ok, Count: count, BrokeAt: brokeAt}
		if len(anchors) > 0 {
			resp.Anchored = len(anchors)
			anchorsOK, results := anchorScan.Results()
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

// maxBundleEntries is how many entries an unwindowed bundle export will assemble.
//
// A bundle is one signed document over every claim it carries, so unlike the streaming verify it has to
// hold them all at once. An audit chain grows for the life of an install, a row per mutating request,
// per webhook fire, and per span beat, so on a long-lived install an unwindowed export assembles the
// whole history in memory, several times its stored size, on every request. Past this the caller is
// asked to name a window instead, which the command has always offered.
const maxBundleEntries = 250_000

// bundleWindow narrows entries to the newest limit of them, and reports the message to answer with when
// the request cannot be served as asked. An empty limit means the whole chain, up to the ceiling.
func bundleWindow(entries []*audit.Entry, limit string) ([]*audit.Entry, string) {
	if limit == "" {
		if len(entries) > maxBundleEntries {
			return nil, fmt.Sprintf("this chain holds %d entries, more than the %d one bundle "+
				"assembles at once. Ask for a window with limit=<count>, which bundles that many of "+
				"the newest entries", len(entries), maxBundleEntries)
		}
		return entries, ""
	}
	n, err := strconv.Atoi(limit)
	if err != nil || n < 1 {
		return nil, "limit must be a count of the newest entries to bundle, such as limit=1000"
	}
	if n > maxBundleEntries {
		return nil, fmt.Sprintf("limit must be at most %d, the number of entries one bundle "+
			"assembles at once", maxBundleEntries)
	}
	if n >= len(entries) {
		return entries, ""
	}
	return entries[len(entries)-n:], ""
}

// auditBundleHandler assembles and serves the signed LoomSeal bundle the CLI produces, so the
// offline-verifiable artifact no rival emits is one click from the audit view rather than only in a
// terminal. It mirrors the bundle command exactly: build over the whole chain, hold it against every
// anchor recorded over it and refuse a chain that cannot reach one, attach the anchors, and sign.
//
// The signed bytes are written exactly as SignBundleDoc produced them and never re-marshaled. A
// re-encode would change the bytes the signature covers, so an offline verifier would then reject a
// bundle this install actually signed.
func auditBundleHandler(store audit.Store, producer *audit.Identity, version string, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if store == nil || producer == nil {
			respondError(w, log, http.StatusNotFound, "signed bundle export is not enabled")
			return
		}
		entries, err := store.Chain(r.Context())
		if err != nil {
			log.Error("server: chain audit entries: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not read the audit trail")
			return
		}
		if len(entries) == 0 {
			respondError(w, log, http.StatusConflict, "the audit chain is empty, there is nothing to bundle")
			return
		}
		// The chain is held against the anchors in full below, before any window is applied, the same
		// way the command does it: checking a window against the anchors reads a deliberate narrowing
		// as lost history.
		full := entries
		windowed, msg := bundleWindow(entries, r.URL.Query().Get("limit"))
		if msg != "" {
			respondError(w, log, http.StatusBadRequest, msg)
			return
		}
		entries = windowed
		doc, err := audit.BuildBundle(entries, *producer, version, time.Now())
		if err != nil {
			log.Error("server: build bundle: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not assemble the bundle")
			return
		}
		if anchors, ok := store.(audit.AnchorStore); ok {
			recorded, aerr := anchors.Anchors(r.Context(), 0)
			if aerr != nil {
				log.Error("server: read anchors: " + aerr.Error())
				respondError(w, log, http.StatusInternalServerError, "could not read the anchors")
				return
			}
			if reachedAll, _ := audit.CheckAnchors(full, recorded, producer.InstallID); !reachedAll {
				respondError(w, log, http.StatusConflict, "the chain does not satisfy every anchor "+
					"recorded over it, so it cannot be published as a bundle that does")
				return
			}
			doc.AttachAnchors(recorded)
		}
		signed, err := audit.SignBundleDoc(doc, producer.Private())
		if err != nil {
			log.Error("server: sign bundle: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not sign the bundle")
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="switchtender-audit.loomseal.json"`)
		_, _ = w.Write(signed)
	}
}
