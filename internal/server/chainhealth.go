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
	// walks is the server's one-walk-at-a-time gate. A refresh that finds another walk running
	// serves its last reading marked stale rather than queuing a scrape behind a verify.
	walks *chainWalks
	// producer is the install identity the tree profile's leaves bind to, needed to check a tree
	// anchor under the name it was taken with. Zero when the producer identity is unavailable,
	// which leaves tree anchors uncheckable and reported as problems rather than silently passed.
	producer audit.Identity
	// mu guards everything below.
	mu sync.Mutex
	// clock reads the time, replaced in tests.
	clock func() time.Time
	// minInterval is the shortest gap between full walks; scrapes inside it serve the cache. Zero
	// walks on every refresh, which tests use to read fresh state.
	minInterval time.Duration
	// lastTook is how long the last full walk ran, which stretches the gap on a long chain.
	lastTook time.Duration
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

// chainHealthDuty is how many times its own duration a walk waits before the next one, so chain
// health holds a read connection at most a tenth of the time however long the chain grows. At
// 201,000 entries a walk took 1.53 s, so the 15 s floor still applies there.
const chainHealthDuty = 10

// chainHealthMaxInterval caps the stretched gap, so one walk that stalls behind a checkpoint or a
// paused disk cannot leave a rewritten entry unreported for ten times the stall.
const chainHealthMaxInterval = 10 * time.Minute

// newChainHealth returns a tracker over the given chain. producer is the install identity the tree
// profile's leaves bind to. It panics on a nil store, a programming error: the caller decides
// whether the audit trail is configured, not this.
func newChainHealth(audits audit.Store, producer audit.Identity) *chainHealth {
	if audits == nil {
		panic("server: newChainHealth: audit store required")
	}
	return &chainHealth{
		audits: audits, walks: newChainWalks(), producer: producer, clock: time.Now,
		minInterval: chainHealthInterval,
	}
}

// interval returns the gap the next walk waits for: the larger of minInterval and chainHealthDuty
// times the last walk's duration, the stretch capped at chainHealthMaxInterval, or zero when
// minInterval is zero.
func (h *chainHealth) interval() time.Duration {
	if h.minInterval == 0 {
		return 0
	}
	return max(h.minInterval, min(chainHealthDuty*h.lastTook, chainHealthMaxInterval))
}

// refresh re-verifies the chain unless a walk ran within the interval. A walk feeds a fresh scanner
// over every entry, so an entry rewritten in place since the last walk is caught. On a read failure,
// or when another walk holds the gate, the previous values stand and the view is marked stale,
// because neither is a broken chain and reporting one as such would page the wrong alarm. The next
// scrape tries again.
func (h *chainHealth) refresh(ctx context.Context) {
	h.mu.Lock()
	defer h.mu.Unlock()

	now := h.clock()
	if h.everWalked && now.Sub(h.lastWalk) < h.interval() {
		return
	}
	release, ok := h.walks.tryAcquire()
	if !ok {
		h.stale = true
		return
	}
	defer release()

	anchors, err := h.anchorsNow(ctx)
	if err != nil {
		h.everWalked = true
		h.stale = true
		return
	}
	chainScan := audit.NewChainScanner(true)
	anchorScan := audit.NewAnchorScanner(anchors, h.producer)
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
	h.lastTook = h.clock().Sub(now)
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
