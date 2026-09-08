package relay

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/run"
)

// flakyRelay is a real relay server that can be taken away and brought back, so a test can drive the
// transport across a link failure the way an isolated segment actually loses one.
type flakyRelay struct {
	// inner is the real relay handler every request reaches while the link is up.
	inner http.Handler
	// down, when set, makes every request fail as though nothing were listening.
	down atomic.Bool
	// logPosts counts the log posts that reached the handler.
	logPosts atomic.Int64
}

// ServeHTTP answers with a server fault while the link is down and forwards otherwise.
func (f *flakyRelay) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasSuffix(r.URL.Path, "/log") {
		f.logPosts.Add(1)
	}
	if f.down.Load() {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":"the relay is unreachable"}`))
		return
	}
	f.inner.ServeHTTP(w, r)
}

// newFlakyTransport stands up a flakyRelay over a fresh store, seeds one claimed running run, and
// returns the transport, the relay, and the backing store.
func newFlakyTransport(t *testing.T, runID string) (*httpTransport, *flakyRelay, run.Store) {
	t.Helper()
	backing := run.NewMemStore()
	if err := backing.Save(context.Background(), &run.Run{
		ID: runID, Playbook: "site.yml", Status: run.StatusRunning, CreatedAt: time.Now(),
		ClaimedBy: "worker-a",
	}); err != nil {
		t.Fatalf("seed Save() error = %v", err)
	}
	f := &flakyRelay{inner: NewHandler(backing, SinglePool("tok"), nil, nil, nil)}
	ts := httptest.NewServer(f)
	t.Cleanup(ts.Close)
	tr, ok := NewHTTPTransport(ts.URL, "tok", ts.Client()).(*httpTransport)
	if !ok {
		t.Fatal("NewHTTPTransport() did not return an *httpTransport")
	}
	return tr, f, backing
}

// bufferedBytes reports how much output the transport is holding for a run.
func bufferedBytes(t *testing.T, tr *httpTransport, id string) int {
	t.Helper()
	tr.mu.Lock()
	defer tr.mu.Unlock()
	b := tr.batches[id]
	if b == nil {
		return 0
	}
	return len(b.buf)
}

