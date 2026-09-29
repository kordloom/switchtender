package relay

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/event"
	"github.com/kordloom/switchtender/internal/run"
)

// countingRelay records how many items each call carried and refuses a call over the control node's
// element cap exactly as the real relay server does.
type countingRelay struct {
	// batches holds the item count of each accepted call, in order.
	batches []int
	// refused counts the calls the cap rejected.
	refused int
}

// handler serves the report endpoints a worker posts to, applying the same cap the relay server does.
func (c *countingRelay) handler() http.Handler {
	mux := http.NewServeMux()
	report := func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var items []json.RawMessage
		if err := json.Unmarshal(body, &items); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if len(items) > maxRelayElements {
			c.refused++
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			return
		}
		c.batches = append(c.batches, len(items))
		w.WriteHeader(http.StatusNoContent)
	}
	for _, path := range []string{"/events", "/host-summary", "/host-facts", "/task-summary"} {
		mux.HandleFunc("/relay/v1/runs/run_1"+path, report)
	}
	return mux
}

// TestWorkerReportsInBatchesTheControlNodeAccepts covers a run whose evidence was thrown away for
// being large. The control node caps one relay call at maxRelayElements items, for good reason: the
// cap is what stops a worker forcing an unbounded decode. The worker posted whatever it had in one
// call. A playbook over a few hundred hosts produces more results than the cap in one report, so the
// call came back 413 and the events, the per-host outcomes, or the per-task timings for that run were
// simply gone: no log line an operator would find, and a dossier that shows a run with no hosts.
func TestWorkerReportsInBatchesTheControlNodeAccepts(t *testing.T) {
	t.Parallel()
	rec := &countingRelay{}
	srv := httptest.NewServer(rec.handler())
	t.Cleanup(srv.Close)

	tr := NewHTTPTransport(srv.URL, "ymt_worker", nil)

	// A 250-host playbook running 30 tasks reports 7500 results, which is over the cap.
	events := make([]event.Event, 7500)
	for i := range events {
		events[i] = event.Event{Type: "runner_ok", Host: "web-1", Task: "install", Time: time.Now()}
	}
	if err := tr.AppendEvents(context.Background(), "run_1", events); err != nil {
		t.Fatalf("AppendEvents of a large run = %v, want it accepted in batches", err)
	}
	if rec.refused != 0 {
		t.Errorf("%d calls were refused for being too large, so that evidence never landed", rec.refused)
	}
	var total int
	for _, n := range rec.batches {
		if n > maxRelayElements {
			t.Errorf("a batch carried %d items, over the cap of %d", n, maxRelayElements)
		}
		total += n
	}
	if total != len(events) {
		t.Errorf("the control node received %d of %d events, so evidence was lost", total, len(events))
	}

	// The same for the other three report kinds, each of which scales with host or task count.
	rec.batches, rec.refused = nil, 0
	summaries := make([]run.HostSummary, 6000)
	for i := range summaries {
		summaries[i] = run.HostSummary{RunID: "run_1", Host: "web-1", OK: 1}
	}
	if err := tr.SaveHostSummary(context.Background(), "run_1", summaries); err != nil {
		t.Fatalf("SaveHostSummary = %v, want it accepted in batches", err)
	}
	if rec.refused != 0 {
		t.Errorf("%d host-summary calls were refused, so the run's per-host outcome was lost", rec.refused)
	}

	rec.batches, rec.refused = nil, 0
	facts := make([]run.HostFacts, 6000)
	for i := range facts {
		facts[i] = run.HostFacts{RunID: "run_1", Host: "web-1"}
	}
	if err := tr.SaveHostFacts(context.Background(), "run_1", facts); err != nil {
		t.Fatalf("SaveHostFacts = %v, want it accepted in batches", err)
	}
	if rec.refused != 0 {
		t.Errorf("%d host-facts calls were refused, so the fleet inventory was lost", rec.refused)
	}

	rec.batches, rec.refused = nil, 0
	tasks := make([]run.TaskSummary, 6000)
	for i := range tasks {
		tasks[i] = run.TaskSummary{RunID: "run_1", Task: "install"}
	}
	if err := tr.SaveTaskSummary(context.Background(), "run_1", tasks); err != nil {
		t.Fatalf("SaveTaskSummary = %v, want it accepted in batches", err)
	}
	if rec.refused != 0 {
		t.Errorf("%d task-summary calls were refused, so the timings were lost", rec.refused)
	}

	// A small run still goes in one call, so batching costs nothing in the ordinary case.
	rec.batches, rec.refused = nil, 0
	if err := tr.AppendEvents(context.Background(), "run_1", events[:12]); err != nil {
		t.Fatalf("AppendEvents of a small run = %v", err)
	}
	if len(rec.batches) != 1 || rec.batches[0] != 12 {
		t.Errorf("a 12-event run was posted as %v, want one call of 12", rec.batches)
	}

	// Nothing to report is no call at all.
	rec.batches = nil
	if err := tr.AppendEvents(context.Background(), "run_1", nil); err != nil {
		t.Fatalf("AppendEvents of nothing = %v", err)
	}
	if len(rec.batches) != 0 {
		t.Errorf("an empty report made %d calls", len(rec.batches))
	}
}
