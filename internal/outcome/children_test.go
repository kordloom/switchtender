package outcome_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/run"
)

// TestOutcomeRollsUpItsChildren covers what a coordinator's receipt used to say about the work it
// coordinated, which was nothing at all.
//
// A split or pipeline parent executes nothing itself, so its own log, hosts, and tasks are empty. Its
// children skip their own outcome commit precisely so it can be rolled up into the parent, and
// nothing rolled it up. A five-step pipeline against production therefore committed one record
// carrying the approved graph, a terminal status, and log_sha256 of the empty string: `verify`
// printed "what happened: succeeded (exit 0)" over an execution the chain held no evidence of, and a
// step that failed under continue-on-failure left no record anywhere.
func TestOutcomeRollsUpItsChildren(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()

	parent := &run.Run{
		ID: "run_parent", Kind: "pipeline", Playbook: "graph", Status: run.StatusSucceeded,
		CreatedAt: time.Now(),
	}
	if err := store.Save(ctx, parent); err != nil {
		t.Fatalf("Save parent: %v", err)
	}

	code0, code2 := 0, 2
	for _, c := range []struct {
		ID     string
		Name   string
		Index  int
		Status run.Status
		Exit   *int
		Log    string
	}{
		{"run_step_b", "smoke", 1, run.StatusFailed, &code2, "smoke output"},
		{"run_step_a", "build", 0, run.StatusSucceeded, &code0, "build output"},
	} {
		idx := c.Index
		// Saved running and finished afterward, the order a real step goes through: output is
		// captured while it runs, and a store may refuse an append to a run that has already ended.
		child := &run.Run{
			ID: c.ID, ParentID: &parent.ID, Kind: "step", StepName: c.Name, StepIndex: &idx,
			Playbook: "graph", Status: run.StatusRunning, CreatedAt: time.Now(),
		}
		if err := store.Save(ctx, child); err != nil {
			t.Fatalf("Save child %s: %v", c.ID, err)
		}
		if err := store.AppendLog(ctx, c.ID, []byte(c.Log)); err != nil {
			t.Fatalf("AppendLog %s: %v", c.ID, err)
		}
		child.Status, child.ExitCode = c.Status, c.Exit
		if err := store.Save(ctx, child); err != nil {
			t.Fatalf("Save child %s terminal: %v", c.ID, err)
		}
	}

	body, err := outcome.Body(ctx, store, parent)
	if err != nil {
		t.Fatalf("Body() error = %v", err)
	}
	var rec outcome.Record
	if err := json.Unmarshal(body, &rec); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}

	if len(rec.Children) != 2 {
		t.Fatalf("children = %d, want the two steps the pipeline ran: %s", len(rec.Children), body)
	}
	// Ordered by index, so the same two runs always digest to the same bytes.
	if rec.Children[0].Name != "build" || rec.Children[1].Name != "smoke" {
		t.Errorf("children out of order: %+v", rec.Children)
	}
	// The failing step is on the record, which is the case that used to vanish entirely.
	if rec.Children[1].Status != string(run.StatusFailed) {
		t.Errorf("the failed step reads %q, want failed", rec.Children[1].Status)
	}
	if rec.Children[1].ExitCode == nil || *rec.Children[1].ExitCode != 2 {
		t.Errorf("the failed step's exit code is %v, want 2", rec.Children[1].ExitCode)
	}
	// Each child's log is digested, so its stored output can be held against the receipt.
	for _, c := range rec.Children {
		if c.LogSHA256 == "" {
			t.Errorf("step %q carries no log digest, so its output proves nothing", c.Name)
		}
	}
	if rec.Children[0].LogSHA256 == rec.Children[1].LogSHA256 {
		t.Error("both steps digest to the same value, so the digests are not of their own output")
	}

	// An ordinary run has no children and gains no field.
	plain := &run.Run{ID: "run_plain", Playbook: "site.yml", Status: run.StatusSucceeded,
		CreatedAt: time.Now()}
	if err := store.Save(ctx, plain); err != nil {
		t.Fatalf("Save plain: %v", err)
	}
	pbody, err := outcome.Body(ctx, store, plain)
	if err != nil {
		t.Fatalf("Body(plain) error = %v", err)
	}
	var prec outcome.Record
	if err := json.Unmarshal(pbody, &prec); err != nil {
		t.Fatalf("Unmarshal(plain) error = %v", err)
	}
	if len(prec.Children) != 0 {
		t.Errorf("an ordinary run gained children: %+v", prec.Children)
	}
}