// waitForLog polls the backing store until the run's stored log reaches want bytes, or the deadline
// passes. Delivery finishes on a timer the test does not own, so waiting on the store is the only
// honest way to observe it.
func waitForLog(t *testing.T, backing run.Store, id string, want int) []byte {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var got []byte
	for time.Now().Before(deadline) {
		var err error
		if got, err = backing.Log(context.Background(), id); err != nil {
			t.Fatalf("backing Log() error = %v", err)
		}
		if len(got) >= want {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
	return got
}

// TestFailedPostPutsItsBytesBackInOrder pins that output a failed post was carrying is not lost and
// not reordered. The bytes go back at the front of the batch, so what the control node finally stores
// is the run's output in the order the tool wrote it. A log that silently drops its middle, or splices
// a later chunk in front of an earlier one, is evidence that reads as a different run than the one
// that happened.
func TestFailedPostPutsItsBytesBackInOrder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tr, flaky, backing := newFlakyTransport(t, "run_requeue")

	flaky.down.Store(true)
	if err := tr.AppendLog(ctx, "run_requeue", []byte("first\n")); err != nil {
		t.Fatalf("AppendLog(first) error = %v", err)
	}
	// The delay flush fires while the link is down, so these bytes have already survived one failure.
	if err := tr.flushLog(ctx, "run_requeue"); err == nil {
		t.Fatal("flushLog() answered nil with the relay down")
	}
	if got := bufferedBytes(t, tr, "run_requeue"); got != len("first\n") {
		t.Fatalf("buffered = %d bytes after a failed post, want the %d that failed put back", got,
			len("first\n"))
	}
	if err := tr.AppendLog(ctx, "run_requeue", []byte("second\n")); err != nil {
		t.Fatalf("AppendLog(second) error = %v", err)
	}

	flaky.down.Store(false)
	if err := tr.flushLog(ctx, "run_requeue"); err != nil {
		t.Fatalf("flushLog() error = %v once the relay returned", err)
	}
	got := waitForLog(t, backing, "run_requeue", len("first\nsecond\n"))
	if diff := cmp.Diff("first\nsecond\n", string(got)); diff != "" {
		t.Errorf("recovered log mismatch (-want +got):\n%s", diff)
	}
}

// TestALandedPostClearsTheBackoff pins that a relay which comes back is not held off by the failures
// that preceded it. Without the reset, a link that blipped once would keep the batch waiting the
// widest backoff for the rest of the run, so a tail-following operator watches a healthy run go quiet.
func TestALandedPostClearsTheBackoff(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tr, flaky, _ := newFlakyTransport(t, "run_backoff")

	flaky.down.Store(true)
	if err := tr.AppendLog(ctx, "run_backoff", []byte("x\n")); err != nil {
		t.Fatalf("AppendLog() error = %v", err)
	}
	for i := range 3 {
		if err := tr.flushLog(ctx, "run_backoff"); err == nil {
			t.Fatalf("flushLog(%d) answered nil with the relay down", i)
		}
	}
	tr.mu.Lock()
	fails, next := tr.batches["run_backoff"].fails, tr.batches["run_backoff"].nextRetry
	tr.mu.Unlock()
	if fails < 3 {
		t.Errorf("consecutive failures = %d, want at least 3 so the wait widens", fails)
	}
	if next.IsZero() {
		t.Error("no retry time was set after a failed post, so a run that goes quiet never " +
			"delivers what it buffered")
	}

	flaky.down.Store(false)
	if err := tr.flushLog(ctx, "run_backoff"); err != nil {
		t.Fatalf("flushLog() error = %v once the relay returned", err)
	}
	tr.mu.Lock()
	defer tr.mu.Unlock()
	b := tr.batches["run_backoff"]
	if b == nil {
		t.Fatal("the batch was dropped for a run that has not finished")
	}
	if b.fails != 0 || !b.nextRetry.IsZero() {
		t.Errorf("after a landed post fails = %d and nextRetry = %v, want the backoff cleared",
			b.fails, b.nextRetry)
	}
}

// TestABackedOffBatchStillSchedulesItsOwnDelivery pins that waiting out a backoff is itself a pending
// flush. A run that fails a post and then goes quiet has no further write to trigger anything, so
// without a timer armed for the retry its buffered output would sit there until the run ended.
func TestABackedOffBatchStillSchedulesItsOwnDelivery(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tr, flaky, backing := newFlakyTransport(t, "run_quiet_after_fail")

	flaky.down.Store(true)
	if err := tr.AppendLog(ctx, "run_quiet_after_fail", []byte("tail\n")); err != nil {
		t.Fatalf("AppendLog() error = %v", err)
	}
	if err := tr.flushLog(ctx, "run_quiet_after_fail"); err == nil {
		t.Fatal("flushLog() answered nil with the relay down")
	}
	tr.mu.Lock()
	armed := tr.batches["run_quiet_after_fail"].timer != nil
	tr.mu.Unlock()
	if !armed {
		t.Fatal("no retry was scheduled, so a run that goes quiet after a failure never delivers " +
			"the output it buffered")
	}

	// Nothing else is written from here. The delivery has to happen on the batch's own timer.
	flaky.down.Store(false)
	got := waitForLog(t, backing, "run_quiet_after_fail", len("tail\n"))
	if diff := cmp.Diff("tail\n", string(got)); diff != "" {
		t.Errorf("the batch did not deliver itself once the relay returned (-want +got):\n%s", diff)
	}
}

// TestOldestOutputIsDroppedFirstOnceTheCapIsReached pins which end of a run's output survives when a
// relay stays down long enough to fill the buffer. The end of the log is what says how the run
// finished, so a buffer that dropped its newest bytes would deliver the opening of a run and lose its
// outcome. It also pins that the survivors are contiguous and in order rather than a mixture.
func TestOldestOutputIsDroppedFirstOnceTheCapIsReached(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tr, flaky, backing := newFlakyTransport(t, "run_capped")
	flaky.down.Store(true)

	// Each chunk carries its own repeated digit, so the surviving window is identifiable on sight.
	var written bytes.Buffer
	const chunks = 12
	for i := range chunks {
		chunk := bytes.Repeat([]byte{byte('0' + i%10)}, logBatchBytes)
		written.Write(chunk)
		if err := tr.AppendLog(ctx, "run_capped", chunk); err != nil {
			t.Fatalf("AppendLog(%d) error = %v", i, err)
		}
	}
	// One more failed post is what applies the cap to everything buffered.
	if err := tr.flushLog(ctx, "run_capped"); err == nil {
		t.Fatal("flushLog() answered nil with the relay down")
	}
	if got := bufferedBytes(t, tr, "run_capped"); got > logBatchLimit {
		t.Fatalf("buffered = %d bytes after the cap was applied, want at most %d", got, logBatchLimit)
	}

	flaky.down.Store(false)
	if err := tr.flushLog(ctx, "run_capped"); err != nil {
		t.Fatalf("flushLog() error = %v once the relay returned", err)
	}
	all := written.Bytes()
	want := all[len(all)-logBatchLimit:]
	got := waitForLog(t, backing, "run_capped", len(want))
	if len(got) != len(want) {
		t.Fatalf("delivered %d bytes, want the last %d of the run's output", len(got), len(want))
	}
	if !bytes.Equal(want, got) {
		t.Error("the delivered window is not the end of the run's output, so a relay that stayed " +
			"down dropped the newest bytes rather than the oldest")
	}
}

// TestBufferedOutputIsHeldToItsCapWhilePostsAreFailing pins the bound the batch limit says it places
// on memory. The bound has to be applied when output is appended and not only when a post fails:
// while the retry backoff is open no post is attempted, so a run producing output quickly would hold
// everything it wrote in that window. The comment on the limit says output past it is dropped rather
// than grown against a relay that is not coming back, and between retries that has to stay true.
func TestBufferedOutputIsHeldToItsCapWhilePostsAreFailing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tr, flaky, _ := newFlakyTransport(t, "run_unbounded")
	flaky.down.Store(true)

	// The first write is a size-triggered flush. It fails, which opens the retry backoff.
	if err := tr.AppendLog(ctx, "run_unbounded", bytes.Repeat([]byte("a"), logBatchBytes)); err != nil {
		t.Fatalf("AppendLog() error = %v", err)
	}
	// Inside that backoff no post is attempted, so nothing applies the cap to what arrives next.
	if err := tr.AppendLog(ctx, "run_unbounded", bytes.Repeat([]byte("b"), 4<<20)); err != nil {
		t.Fatalf("AppendLog(burst) error = %v", err)
	}
	if got := bufferedBytes(t, tr, "run_unbounded"); got > logBatchLimit {
		t.Errorf("buffered = %d bytes for one run while the relay is down, want at most %d", got,
			logBatchLimit)
	}
}

