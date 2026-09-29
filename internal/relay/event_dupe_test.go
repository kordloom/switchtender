package relay_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/event"
	"github.com/kordloom/switchtender/internal/relay"
	"github.com/kordloom/switchtender/internal/run"
)

// TestARetriedEventFlushDoesNotDuplicateTheRecord covers the run's event record showing work twice.
//
// A worker posts events in batches of a thousand, and the append is wrapped in a retry. The batching
// starts over from the first batch on every attempt, and the server appends whatever it is given with no
// notion of which batch it is, so when the second batch failed the retry re-posted the first one too and
// the stored stream held it twice. A reader of the run's event export then sees every task in those
// batches execute twice, on a run whose approval turned on what it did.
//
// A batch is retried where it failed now, and a write that landed in part reports as much, so nothing
// above it repeats work that already arrived.
func TestARetriedEventFlushDoesNotDuplicateTheRecord(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	backing := run.NewMemStore()

	// A control node that refuses the second batch once, the way a dropped connection does mid-sequence.
	var posts atomic.Int64
	inner := relay.NewHandler(backing, relay.SinglePool(testWorkerToken), nil, nil, nil)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/events") && posts.Add(1) == 2 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	c := relay.NewClient(relay.NewHTTPTransport(ts.URL, testWorkerToken, ts.Client()))

	claimed := time.Now()
	r := &run.Run{
		ID: "run_events", Playbook: "site.yml", Status: run.StatusRunning, CreatedAt: claimed,
		ClaimedBy: "worker-a", ClaimedAt: &claimed, StartedAt: &claimed,
	}
	if err := backing.Save(ctx, r); err != nil {
		t.Fatalf("seed Save() error = %v", err)
	}

	// Wider than one batch, so there is a second batch to fail on.
	const count = 1500
	events := make([]event.Event, 0, count)
	for i := range count {
		events = append(events, event.Event{
			Type: event.TypeRunnerOK, Host: fmt.Sprintf("web-%04d", i), Task: "converge",
		})
	}

	// The worker's own retry, the same wrapper the executor puts around this call.
	var err error
	for attempt := 0; attempt < 4; attempt++ {
		if err = c.AppendEvents(ctx, r.ID, events); err == nil {
			break
		}
	}
	if err != nil {
		t.Fatalf("AppendEvents() never landed: %v", err)
	}

	stored, err := backing.Events(ctx, r.ID)
	if err != nil {
		t.Fatalf("Events() error = %v", err)
	}
	if len(stored) != count {
		t.Errorf("the run's record holds %d events for %d that happened, so a reader of its export sees "+
			"the same tasks execute more than once", len(stored), count)
	}
	// And each host appears once, which is what a reader actually notices.
	seen := map[string]int{}
	for _, e := range stored {
		seen[e.Host]++
	}
	for _, host := range []string{"web-0000", "web-0999", "web-1000", "web-1499"} {
		if seen[host] != 1 {
			t.Errorf("host %s appears %d times in the run's events, want once", host, seen[host])
		}
	}
}

// TestAnAppendThatLandedInPartSaysSo is the other half. An outage that outlasts the batch's own retries
// leaves the earlier batches recorded, so the call has to report that it landed in part rather than
// merely failed: a caller that repeats it records those batches a second time. The executor's retry
// wrapper reads that report and stops.
func TestAnAppendThatLandedInPartSaysSo(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	backing := run.NewMemStore()

	// A control node that takes the first batch and then refuses for good.
	var posts atomic.Int64
	inner := relay.NewHandler(backing, relay.SinglePool(testWorkerToken), nil, nil, nil)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/events") && posts.Add(1) > 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	c := relay.NewClient(relay.NewHTTPTransport(ts.URL, testWorkerToken, ts.Client()))

	claimed := time.Now()
	r := &run.Run{
		ID: "run_partial", Playbook: "site.yml", Status: run.StatusRunning, CreatedAt: claimed,
		ClaimedBy: "worker-a", ClaimedAt: &claimed, StartedAt: &claimed,
	}
	if err := backing.Save(ctx, r); err != nil {
		t.Fatalf("seed Save() error = %v", err)
	}

	const count = 1500
	events := make([]event.Event, 0, count)
	for i := range count {
		events = append(events, event.Event{
			Type: event.TypeRunnerOK, Host: fmt.Sprintf("web-%04d", i), Task: "converge",
		})
	}

	err := c.AppendEvents(ctx, r.ID, events)
	if err == nil {
		t.Fatal("the append reported success while a batch never landed")
	}
	if !errors.Is(err, run.ErrPartlyDelivered) {
		t.Errorf("the append reported %v, want it marked as landed in part: a caller that retries the "+
			"whole call records the batches that did land a second time", err)
	}

	// What landed is there once, and nothing landed twice.
	stored, serr := backing.Events(ctx, r.ID)
	if serr != nil {
		t.Fatalf("Events() error = %v", serr)
	}
	seen := map[string]int{}
	for _, e := range stored {
		seen[e.Host]++
	}
	for host, n := range seen {
		if n != 1 {
			t.Errorf("host %s appears %d times, want at most once", host, n)
		}
	}
}
