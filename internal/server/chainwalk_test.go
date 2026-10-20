package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/run"
)

// overlapAudits wraps an audit store and records the most chain walks it ever saw running at once.
// Each walk lingers long enough that two unserialized walks started together overlap.
type overlapAudits struct {
	// Store is the store underneath.
	audit.Store
	// running is how many walks are in flight.
	running atomic.Int32
	// most is the largest value running reached.
	most atomic.Int32
	// scans is how many walks started.
	scans atomic.Int32
}

// ChainScan records the walk's overlap with any other and passes it through.
func (o *overlapAudits) ChainScan(ctx context.Context, after int64, fn func(*audit.Entry) error) error {
	o.scans.Add(1)
	n := o.running.Add(1)
	defer o.running.Add(-1)
	for {
		m := o.most.Load()
		if n <= m || o.most.CompareAndSwap(m, n) {
			break
		}
	}
	time.Sleep(20 * time.Millisecond)
	return o.Store.ChainScan(ctx, after, fn)
}

// TestChainWalksNeverOverlapAcrossTheServer is the guard on read-pool starvation. A full walk holds
// one store read connection for as long as the chain takes to stream, and SQLite has four, so
// concurrent verifies, bundles, and chain health once took every connection between them and
// stalled each other read the server answers. Through the server's own routes, however many walk
// at once, the store never sees two walks in flight.
func TestChainWalksNeverOverlapAcrossTheServer(t *testing.T) {
	t.Parallel()
	audits := &overlapAudits{Store: seedChain(t, 50)}
	id, err := audit.LoadIdentity(t.TempDir())
	if err != nil {
		t.Fatalf("LoadIdentity() error = %v", err)
	}
	handler := New(run.NewMemStore(), &fakeSubmitter{}, zap.NewNop(), WithAudit(audits),
		WithProducerIdentity(&id, "v-test")).Handler()

	paths := []string{"/v1/audit/verify", "/v1/audit/verify", "/v1/audit/bundle",
		"/v1/audit/bundle", "/metrics"}
	start := make(chan struct{})
	var wg sync.WaitGroup
	codes := make([]int, len(paths))
	for i, path := range paths {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
			codes[i] = rec.Code
		}()
	}
	close(start)
	wg.Wait()

	for i, code := range codes {
		if code >= http.StatusInternalServerError {
			t.Errorf("%s status = %d, want an answer, not a fault", paths[i], code)
		}
	}
	if got := audits.scans.Load(); got < 4 {
		t.Fatalf("chain walks = %d, want at least the four verifies and bundles", got)
	}
	if got := audits.most.Load(); got != 1 {
		t.Errorf("chain walks in flight at once = %d, want 1", got)
	}
}

// TestAnOverCeilingBundleIsRefusedBeforeTheWalk pins that a bundle that could never be served costs
// nothing. The limit used to be judged after the whole chain was walked, so a request asking for
// more than one bundle holds paid a full walk to be told no.
func TestAnOverCeilingBundleIsRefusedBeforeTheWalk(t *testing.T) {
	t.Parallel()
	id, err := audit.LoadIdentity(t.TempDir())
	if err != nil {
		t.Fatalf("LoadIdentity() error = %v", err)
	}
	for _, limit := range []string{strconv.Itoa(maxBundleEntries + 1), "abc", "0"} {
		counting := &countingAudits{Store: seedChain(t, 3)}
		rec := httptest.NewRecorder()
		auditBundleHandler(counting, &id, "", "v-test", zap.NewNop()).ServeHTTP(rec,
			httptest.NewRequest(http.MethodGet, "/v1/audit/bundle?limit="+limit, nil))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("limit=%s status = %d, want 400", limit, rec.Code)
		}
		if counting.scans != 0 {
			t.Errorf("limit=%s walked the chain %d times before refusing, want 0", limit,
				counting.scans)
		}
	}
}

// slowAudits wraps an audit store and moves a test clock forward by took on every walk, standing in
// for a chain long enough that walking it takes that long.
type slowAudits struct {
	// Store is the store underneath.
	audit.Store
	// now is the test clock the walk advances.
	now *time.Time
	// took is how far each walk advances it.
	took time.Duration
	// scans is how many walks started.
	scans int
}