// TestFlushOfARunWithNothingBufferedMakesNoCall pins that flushing a run that wrote nothing costs no
// request. Save flushes on every status change, and a run with no output would otherwise spend a
// request per transition telling the control node it had nothing to say.
func TestFlushOfARunWithNothingBufferedMakesNoCall(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tr, flaky, _ := newFlakyTransport(t, "run_silent")

	if err := tr.flushLog(ctx, "run_silent"); err != nil {
		t.Errorf("flushLog() on a run with no batch error = %v", err)
	}
	// An empty append creates no batch at all, so the next flush is still a no-op.
	if err := tr.AppendLog(ctx, "run_silent", nil); err != nil {
		t.Errorf("AppendLog(nil) error = %v", err)
	}
	if err := tr.flushLog(ctx, "run_silent"); err != nil {
		t.Errorf("flushLog() after an empty append error = %v", err)
	}
	tr.mu.Lock()
	_, held := tr.batches["run_silent"]
	tr.mu.Unlock()
	if held {
		t.Error("an empty append created a batch, so a run that writes nothing still holds one " +
			"entry and one timer for the life of the worker")
	}
	if got := flaky.logPosts.Load(); got != 0 {
		t.Errorf("log posts = %d for a run that wrote nothing, want none", got)
	}
}

// TestPostLogMapsTheRelaysAnswer pins what a failed log post reports. A run purged out from under a
// worker has to surface as run.ErrNotFound so the dispatcher stops working it, and anything else has
// to name the operation and carry the status so an operator can tell a purged run from a broken one.
func TestPostLogMapsTheRelaysAnswer(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name         string
		Body         string
		Status       int
		WantErr      bool
		WantNotFound bool
	}{{ // Test 0: The documented success, which is a 204 with no body.
		Name: "no content", Status: http.StatusNoContent, WantErr: false,
	}, { // Test 1: The run was purged, which the dispatcher branches on.
		Name: "not found", Status: http.StatusNotFound, Body: `{"error":"run not found"}`,
		WantErr: true, WantNotFound: true,
	}, { // Test 2: A store fault, which is a failed report and not a lost run.
		Name: "server fault", Status: http.StatusInternalServerError, Body: `{"error":"store down"}`,
		WantErr: true,
	}, { // Test 3: A 200 is not the documented success and must not be read as one.
		Name: "ok is not no content", Status: http.StatusOK, WantErr: true,
	}, { // Test 4: The body was refused for being too large.
		Name: "too large", Status: http.StatusRequestEntityTooLarge, WantErr: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.Status)
				_, _ = w.Write([]byte(test.Body))
			}))
			t.Cleanup(ts.Close)
			tr, ok := NewHTTPTransport(ts.URL, "tok", ts.Client()).(*httpTransport)
			if !ok {
				t.Fatal("NewHTTPTransport() did not return an *httpTransport")
			}
			if err := tr.AppendLog(context.Background(), "run_1", []byte("out\n")); err != nil {
				t.Fatalf("AppendLog() error = %v", err)
			}
			err := tr.flushLog(context.Background(), "run_1")
			switch {
			case test.WantErr && err == nil:
				t.Fatalf("flushLog() answered nil on %s", test.Name)
			case !test.WantErr && err != nil:
				t.Fatalf("flushLog() error = %v on %s", err, test.Name)
			}
			if test.WantNotFound && !errors.Is(err, run.ErrNotFound) {
				t.Errorf("flushLog() error = %v, want ErrNotFound so the dispatcher stops working "+
					"a run that no longer exists", err)
			}
			if test.WantErr && !test.WantNotFound && !strings.Contains(err.Error(), "append log") {
				t.Errorf("flushLog() error = %q, want it to name the operation", err)
			}
		})
	}
}

