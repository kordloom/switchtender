package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
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

// bundleWindow decides how many of the chain's newest entries a bundle may assemble, from the chain's
// size rather than a materialized slice, and reports the message to answer with when the request
// cannot be served as asked. An empty limit means the whole chain, up to the ceiling.
func bundleWindow(count int, limit string) (int, string) {
	if limit == "" {
		if count > maxBundleEntries {
			return 0, fmt.Sprintf("this chain holds %d entries, more than the %d one bundle "+
				"assembles at once. Ask for a window with limit=<count>, which bundles that many of "+
				"the newest entries", count, maxBundleEntries)
		}
		return count, ""
	}
	n, err := strconv.Atoi(limit)
	if err != nil || n < 1 {
		return 0, "limit must be a count of the newest entries to bundle, such as limit=1000"
	}
	if n > maxBundleEntries {
		return 0, fmt.Sprintf("limit must be at most %d, the number of entries one bundle "+
			"assembles at once", maxBundleEntries)
	}
	if n >= count {
		return count, ""
	}
	return n, ""
}

// The reasons a bundle download refuses. Each names a state of the stored chain, never a fault in
// this server, so a caller branches on the code without reading the message.
const (
	// reasonChainBreak means the stored chain does not recompute: an entry was altered, reordered,
	// or removed. This is the tamper the audit trail exists to surface.
	reasonChainBreak = "chain_break"
	// reasonAnchorUnsatisfied means the chain no longer reaches an anchor recorded over it, which is
	// how a chain that hash-verifies but has lost its tail is caught.
	reasonAnchorUnsatisfied = "anchor_unsatisfied"
	// reasonChainUnbundlable means the builder refused the chain for a state the chain walk does not
	// cover, such as an entry recorded at nanosecond precision or a span beat that does not advance.
	reasonChainUnbundlable = "chain_unbundlable"
)

// bundleRefusal is the body an endpoint answers with when the chain itself is why no signed
// artifact can be published. The bundle download and the run receipt both answer with it.
//
// It is deliberately more than an error string. A detected tamper is the one answer these endpoints
// exist to be able to give, and an operator holding it has to be able to tell a chain this server
// checked and rejected from this server having faulted, without reading prose. The coordinates are
// the same ones GET /v1/audit/verify reports, so all three answers line up. One type rather than
// one per endpoint is what keeps them lined up: a caller parses a refusal without knowing which
// signed artifact it asked for.
type bundleRefusal struct {
	// Error is the human-readable refusal, naming what failed and where in the chain.
	Error string `json:"error"`
	// Reason is the stable code for the refusal: chain_break, anchor_unsatisfied, or
	// chain_unbundlable.
	Reason string `json:"reason"`
	// BrokeAt is the one-based position of the first entry that does not verify, zero when the
	// refusal is not a chain break.
	BrokeAt int `json:"broke_at,omitempty"`
	// BrokeSeq is the chain sequence number of that entry, zero when the refusal is not a chain
	// break or the entry carries no readable sequence, which is itself a shape tampering takes.
	BrokeSeq int64 `json:"broke_seq,omitempty"`
	// Count is the number of entries walked.
	Count int `json:"count"`
	// AnchorProblems describes each anchor the chain no longer satisfies, empty for other refusals.
	AnchorProblems []string `json:"anchor_problems,omitempty"`
}

// chainBreakMessage states where a walk of the stored chain found it broken. artifact names what
// could not be published as a result, so the same coordinates read correctly for a bundle and for a
// receipt.
//
// The sequence is named only when the breaking entry carried a readable one, since a row blanked
// out is exactly the tamper that leaves none, and "sequence 0" would read as a fact, not a gap.
func chainBreakMessage(artifact string, brokeAt, count int, seq int64) string {
	where := fmt.Sprintf("at entry %d of %d", brokeAt, count)
	if seq > 0 {
		where += fmt.Sprintf(", sequence %d", seq)
	}
	return "the audit chain does not verify " + where + ", so it cannot be published as " + artifact +
		" any verifier would accept. An entry was altered, reordered, or removed. This is a break " +
		"detected in the stored chain, not a fault in this server. GET /v1/audit/verify reports " +
		"the same position"
}

