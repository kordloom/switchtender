package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/event"
	"github.com/kordloom/switchtender/internal/run"
)

// TestEventExportStreamsEveryPage checks the NDJSON export returns every event of a run that holds
// more than one page, which is what proves it pages rather than loading the run whole.
//
// It previously read the entire event list into memory before writing a byte, so a run over a
// thousand hosts put its whole event stream in the server's memory for every concurrent download. A
// test that exported a handful of events could not tell the two apart; this one spans pages.
func TestEventExportStreamsEveryPage(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	// Evidence is written while a run is executing; the store fences appends to a terminal run.
	rn := &run.Run{ID: "run_export", Playbook: "site.yml", Status: run.StatusRunning}
	if err := store.Save(ctx, rn); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	// More than one page, so a single read cannot return them all.
	total := maxEventsPage + 25
	batch := make([]event.Event, 0, total)
	for i := 0; i < total; i++ {
		batch = append(batch, event.Event{
			Type: event.TypeRunnerOK, Host: fmt.Sprintf("host%04d", i), Task: "ping",
		})
	}
	if err := store.AppendEvents(ctx, rn.ID, batch); err != nil {
		t.Fatalf("AppendEvents() error = %v", err)
	}
	rn.Status = run.StatusSucceeded
	if err := store.Save(ctx, rn); err != nil {
		t.Fatalf("Save(succeeded) error = %v", err)
	}

	handler := New(store, &fakeSubmitter{}, zap.NewNop()).Handler()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/v1/runs/"+rn.ID+"/events?download=1", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	lines := strings.Count(strings.TrimSpace(rec.Body.String()), "\n") + 1
	if lines != total {
		t.Errorf("exported %d events, want all %d; the export stops at a page boundary", lines, total)
	}
	// The first and last host must both appear, so no page was skipped at either end.
	for _, host := range []string{"host0000", fmt.Sprintf("host%04d", total-1)} {
		if !strings.Contains(rec.Body.String(), host) {
			t.Errorf("export is missing %s", host)
		}
	}
}

// TestAStreamReconnectResumesTheLogFromItsCursor pins the reconnect path a secured install is
// forced onto.
//
// A stream ticket is single-use, so the browser's automatic retry, the one that would have sent
// Last-Event-ID, always gets 401. The page opens a brand-new stream instead, and a brand-new
// stream used to start the log at its current end: every line written during the outage was
// silently skipped, on the live view of exactly the runs somebody was watching closely enough to
// notice. The explicit logafter cursor is that reconnect's Last-Event-ID.
func TestAStreamReconnectResumesTheLogFromItsCursor(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	// The log is written while the run is live, because a terminal run's log is fenced against
	// late appends, then the run ends so the stream drains and closes.
	r := &run.Run{ID: "run_s", Status: run.StatusRunning, CreatedAt: time.Now()}
	if err := store.Save(ctx, r); err != nil {
		t.Fatalf("save run: %v", err)
	}
	// Ten bytes before the outage, ten after; the memory store's log cursor is the byte offset.
	// A reconnect resuming from offset ten sees the second ten; one that starts at the current
	// end sees nothing, which is the hole this cursor exists to close.
	if err := store.AppendLog(ctx, r.ID, []byte("before-gap")); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := store.AppendLog(ctx, r.ID, []byte("after-gap!")); err != nil {
		t.Fatalf("append: %v", err)
	}
	r.Status = run.StatusSucceeded
	if err := store.Save(ctx, r); err != nil {
		t.Fatalf("finish run: %v", err)
	}

	handler := New(store, &fakeSubmitter{}, zap.NewNop()).Handler()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/runs/run_s/stream?after=0&logafter=10", nil)
	handler.ServeHTTP(rec, req)

	body := rec.Body.String()
	if strings.Contains(body, "before-gap") {
		t.Errorf("the stream replayed bytes before the cursor:\n%s", body)
	}
	if !strings.Contains(body, "after-gap!") {
		t.Errorf("the stream skipped the bytes written after the cursor, which is the outage "+
			"gap this cursor exists to close:\n%s", body)
	}
}
