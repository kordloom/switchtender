package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/event"
	"github.com/kordloom/switchtender/internal/run"
)

// errStoreGone is the mid-stream store failure the export tests inject.
var errStoreGone = errors.New("store gone")

// brokenPageStore serves one full page of a stream and fails on every read after it, which is what
// a store that dies part way through a download looks like to a handler whose status line has
// already gone out.
type brokenPageStore struct {
	run.Store
	// events is the one page the event export is served before the reads start failing.
	events []event.Event
	// chunks is the one page the log download is served before the reads start failing.
	chunks []run.LogChunk
	// eventCalls counts EventsAfter calls, so only the first is answered.
	eventCalls atomic.Int64
	// logCalls counts LogAfter calls, so only the first is answered.
	logCalls atomic.Int64
	// failFirst makes the very first read fail too, for the case where nothing is written yet.
	failFirst bool
}

// EventsAfter answers the first call with the canned page and fails every call after it.
func (b *brokenPageStore) EventsAfter(_ context.Context, _ string, _ int64, _ int) ([]event.Event, error) {
	if b.failFirst || b.eventCalls.Add(1) > 1 {
		return nil, errStoreGone
	}
	return b.events, nil
}

// LogAfter answers the first call with the canned chunks and fails every call after it.
func (b *brokenPageStore) LogAfter(_ context.Context, _ string, _ int64, _ int) ([]run.LogChunk, error) {
	if b.failFirst || b.logCalls.Add(1) > 1 {
		return nil, errStoreGone
	}
	return b.chunks, nil
}

// newBrokenExportServer stores one run in a memory store, wraps it in the broken page store, and
// returns a handler over the pair.
func newBrokenExportServer(t *testing.T, broken *brokenPageStore) http.Handler {
	t.Helper()
	mem := run.NewMemStore()
	err := mem.Save(context.Background(), &run.Run{
		ID: "run_1", Playbook: "site.yml", Status: run.StatusSucceeded, CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	broken.Store = mem
	return New(broken, &fakeSubmitter{}, zap.NewNop()).Handler()
}

// decodeSentinel decodes one NDJSON line as the incomplete marker.
func decodeSentinel(t *testing.T, line string) exportSentinel {
	t.Helper()
	var got exportSentinel
	if err := json.Unmarshal([]byte(line), &got); err != nil {
		t.Fatalf("last export line %q does not parse as JSON: %v", line, err)
	}
	return got
}

// TestEventsExportMarksItselfIncomplete pins that a run's NDJSON event export says so when the
// store fails part way through it.
//
// The export sends its status line with the first event, so a store failure after that cannot
// answer 500 any more. It returned silently instead, leaving a file that parses cleanly, ends on a
// whole event, and is missing everything after the failure. The log download at least has
// LogSHA256 recorded in the outcome to check a copy against. The event export has no digest behind
// it at all, so the trailing sentinel is the only thing that distinguishes a short file from a
// whole one.
func TestEventsExportMarksItselfIncomplete(t *testing.T) {
	t.Parallel()
	// A page has to be exactly full for the export to come back for another one, which is where
	// the failure lands.
	page := make([]event.Event, maxEventsPage)
	for i := range page {
		page[i] = event.Event{Seq: int64(i + 1), Type: event.TypeTaskStart, Task: fmt.Sprintf("task %d", i)}
	}
	handler := newBrokenExportServer(t, &brokenPageStore{events: page})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/runs/run_1/events?download=1", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("events export = %d, want 200: the status line is already spent", rec.Code)
	}
	lines := strings.Split(strings.TrimSuffix(rec.Body.String(), "\n"), "\n")
	if len(lines) != maxEventsPage+1 {
		t.Fatalf("export has %d lines, want %d events plus the sentinel", len(lines), maxEventsPage+1)
	}
	// Every event that was read has to be in the file. A sentinel that replaced data would be a
	// different bug wearing the fix's clothes.
	var first event.Event
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("first export line does not parse as an event: %v", err)
	}
	if first.Task != "task 0" {
		t.Errorf("first exported event task = %q, want %q", first.Task, "task 0")
	}
	got := decodeSentinel(t, lines[len(lines)-1])
	want := exportSentinel{
		Incomplete: true, Reason: "the event store failed part way through this export",
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("sentinel mismatch (-want +got):\n%s", diff)
	}
}

