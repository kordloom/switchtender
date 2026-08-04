package server

import (
	"context"
	"sync"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
)

// chainHealth keeps a verified view of the audit chain for the metrics endpoint, so integrity
// becomes an alarm that pages rather than a page an operator has to visit. Each refresh re-verifies
// the whole chain, because tamper evidence means catching an in-place rewrite of an already-recorded
// entry, and a forward-only cursor would never re-read the entry that changed. Repeated scrapes
// inside a short window serve the last verdict rather than re-walking, so an aggressive scraper
// cannot turn the endpoint into a busy loop; the window bounds how stale a reading can be.
type chainHealth struct {
	// audits is the chain being verified.
	audits audit.Store
	// mu guards everything below.
	mu sync.Mutex
	// clock reads the time, replaced in tests.
	clock func() time.Time
	// minInterval is the shortest gap between full walks; scrapes inside it serve the cache.
	minInterval time.Duration
	// lastWalk is when the last full walk ran, and everWalked whether one ever succeeded or failed.
	lastWalk   time.Time
	everWalked bool
	// verified, brokeAt, and entries mirror the last walk's chain result. verified starts false so
	// a chain that has never been verified is never reported sound.
	verified bool
	brokeAt  int
	entries  int
	// anchorsTotal, anchorProblems, and lastAnchorAt mirror the last walk's anchor result.
	anchorsTotal   int
	anchorProblems int
	lastAnchorAt   time.Time
	// stale reports the last walk could not run, so these values are from an earlier one or, before
	// any walk, are the unverified defaults. A relying alarm reads verified together with stale:
	// verified==0 with stale==0 is a confirmed break; stale==1 is "could not check".
	stale bool
}

// chainHealthInterval is how long a verified reading is served before the chain is walked again. It
// bounds both the cost of a scrape storm and how long a tamper can sit unreported.
const chainHealthInterval = 15 * time.Second

// newChainHealth returns a tracker over the given chain. It panics on a nil store, a programming
// error: the caller decides whether the audit trail is configured, not this.
func newChainHealth(audits audit.Store) *chainHealth {
	if audits == nil {
		panic("server: newChainHealth: audit store required")
	}
	return &chainHealth{audits: audits, clock: time.Now, minInterval: chainHealthInterval}
}

// refresh re-verifies the chain unless a walk ran within minInterval. A walk feeds a fresh scanner
// over every entry, so an entry rewritten in place since the last walk is caught. On a read failure
// the previous values stand and the view is marked stale, because a read failure is not a broken
// chain and reporting it as one would page the wrong alarm.
func (h *chainHealth) refresh(ctx context.Context) {
	h.mu.Lock()
	defer h.mu.Unlock()

	now := h.clock()
	if h.everWalked && now.Sub(h.lastWalk) < h.minInterval {
		return
	}

	anchors, err := h.anchorsNow(ctx)
	if err != nil {
		h.everWalked = true
		h.stale = true
		return
	}
	chainScan := audit.NewChainScanner(true)
	anchorScan := audit.NewAnchorScanner(anchors)
	err = h.audits.ChainScan(ctx, 0, func(e *audit.Entry) error {
		chainScan.Feed(e)
		anchorScan.Feed(e)
		return nil
	})
	if err != nil {
		h.everWalked = true
		h.stale = true
		return
	}

	h.verified, h.brokeAt, h.entries = chainScan.Result()
	_, checks := anchorScan.Results()
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
	h.lastWalk = now
	h.everWalked = true
	h.stale = false
}

// anchorsNow returns the store's anchors, or none when the store keeps no anchors.
func (h *chainHealth) anchorsNow(ctx context.Context) ([]*audit.Anchor, error) {
	anchored, ok := h.audits.(audit.AnchorStore)
	if !ok {
		return nil, nil
	}
	return anchored.Anchors(ctx, 0)
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
	// Stale reports the values could not be refreshed, or no walk has succeeded yet.
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
