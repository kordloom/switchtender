package server

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/event"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/sqlitestore"
)

// TestExplainEventsSpansBusyInstall proves explainEvents returns a run's own earlier events, its
// failures among them, even when far more than the window's worth of other runs' events were recorded
// between them. Event sequences are global (run_events.seq autoincrements across every run), so the
// previous last-minus-window arithmetic skipped a run's early failures on a busy install, the same
// class as the dossier cursor bug. A memory store hides this because it numbers events per run.
func TestExplainEventsSpansBusyInstall(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, err := sqlitestore.Open(filepath.Join(t.TempDir(), "switchtender.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()
	store := db.Runs()

	now := time.Now()
	for _, id := range []string{"run_target", "run_decoy"} {
		if err := store.Save(ctx, &run.Run{ID: id, Status: run.StatusRunning, CreatedAt: now,
			Playbook: "site.yml", Inventory: "inv"}); err != nil {
			t.Fatalf("save %s: %v", id, err)
		}
	}

	// The target's failing event is recorded first, at a low global sequence.
	if err := store.AppendEvents(ctx, "run_target", []event.Event{
		{Type: event.TypeRunnerFailed, Host: "web1", Play: "deploy", Task: "apply", Time: now},
	}); err != nil {
		t.Fatalf("append target fail: %v", err)
	}
	// More than a window's worth of another run's events push the global sequence far ahead.
	decoy := make([]event.Event, 600)
	for i := range decoy {
		decoy[i] = event.Event{Type: event.TypeRunnerOK, Host: "d", Time: now}
	}
	if err := store.AppendEvents(ctx, "run_decoy", decoy); err != nil {
		t.Fatalf("append decoy: %v", err)
	}
	// The target's last event lands at a high global sequence, a long way past its first.
	if err := store.AppendEvents(ctx, "run_target", []event.Event{
		{Type: event.TypeRunnerOK, Host: "web1", Play: "deploy", Task: "verify", Time: now},
	}); err != nil {
		t.Fatalf("append target ok: %v", err)
	}

	got := explainEvents(ctx, store, "run_target")
	sawFail := false
	for _, e := range got {
		if e.Type == event.TypeRunnerFailed {
			sawFail = true
		}
	}
	if !sawFail {
		t.Errorf("explainEvents dropped the run's failing event; got %d events, want the early "+
			"failure included despite the busy global sequence", len(got))
	}
	if len(got) != 2 {
		t.Errorf("explainEvents returned %d events, want the target's 2 own events", len(got))
	}
}