// TestEventsExportStillFailsLoudlyBeforeAnyBytes pins that a store failure on the first read is
// still a 500 with no attachment, since nothing has been committed to the wire yet. The sentinel
// is for the case where a clean error is no longer available, not a replacement for one.
func TestEventsExportStillFailsLoudlyBeforeAnyBytes(t *testing.T) {
	t.Parallel()
	handler := newBrokenExportServer(t, &brokenPageStore{failFirst: true})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/runs/run_1/events?download=1", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("events export with a store down from the start = %d, want 500", rec.Code)
	}
	if cd := rec.Header().Get("Content-Disposition"); cd != "" {
		t.Errorf("Content-Disposition = %q on a failed export, want none", cd)
	}
	if strings.Contains(rec.Body.String(), "export_incomplete") {
		t.Errorf("body carries the sentinel on a clean 500: %q", rec.Body.String())
	}
}

// TestEventsExportWholeRunHasNoSentinel pins the ordinary export: every event, one per line, no
// marker, so the sentinel means something when it does appear.
func TestEventsExportWholeRunHasNoSentinel(t *testing.T) {
	t.Parallel()
	store := run.NewMemStore()
	ctx := context.Background()
	err := store.Save(ctx, &run.Run{
		ID: "run_1", Playbook: "site.yml", Status: run.StatusRunning, CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	err = store.AppendEvents(ctx, "run_1", []event.Event{
		{Type: event.TypeTaskStart, Task: "one"}, {Type: event.TypeTaskStart, Task: "two"},
	})
	if err != nil {
		t.Fatalf("AppendEvents() error = %v", err)
	}
	handler := New(store, &fakeSubmitter{}, zap.NewNop()).Handler()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/runs/run_1/events?download=1", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("events export = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/x-ndjson") {
		t.Errorf("Content-Type = %q, want NDJSON", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "run_1-events.ndjson") {
		t.Errorf("Content-Disposition = %q, want the named attachment", cd)
	}
	var tasks []string
	for _, line := range strings.Split(strings.TrimSuffix(rec.Body.String(), "\n"), "\n") {
		var e event.Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("export line %q does not parse as an event: %v", line, err)
		}
		tasks = append(tasks, e.Task)
	}
	if diff := cmp.Diff([]string{"one", "two"}, tasks, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("exported tasks mismatch (-want +got):\n%s", diff)
	}
	if strings.Contains(rec.Body.String(), "export_incomplete") {
		t.Errorf("a complete export carries the sentinel: %q", rec.Body.String())
	}
}

// TestLogDownloadMarksItselfIncomplete pins the same sentinel on the plain text log download,
// including the newline that keeps the marker off the end of a partial log line.
func TestLogDownloadMarksItselfIncomplete(t *testing.T) {
	t.Parallel()
	chunks := make([]run.LogChunk, streamBatch)
	for i := range chunks {
		chunks[i] = run.LogChunk{Seq: int64(i + 1), Data: []byte(fmt.Sprintf("line %d\n", i))}
	}
	// The last chunk ends mid line, which is how a real log page boundary usually falls.
	chunks[len(chunks)-1] = run.LogChunk{Seq: int64(streamBatch), Data: []byte("PLAY RECAP")}
	handler := newBrokenExportServer(t, &brokenPageStore{chunks: chunks})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/runs/run_1/logs", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("log download = %d, want 200: the status line is already spent", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "line 0\n") || !strings.Contains(body, "PLAY RECAP") {
		t.Errorf("log body lost what was read before the failure: %q", body[:min(len(body), 200)])
	}
	lines := strings.Split(strings.TrimSuffix(body, "\n"), "\n")
	got := decodeSentinel(t, lines[len(lines)-1])
	// The marker takes a line of its own even though the last page ended mid line, so it is not
	// read as part of whatever the playbook was printing when the store went away.
	if prev := lines[len(lines)-2]; prev != "PLAY RECAP" {
		t.Errorf("line before the sentinel = %q, want the partial log tail on its own line", prev)
	}
	want := exportSentinel{
		Incomplete: true, Reason: "the log store failed part way through this download",
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("sentinel mismatch (-want +got):\n%s", diff)
	}
}
