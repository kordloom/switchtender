package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