// TestTheClaimCapabilityIsDroppedWhenTheRunEnds pins that neither the run's buffered output nor the
// per-claim secret outlives the run. The secret authorizes every report made for that run, so holding
// it after the run's record closes keeps a live credential in memory for nothing; and a map entry per
// run that is never removed is unbounded growth in a worker that runs for weeks.
func TestTheClaimCapabilityIsDroppedWhenTheRunEnds(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	backing := run.NewMemStore()
	if err := backing.Save(ctx, &run.Run{ID: "run_lifecycle", Playbook: "site.yml",
		Status: run.StatusPending, CreatedAt: time.Now()}); err != nil {
		t.Fatalf("seed Save() error = %v", err)
	}
	ts := httptest.NewServer(NewHandler(backing, SinglePool("tok"), nil, nil, nil))
	t.Cleanup(ts.Close)
	tr, ok := NewHTTPTransport(ts.URL, "tok", ts.Client()).(*httpTransport)
	if !ok {
		t.Fatal("NewHTTPTransport() did not return an *httpTransport")
	}

	claimed, err := tr.Claim(ctx, "worker-a", []string{""})
	if err != nil {
		t.Fatalf("Claim() error = %v", err)
	}
	if tr.leaseFor("run_lifecycle") == "" {
		t.Fatal("the claim response carried no capability, so every later report is unauthenticated")
	}
	if err := tr.AppendLog(ctx, "run_lifecycle", []byte("out\n")); err != nil {
		t.Fatalf("AppendLog() error = %v", err)
	}

	claimed.Status = run.StatusSucceeded
	if err := tr.Save(ctx, claimed); err != nil {
		t.Fatalf("Save(succeeded) error = %v", err)
	}
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if _, held := tr.batches["run_lifecycle"]; held {
		t.Error("the run's batch outlived its terminal save, so a long-lived worker keeps one " +
			"entry and one timer per finished run")
	}
	if got := tr.leases["run_lifecycle"]; got != "" {
		t.Error("the per-claim capability outlived the run it authorizes, so a finished run's " +
			"credential stays in memory for the life of the worker")
	}
}

// TestConcurrentAppendsAndFlushesLoseNothing pins that the batch survives the way it is actually
// used: an executor writing output from the process it is running while a delay timer, a size flush,
// and a status save all reach for the same buffer. Run under -race this is the test that would catch
// a torn buffer; without it the failure shows up as a run's log missing lines nobody can reproduce.
func TestConcurrentAppendsAndFlushesLoseNothing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tr, _, backing := newFlakyTransport(t, "run_race")

	const writers, perWriter = 8, 50
	var wg sync.WaitGroup
	for w := range writers {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := range perWriter {
				line := fmt.Sprintf("w%02di%03d\n", w, i)
				if err := tr.AppendLog(ctx, "run_race", []byte(line)); err != nil {
					t.Errorf("AppendLog() error = %v", err)
					return
				}
			}
		}(w)
	}
	// A flusher runs alongside, standing in for the delay timer and the save path.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 20 {
			if err := tr.flushLog(ctx, "run_race"); err != nil {
				t.Errorf("flushLog() error = %v", err)
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	wg.Wait()
	if err := tr.flushLog(ctx, "run_race"); err != nil {
		t.Fatalf("final flushLog() error = %v", err)
	}

	const lineLen = len("w00i000\n")
	want := writers * perWriter * lineLen
	got := waitForLog(t, backing, "run_race", want)
	if len(got) != want {
		t.Fatalf("stored log = %d bytes, want %d: output was lost or duplicated under concurrent "+
			"appends", len(got), want)
	}
	// Every line each writer produced is present exactly once, whatever order the batches went in.
	seen := make(map[string]int, writers*perWriter)
	for _, line := range strings.Split(strings.TrimSuffix(string(got), "\n"), "\n") {
		seen[line]++
	}
	for w := range writers {
		for i := range perWriter {
			line := fmt.Sprintf("w%02di%03d", w, i)
			if seen[line] != 1 {
				t.Fatalf("line %q appears %d times in the stored log, want once", line, seen[line])
			}
		}
	}
}