// chainVerdict is what one full walk of the stored chain found: whether every link recomputes, and
// where the walk stopped believing it if not.
type chainVerdict struct {
	// OK reports that every entry's hash and link recomputed from genesis.
	OK bool
	// BrokeAt is the one-based position of the first entry that does not verify, zero when OK.
	BrokeAt int
	// BrokeSeq is the chain sequence number of that entry, zero when OK or when the entry carries no
	// readable sequence, which is itself a shape tampering takes.
	BrokeSeq int64
	// Count is how many entries the walk fed.
	Count int
	// Highest is the largest sequence the walk saw, which is the chain's head position.
	Highest int64
}

// walkChain streams the whole stored chain once, feeding the link scanner and, when one is given,
// the anchor scanner, and reports what the link walk found.
//
// Every endpoint that signs something drawn from the chain has to hold the whole chain first, and
// has to name a break at a coordinate in the trail rather than at an offset into a walk the caller
// cannot see. Both the bundle export and the run receipt need that, so the walk lives here once. It
// streams rather than materializing: an audit chain grows for the life of an install and these
// endpoints are reachable from a browser.
func walkChain(ctx context.Context, store audit.Store,
	anchorScan *audit.AnchorScanner) (chainVerdict, error) {
	chainScan := audit.NewChainScanner(true)
	var v chainVerdict
	err := store.ChainScan(ctx, 0, func(e *audit.Entry) error {
		chainScan.Feed(e)
		if anchorScan != nil {
			anchorScan.Feed(e)
		}
		if e == nil {
			return nil
		}
		v.Highest = e.Seq
		// The scanner marks a break at the position it was fed, and never moves it, so the one entry
		// whose position equals the count fed so far is the entry that broke the chain. Capturing its
		// sequence here is what lets the refusal name a coordinate in the trail.
		if _, at, walked := chainScan.Result(); at != 0 && at == walked {
			v.BrokeSeq = e.Seq
		}
		return nil
	})
	if err != nil {
		return chainVerdict{}, err
	}
	v.OK, v.BrokeAt, v.Count = chainScan.Result()
	return v, nil
}

// unsatisfiedAnchors returns the problem text for each anchor the chain no longer satisfies, and
// whether any were found. A chain can hash-verify perfectly and still have lost its tail, because a
// prefix of a valid chain is itself a valid chain, so this is the half of the check the link walk
// cannot answer.
func unsatisfiedAnchors(scan *audit.AnchorScanner, recorded int) ([]string, bool) {
	if scan == nil || recorded == 0 {
		return nil, false
	}
	reachedAll, results := scan.Results()
	if reachedAll {
		return nil, false
	}
	problems := make([]string, 0, len(results))
	for _, res := range results {
		if !res.Reached {
			problems = append(problems, res.Problem)
		}
	}
	return problems, true
}

// exportRefusal returns the builder's own words for why the chain could not be bundled, without the
// sentinel prefix that names the operation rather than the problem.
func exportRefusal(err error) string {
	return strings.TrimPrefix(err.Error(), audit.ErrExport.Error()+": ")
}

