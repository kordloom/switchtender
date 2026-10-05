package storetest

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/event"
	"github.com/kordloom/switchtender/internal/run"
)

// testRetentionKeepsOwedOutcomes verifies retention keeps everything an outcome still owed to the
// chain is built from, the owing run's record, events, log, and summaries and each child's record
// and log, through every purge, and lets all of it go once the outcome is settled, while a run
// that owes nothing goes as before. Without the hold, a chain that refused appends for longer than
// the retention window left the janitor to build the outcome after the purge, a record of an empty
// log and no hosts presented as what the run did.
func testRetentionKeepsOwedOutcomes(t *testing.T, store run.Store) {
	t.Helper()
	ctx := context.Background()
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	cutoff := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	owed, settled := "keep_owed", "keep_settled"
	runs := []*run.Run{
		{ID: owed, Kind: run.KindPipeline},
		{ID: "keep_owed_step", ParentID: &owed},
		{ID: settled, Kind: run.KindPipeline},
		{ID: "keep_settled_step", ParentID: &settled},
	}
	// Each run records its output while it runs, as a real run does, and the newest summaries are
	// the settled step's, so a trim to one per host and task reaches every other run's.
	for i, r := range runs {
		r.Playbook, r.Status, r.ClaimedBy, r.CreatedAt = "p.yml", run.StatusRunning, "w", old
		if err := store.Save(ctx, r); err != nil {
			t.Fatalf("Save(%s) error = %v", r.ID, err)
		}
		ranAt := old.Add(time.Duration(i) * time.Minute)
		if err := store.AppendEvents(ctx, r.ID,
			[]event.Event{{Type: event.TypePlayStart, Time: ranAt, Play: "p"}}); err != nil {
			t.Fatalf("AppendEvents(%s) error = %v", r.ID, err)
		}
		if err := store.AppendLog(ctx, r.ID, []byte("output of "+r.ID)); err != nil {
			t.Fatalf("AppendLog(%s) error = %v", r.ID, err)
		}
		if err := store.SaveHostSummary(ctx, r.ID,
			[]run.HostSummary{{Host: "h1", Worst: "ok", OK: 1, RanAt: ranAt}}); err != nil {
			t.Fatalf("SaveHostSummary(%s) error = %v", r.ID, err)
		}
		if err := store.SaveTaskSummary(ctx, r.ID,
			[]run.TaskSummary{{Task: "t1", Seconds: 1, RanAt: ranAt}}); err != nil {
			t.Fatalf("SaveTaskSummary(%s) error = %v", r.ID, err)
		}
	}
	// The steps end before their pipelines do. Both pipelines come to owe their outcome, and the
	// settled one's is committed, as the process that finished it would.
	fin := run.Finalization{Status: run.StatusSucceeded, EndedAt: old.Add(time.Hour)}
	for _, id := range []string{"keep_owed_step", "keep_settled_step", owed, settled} {
		if moved, err := store.FinalizeRunning(ctx, id, fin); err != nil || !moved {
			t.Fatalf("FinalizeRunning(%s) = %t, %v", id, moved, err)
		}
	}
	if err := store.SettleOutcome(ctx, settled); err != nil {
		t.Fatalf("SettleOutcome(%s) error = %v", settled, err)
	}

	tests := []struct {
		Settle           string
		WantKept         []string
		WantGone         []string
		WantTrimmed      int
		WantSummaryTrims int
		WantDeleted      int
	}{{ // Test 0: The owing pipeline and its step keep everything while the settled pair goes.
		WantKept: []string{owed, "keep_owed_step"}, WantGone: []string{settled, "keep_settled_step"},
		WantTrimmed: 2, WantSummaryTrims: 2, WantDeleted: 2,
	}, { // Test 1: The same pass again removes nothing more while the outcome is owed.
		WantKept: []string{owed, "keep_owed_step"}, WantGone: []string{settled, "keep_settled_step"},
	}, { // Test 2: Once the outcome is settled, retention takes the pipeline and its step.
		Settle:      owed,
		WantGone:    []string{owed, "keep_owed_step", settled, "keep_settled_step"},
		WantTrimmed: 2, WantSummaryTrims: 4, WantDeleted: 2,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			if test.Settle != "" {
				if err := store.SettleOutcome(ctx, test.Settle); err != nil {
					t.Fatalf("SettleOutcome(%s) error = %v", test.Settle, err)
				}
			}
			// Kept runs are read between the passes too, so a pass that removed what a later
			// pass removes again is still caught at the step that did it.
			kept := map[string]keptRun{}
			for _, id := range test.WantKept {
				kept[id] = readKeptRun(ctx, t, store, id)
			}
			trimmed, err := store.PurgeEventsBefore(ctx, cutoff)
			if err != nil || trimmed != test.WantTrimmed {
				t.Errorf("PurgeEventsBefore() = %d, %v, want %d", trimmed, err, test.WantTrimmed)
			}
			trims, err := store.TrimSummaries(ctx, 1)
			if err != nil || trims != test.WantSummaryTrims {
				t.Errorf("TrimSummaries(1) = %d, %v, want %d", trims, err, test.WantSummaryTrims)
			}
			deleted, err := store.PurgeRunsBefore(ctx, cutoff)
			if err != nil || deleted != test.WantDeleted {
				t.Errorf("PurgeRunsBefore() = %d, %v, want %d", deleted, err, test.WantDeleted)
			}
			for _, id := range test.WantKept {
				if diff := cmp.Diff(kept[id], readKeptRun(ctx, t, store, id)); diff != "" {
					t.Errorf("run %s, whose outcome is owed, changed under retention "+
						"(-before +after):\n%s", id, diff)
				}
			}
			for _, id := range test.WantGone {
				if _, err := store.Get(ctx, id); !errors.Is(err, run.ErrNotFound) {
					t.Errorf("Get(%s) after retention error = %v, want run.ErrNotFound", id, err)
				}
			}
		})
	}
}

// keptRun is what an outcome is built from for one run, read back from the store.
type keptRun struct {
	// Status is the run's stored status.
	Status run.Status
	// Events is how many events the run holds.
	Events int
	// Log is the run's whole log.
	Log string
	// Hosts is how many host summaries the run holds.
	Hosts int
	// Tasks is how many task summaries the run holds.
	Tasks int
}

// readKeptRun reads what an outcome is built from for the run id, failing the test when the run is
// gone.
func readKeptRun(ctx context.Context, t *testing.T, store run.Store, id string) keptRun {
	t.Helper()
	r, err := store.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get(%s) error = %v, want the run kept while its outcome is owed", id, err)
	}
	events, err := store.Events(ctx, id)
	if err != nil {
		t.Fatalf("Events(%s) error = %v", id, err)
	}
	chunks, err := store.LogAfter(ctx, id, 0, 100)
	if err != nil {
		t.Fatalf("LogAfter(%s) error = %v", id, err)
	}
	var log string
	for _, c := range chunks {
		log += string(c.Data)
	}
	hosts, err := store.RunHostSummaries(ctx, id)
	if err != nil {
		t.Fatalf("RunHostSummaries(%s) error = %v", id, err)
	}
	tasks, err := store.RunTaskSummaries(ctx, id)
	if err != nil {
		t.Fatalf("RunTaskSummaries(%s) error = %v", id, err)
	}
	return keptRun{Status: r.Status, Events: len(events), Log: log, Hosts: len(hosts),
		Tasks: len(tasks)}
}