// markDone stands in for the terminal save that stamps a run's batch as finished. The real path
// reaches this only after a terminal save whose final flush failed, which costs the full tail window;
// setting the stamp directly exercises the same state the abandon and drop rules read.
func markDone(t *testing.T, tr *httpTransport, id string, at time.Time) {
	t.Helper()
	tr.mu.Lock()
	defer tr.mu.Unlock()
	b := tr.batches[id]
	if b == nil {
		t.Fatalf("no batch is held for %q", id)
	}
	b.doneAt = at
	tr.leases[id] = "the-capability-minted-at-claim"
}

// TestAFinishedRunsTailIsDroppedOnceItLands pins that the last of a finished run's output releases
// both the batch and the capability it was reported under.
//
// Dropping it here is what closes a leak the terminal save cannot: a run that ended while the relay
// was briefly down has already been through Save by the time its retry finally succeeds, so nothing
// else would ever remove the entry. On a worker that runs for weeks that is one map entry, one timer,
// and one live credential per finished run, kept for the life of the process.
func TestAFinishedRunsTailIsDroppedOnceItLands(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tr, flaky, backing := newFlakyTransport(t, "run_tail_lands")

	flaky.down.Store(true)
	if err := tr.AppendLog(ctx, "run_tail_lands", []byte("last line\n")); err != nil {
		t.Fatalf("AppendLog() error = %v", err)
	}
	if err := tr.flushLog(ctx, "run_tail_lands"); err == nil {
		t.Fatal("flushLog() answered nil with the relay down")
	}
	markDone(t, tr, "run_tail_lands", time.Now())

	flaky.down.Store(false)
	if err := tr.flushLog(ctx, "run_tail_lands"); err != nil {
		t.Fatalf("flushLog() error = %v once the relay returned", err)
	}
	got := waitForLog(t, backing, "run_tail_lands", len("last line\n"))
	if diff := cmp.Diff("last line\n", string(got)); diff != "" {
		t.Errorf("the finished run's tail did not land (-want +got):\n%s", diff)
	}
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if _, held := tr.batches["run_tail_lands"]; held {
		t.Error("the batch outlived the delivery of a finished run's last output")
	}
	if tr.leases["run_tail_lands"] != "" {
		t.Error("the per-claim capability outlived the last report it could authorize")
	}
}

// TestAFinishedRunsTailIsAbandonedRatherThanRetriedForever pins the other end of the same lifecycle. A
// run that ended a quarter of an hour ago is not going to deliver its tail, and re-arming the retry
// anyway is what kept a dead relay's finished runs holding a live timer and up to the buffer limit of
// output for the life of the worker. The batch and the capability go together, because once the tail
// is abandoned nothing more will ever be reported for that run.
func TestAFinishedRunsTailIsAbandonedRatherThanRetriedForever(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tr, flaky, _ := newFlakyTransport(t, "run_tail_abandoned")

	flaky.down.Store(true)
	if err := tr.AppendLog(ctx, "run_tail_abandoned", []byte("never lands\n")); err != nil {
		t.Fatalf("AppendLog() error = %v", err)
	}
	if err := tr.flushLog(ctx, "run_tail_abandoned"); err == nil {
		t.Fatal("flushLog() answered nil with the relay down")
	}
	// The run finished longer ago than the abandon window, and the relay is still not answering.
	markDone(t, tr, "run_tail_abandoned", time.Now().Add(-logAbandonAfter-time.Minute))
	if err := tr.flushLog(ctx, "run_tail_abandoned"); err == nil {
		t.Fatal("flushLog() answered nil with the relay down")
	}

	tr.mu.Lock()
	defer tr.mu.Unlock()
	if _, held := tr.batches["run_tail_abandoned"]; held {
		t.Error("a finished run kept retrying past the abandon window, so a dead relay leaves one " +
			"entry, one timer, and up to the buffer limit of output alive per run")
	}
	if tr.leases["run_tail_abandoned"] != "" {
		t.Error("the per-claim capability outlived the abandoned tail, so a finished run's " +
			"credential stays in memory with nothing left to authorize")
	}
}