// ChainScan advances the clock by took and passes the walk through.
func (s *slowAudits) ChainScan(ctx context.Context, after int64, fn func(*audit.Entry) error) error {
	s.scans++
	*s.now = s.now.Add(s.took)
	return s.Store.ChainScan(ctx, after, fn)
}

// TestChainHealthStretchesItsIntervalOnALongWalk pins chain health's duty cycle. On a chain whose
// walk takes 2 s, a 15 s interval would hold a read connection more than a tenth of the time, so the
// next walk waits ten times the last walk's duration instead.
func TestChainHealthStretchesItsIntervalOnALongWalk(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	slow := &slowAudits{Store: seedChain(t, 3), now: &now, took: 2 * time.Second}
	health := newChainHealth(slow, audit.Identity{})
	health.clock = func() time.Time { return now }
	start := now

	health.snapshot(ctx)
	now = start.Add(16 * time.Second)
	health.snapshot(ctx)
	if slow.scans != 1 {
		t.Errorf("walks 16 s after a 2 s walk = %d, want 1: the next waits 20 s", slow.scans)
	}
	now = start.Add(21 * time.Second)
	health.snapshot(ctx)
	if slow.scans != 2 {
		t.Errorf("walks 21 s after a 2 s walk = %d, want 2", slow.scans)
	}
}

// TestChainHealthServesStaleWhileAnotherWalkRuns pins that a scrape never queues behind a verify. A
// refresh that finds the walk gate held serves its last reading marked stale, without touching the
// store, and the next scrape after the gate frees walks again.
func TestChainHealthServesStaleWhileAnotherWalkRuns(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	counting := &countingAudits{Store: seedChain(t, 3)}
	health := newChainHealth(counting, audit.Identity{})
	health.minInterval = 0

	release, err := health.walks.acquire(ctx)
	if err != nil {
		t.Fatalf("acquire() error = %v", err)
	}
	if got := health.snapshot(ctx); !got.Stale || got.Verified {
		t.Errorf("snapshot while another walk runs = %+v, want stale and not verified", got)
	}
	if counting.scans != 0 {
		t.Errorf("chain walks while the gate was held = %d, want 0", counting.scans)
	}
	release()
	if got := health.snapshot(ctx); got.Stale || !got.Verified {
		t.Errorf("snapshot once the gate freed = %+v, want a fresh verified reading", got)
	}
	if counting.scans != 1 {
		t.Errorf("chain walks once the gate freed = %d, want 1", counting.scans)
	}
}

// TestAWaitingWalkerThatGivesUpNeverRuns pins that a request canceled while waiting for the walk gate
// leaves without walking, so a queue of abandoned verifies cannot keep the store busy after its
// callers have gone.
func TestAWaitingWalkerThatGivesUpNeverRuns(t *testing.T) {
	t.Parallel()
	walks := newChainWalks()
	release, err := walks.acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire() error = %v", err)
	}
	defer release()
	ran := false
	h := walks.hold(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { ran = true }))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h.ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequestWithContext(ctx, http.MethodGet, "/v1/audit/verify", nil))
	if ran {
		t.Error("a walker whose caller gave up ran anyway")
	}
}

// TestChainHealthCatchesAnOldEntryRewrittenInPlace is the attack the full walk exists for. An entry
// 40,000 back is rewritten in place after a clean walk. A walk extended from a verified prefix would
// never re-read it, which is why chain health keeps walking every entry: the next walk past the
// interval reports the break at that entry's position.
func TestChainHealthCatchesAnOldEntryRewrittenInPlace(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("seeds a 41,000-entry chain")
	}
	ctx := context.Background()
	chain := &switchableChain{Store: seedChain(t, 41_000), at: 1_000}
	health := newChainHealth(chain, audit.Identity{})
	now := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	health.clock = func() time.Time { return now }

	if got := health.snapshot(ctx); !got.Verified || got.Entries != 41_000 {
		t.Fatalf("first snapshot = %+v, want 41,000 verified entries", got)
	}
	chain.tampered.Store(true)
	now = now.Add(health.interval())
	got := health.snapshot(ctx)
	if got.Verified || got.BrokeAt != 1_000 {
		t.Errorf("snapshot one interval after the rewrite = %+v, want the break at 1000", got)
	}
}