// auditBundleHandler assembles and serves the signed LoomSeal bundle the CLI produces, so the
// offline-verifiable artifact no rival emits is one click from the audit view rather than only in a
// terminal. It assembles the document the bundle command assembles: hold the whole chain against
// every anchor recorded over it and refuse a chain that cannot reach one, attach the anchors, and
// sign. It is stricter than the command in one place, and deliberately: the hash chain is
// recomputed in full before any window is applied, so a break older than a windowed range is
// refused here rather than signed over.
//
// A chain this endpoint checked and rejected is answered with a 409 and a bundleRefusal naming what
// failed and where, never a 500. A broken chain is the answer this endpoint exists to be able to
// give, and an operator who cannot tell it from a crashed server has been told nothing. A 500 is
// left to what is a fault here, such as a store that will not read or a signature that will not
// form, and a limit that is not a count stays a 400.
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
		// The chain is streamed rather than materialized: the first pass counts it and holds it
		// against every anchor in full, before any window is applied, the same way the command does
		// it, and only the second pass assembles the window the response actually carries. Loading
		// the whole history to then slice its tail was the cap defeating its own purpose.
		recorded, aerr := anchorsFor(r.Context(), store)
		if aerr != nil {
			log.Error("server: read anchors: " + aerr.Error())
			respondError(w, log, http.StatusInternalServerError, "could not read the anchors")
			return
		}
		// The hash chain rides the same pass as the anchors. It has to be walked here rather than
		// left to the builder for two reasons. The builder only ever sees the window, so a break
		// before a windowed range was never looked at and the endpoint signed a clean-looking bundle
		// over the tail of a chain it could see was broken. And the builder reports a break by
		// failing, which arrived as a bare 500: indistinguishable from a crashed server, on the one
		// question this endpoint exists to answer.
		anchorScan := audit.NewAnchorScanner(recorded, producer.InstallID)
		verdict, err := walkChain(r.Context(), store, anchorScan)
		if err != nil {
			log.Error("server: chain audit entries: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not read the audit trail")
			return
		}
		count, highest := verdict.Count, verdict.Highest
		if count == 0 {
			respondError(w, log, http.StatusConflict, "the audit chain is empty, there is nothing to bundle")
			return
		}
		if !verdict.OK {
			log.Error("server: audit bundle refused: the stored audit chain does not verify",
				zap.Int("broke_at", verdict.BrokeAt), zap.Int64("broke_seq", verdict.BrokeSeq),
				zap.Int("entries", count))
			respondJSON(w, log, http.StatusConflict, bundleRefusal{
				Error:  chainBreakMessage("a bundle", verdict.BrokeAt, count, verdict.BrokeSeq),
				Reason: reasonChainBreak, BrokeAt: verdict.BrokeAt, BrokeSeq: verdict.BrokeSeq,
				Count: count,
			}, wantsPretty(r))
			return
		}
		if problems, unsatisfied := unsatisfiedAnchors(anchorScan, len(recorded)); unsatisfied {
			log.Error("server: audit bundle refused: the chain does not satisfy every anchor "+
				"recorded over it", zap.Strings("anchor_problems", problems))
			respondJSON(w, log, http.StatusConflict, bundleRefusal{
				Error: "the chain does not satisfy every anchor recorded over it, so it cannot " +
					"be published as a bundle that does. This is a problem detected in the " +
					"stored chain, not a fault in this server",
				Reason: reasonAnchorUnsatisfied, Count: count, AnchorProblems: problems,
			}, wantsPretty(r))
			return
		}
		window, msg := bundleWindow(count, r.URL.Query().Get("limit"))
		if msg != "" {
			respondError(w, log, http.StatusBadRequest, msg)
			return
		}
		entries := make([]*audit.Entry, 0, window)
		err = store.ChainScan(r.Context(), highest-int64(window), func(e *audit.Entry) error {
			entries = append(entries, e)
			return nil
		})
		if err != nil {
			log.Error("server: chain audit entries: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not read the audit trail")
			return
		}
		doc, err := audit.BuildBundle(entries, *producer, version, time.Now())
		if err != nil {
			// A refusal from the builder is a statement about the chain, not a fault in this server.
			// The walk above catches the ordinary tamper; this catches the rest, such as an entry
			// recorded before times were truncated or a span beat that does not advance past the one
			// before it. Both mean the stored entries would be rejected by a verifier, and both used
			// to arrive as a 500 telling the operator this server had crashed.
			if errors.Is(err, audit.ErrExport) {
				log.Error("server: audit bundle refused: " + err.Error())
				respondJSON(w, log, http.StatusConflict, bundleRefusal{
					Error: exportRefusal(err), Reason: reasonChainUnbundlable, Count: count,
				}, wantsPretty(r))
				return
			}
			log.Error("server: build bundle: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not assemble the bundle")
			return
		}
		doc.AttachAnchors(recorded)
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
