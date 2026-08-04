package server

import (
	"context"
	"strconv"
	"sync"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
)

// chainHealth keeps an incrementally verified view of the audit chain for the metrics endpoint,
// so integrity becomes an alarm that pages rather than a page an operator has to visit. Each
// refresh feeds only the entries appended since the last one, which keeps a scrape cheap however
// long the trail grows; the one full walk happens on the first scrape, and again only when the
// anchor set changes, since an anchor fixes a position the incremental walk has already passed.
type chainHealth struct {
	// audits is the chain being watched.
	audits audit.Store
	// mu guards everything below.
	mu sync.Mutex
	// scanner carries the link verification across refreshes.
	scanner *audit.ChainScanner
	// anchorScanner holds the chain against the anchor set it was built for.
	anchorScanner *audit.AnchorScanner
	// anchorMark fingerprints the anchor set the scanners were fed under; a change rebuilds them.
	anchorMark string
	// lastSeq is the highest sequence fed, the resume cursor.
	lastSeq int64
	// verified, brokeAt, and entries mirror the chain scanner's last result.
	verified bool
	brokeAt  int
	entries  int
	// anchorsTotal, anchorProblems, and lastAnchorAt mirror the anchor check's last result.
	anchorsTotal   int
	anchorProblems int
	lastAnchorAt   time.Time
	// stale reports that the last refresh failed and the values shown are from an earlier one.
	stale bool
}

// newChainHealth returns a tracker over the given chain. It panics on a nil store, a programming
// error: the caller decides whether the audit trail is configured, not this.
func newChainHealth(audits audit.Store) *chainHealth {
	if audits == nil {
		panic("server: newChainHealth: audit store required")
	}
	return &chainHealth{audits: audits, verified: true}
}

// refresh brings the view up to the chain's present end. On error the previous values stand and
// the view is marked stale, because a read failure is not a broken chain and reporting either lie
// would misdirect whoever the alarm wakes up.
func (h *chainHealth) refresh(ctx context.Context) {
	h.mu.Lock()
	defer h.mu.Unlock()

	anchors, err := h.anchorsNow(ctx)
	if err != nil {
		h.stale = true
		return
	}
	mark := anchorFingerprint(anchors)
	if h.scanner == nil || mark != h.anchorMark {
		// The anchor set changed, and an anchor fixes a position already streamed past, so the
		// walk restarts to capture the hash at every anchored position.
		h.scanner = audit.NewChainScanner(true)
		h.anchorScanner = audit.NewAnchorScanner(anchors)
		h.anchorMark = mark
		h.lastSeq = 0
	}
	cursor := h.lastSeq
	err = h.audits.ChainScan(ctx, cursor, func(e *audit.Entry) error {
		h.scanner.Feed(e)
		h.anchorScanner.Feed(e)
		if e.Seq > cursor {
			cursor = e.Seq
		}
		return nil
	})
	if err != nil {
		// The scanners may have been part-fed; their state is still sound, since a later refresh
		// resumes after the last entry they saw.
		h.lastSeq = cursor
		h.stale = true
		return
	}
	h.lastSeq = cursor
	h.stale = false
	h.verified, h.brokeAt, h.entries = h.scanner.Result()
	_, checks := h.anchorScanner.Results()
	h.anchorsTotal, h.anchorProblems = len(checks), 0
	for _, c := range checks {
		if !c.Reached {
			h.anchorProblems++
		}
	}
	h.lastAnchorAt = time.Time{}
	for _, a := range anchors {
		if a.At.After(h.lastAnchorAt) {
			h.lastAnchorAt = a.At
		}
	}
}

// anchorsNow returns the store's anchors, or none when the store keeps no anchors.
func (h *chainHealth) anchorsNow(ctx context.Context) ([]*audit.Anchor, error) {
	anchored, ok := h.audits.(audit.AnchorStore)
	if !ok {
		return nil, nil
	}
	return anchored.Anchors(ctx, 0)
}

// anchorFingerprint names an anchor set: its size and its newest member. Anchors are only ever
// added, so this changes exactly when the set does.
func anchorFingerprint(anchors []*audit.Anchor) string {
	if len(anchors) == 0 {
		return "none"
	}
	last := anchors[len(anchors)-1]
	return last.ID + "@" + strconv.Itoa(len(anchors))
}

// chainGauges is one consistent reading for the metrics page.
type chainGauges struct {
	// Verified, BrokeAt, and Entries report the link walk.
	Verified bool
	BrokeAt  int
	Entries  int
	// AnchorsTotal and AnchorProblems report the anchor check.
	AnchorsTotal   int
	AnchorProblems int
	// LastAnchorAt is when the newest anchor was made, zero when none exist.
	LastAnchorAt time.Time
	// Stale reports the values are from an earlier refresh that could not be updated.
	Stale bool
}

// snapshot refreshes and returns one consistent reading.
func (h *chainHealth) snapshot(ctx context.Context) chainGauges {
	h.refresh(ctx)
	h.mu.Lock()
	defer h.mu.Unlock()
	return chainGauges{
		Verified: h.verified, BrokeAt: h.brokeAt, Entries: h.entries,
		AnchorsTotal: h.anchorsTotal, AnchorProblems: h.anchorProblems,
		LastAnchorAt: h.lastAnchorAt, Stale: h.stale,
	}
}
