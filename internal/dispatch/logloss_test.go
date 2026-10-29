package dispatch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/dispatchtest"
	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/plantest"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// errDiskFull stands in for the error a store returns once its disk is full.
var errDiskFull = errors.New("database or disk is full (13)")

// lossyLogStore is a run store whose log appends fail for the chunks refuse picks, standing in for a
// database whose disk filled while a run was writing.
type lossyLogStore struct {
	run.Store
	// refuse reports whether an append of p fails. Nil refuses every append.
	refuse func(p []byte) bool
}

// AppendLog fails the appends refuse picks and stores the rest.
func (s *lossyLogStore) AppendLog(ctx context.Context, id string, p []byte) error {
	if s.refuse == nil || s.refuse(p) {
		return errDiskFull
	}
	return s.Store.AppendLog(ctx, id, p)
}

// TestARunWhoseOutputWasNotStoredIsNotASuccess pins that a run cannot finish succeeded over a log the
// store did not keep. At disk full every append failed, the sink logged each one and carried on, and
// the run was recorded succeeded with an empty warning and a signed outcome over the truncated log.
// A run that ended for its own reason keeps that status and says its log is incomplete.
func TestARunWhoseOutputWasNotStoredIsNotASuccess(t *testing.T) {
	t.Parallel()
	const output = "runner output\n"
	tests := []struct {
		Events      string
		WantStatus  run.Status
		WantError   string
		WantWarning string
		Refuse      bool
		ExitCode    int
	}{{ // Test 0: A clean run whose output was lost fails, keeping the tool's clean exit.
		Events: changedOneHostEvents, Refuse: true, ExitCode: 0,
		WantStatus: run.StatusFailed, WantError: logIncomplete(int64(len(output))),
	}, { // Test 1: A run that failed on its own keeps its failure and warns about the log.
		Events: failedOneHostEvents, Refuse: true, ExitCode: 2, WantStatus: run.StatusFailed,
		WantWarning: "14 bytes of output could not be stored, so this run's log is incomplete",
	}, { // Test 2: A run whose output was stored is untouched.
		Events: changedOneHostEvents, ExitCode: 0, WantStatus: run.StatusSucceeded,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			store := &lossyLogStore{Store: run.NewMemStore(), refuse: func([]byte) bool { return test.Refuse }}
			d := New(store, &recapRunner{events: test.Events, exitCode: test.ExitCode}, nil,
				WithNoJanitor(), WithClaimInterval(time.Millisecond))
			defer d.Close()

			created, err := d.Submit(ctx, "site.yml", "inv")
			if err != nil {
				t.Fatalf("Submit() error = %v", err)
			}
			got := dispatchtest.WaitTerminal(t, store, created.ID)
			if got.Status != test.WantStatus {
				t.Errorf("run status = %q, want %q", got.Status, test.WantStatus)
			}
			if got.ExitCode == nil || *got.ExitCode != test.ExitCode {
				t.Errorf("run exit code = %v, want the tool's %d kept beside the outcome",
					got.ExitCode, test.ExitCode)
			}
			if diff := cmp.Diff(test.WantError, got.Error, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("run error mismatch (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(test.WantWarning, got.Warning, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("run warning mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestLostOutputReachesTheSignedOutcome pins that the reason a run failed over its lost output is in
// the outcome the chain commits, not only on the run row, so a verifier reading the record learns the
// log it digests is incomplete.
func TestLostOutputReachesTheSignedOutcome(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := &lossyLogStore{Store: run.NewMemStore()}
	audits := audit.NewMemStore()
	d := New(store, &recapRunner{events: changedOneHostEvents}, nil, WithAudits(audits))
	defer d.Close()

	created, err := d.Submit(ctx, "site.yml", "inv", run.WithActor("alice"))
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	got := dispatchtest.WaitTerminal(t, store, created.ID)
	entry := waitOutcomeEntry(t, audits, created.ID)
	body, err := outcome.Body(ctx, store, got)
	if err != nil {
		t.Fatalf("outcome.Body() error = %v", err)
	}
	if !outcome.VerifyBody(entry.ContentDigest, entry.Nonce, body) {
		t.Fatal("the committed digest does not verify against the run's rebuilt outcome")
	}
	rec, err := outcome.Parse(body)
	if err != nil {
		t.Fatalf("outcome.Parse() error = %v", err)
	}
	if rec.Status != string(run.StatusFailed) {
		t.Errorf("committed status = %q, want failed", rec.Status)
	}
	if diff := cmp.Diff(logIncomplete(int64(len("runner output\n"))), rec.Error); diff != "" {
		t.Errorf("committed failure reason mismatch (-want +got):\n%s", diff)
	}
}

// TestAPlanWhoseOutputWasNotStoredProposesNoApply pins the plan gate's half. An approver weighs the
// plan's output, so a plan whose output the store did not keep fails and proposes nothing, rather
// than queueing an apply beside a log that cannot show what the plan would do.
func TestAPlanWhoseOutputWasNotStoredProposesNoApply(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const output = "Plan: 0 to add, 0 to change, 0 to destroy.\n"
	store := &lossyLogStore{Store: run.NewMemStore()}
	runner := &brokenPlanRunner{
		output: output,
		plan:   &roundhouse.Result{ExitCode: 0, Drift: true, PlanFile: []byte(plantest.File)},
	}
	d := New(store, runner, nil, WithNoJanitor(), WithPolicies(destroyGuardStore(t, 3)),
		WithClaimInterval(time.Millisecond))
	defer d.Close()

	created, err := d.Submit(ctx, "", "",
		run.WithTool(run.ToolTerraform), run.WithCommand("infra/prod"))
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	got := dispatchtest.WaitTerminal(t, store, created.ID)
	if got.Status != run.StatusFailed {
		t.Errorf("plan run status = %q, want failed: its output was not stored", got.Status)
	}
	if diff := cmp.Diff(logIncomplete(int64(len(output))), got.Error); diff != "" {
		t.Errorf("plan run error mismatch (-want +got):\n%s", diff)
	}
	// Give any proposal that was going to be made time to appear.
	time.Sleep(200 * time.Millisecond)
	if n := countProposals(t, store); n != 0 {
		t.Errorf("%d applies were proposed from a plan whose output nobody can read whole", n)
	}
}

// TestALostPlanGateNoteIsNotSilent pins that the note naming the proposed apply, which the gate
// writes beneath the plan's output, says so on the run when it could not be stored. The plan itself
// was kept, so the run still succeeds and its apply is still proposed.
func TestALostPlanGateNoteIsNotSilent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := &lossyLogStore{Store: run.NewMemStore(), refuse: func(p []byte) bool {
		return bytes.HasPrefix(p, []byte("switchtender: "))
	}}
	runner := &brokenPlanRunner{
		output: "Plan: 0 to add, 0 to change, 0 to destroy.\n",
		plan:   &roundhouse.Result{ExitCode: 0, Drift: true, PlanFile: []byte(plantest.File)},
	}
	d := New(store, runner, nil, WithNoJanitor(), WithPolicies(destroyGuardStore(t, 3)),
		WithClaimInterval(time.Millisecond))
	defer d.Close()

	created, err := d.Submit(ctx, "", "",
		run.WithTool(run.ToolTerraform), run.WithCommand("infra/prod"))
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	got := dispatchtest.WaitTerminal(t, store, created.ID)
	if got.Status != run.StatusSucceeded {
		t.Fatalf("plan run status = %q, want succeeded: the plan's own output was stored", got.Status)
	}
	proposal := dispatchtest.WaitProposal(t, store, created.ID)
	want := "the note naming proposed apply " + proposal.ID + " could not be stored in this log"
	if !strings.Contains(got.Warning, want) {
		t.Errorf("plan run warning = %q, want it to contain %q", got.Warning, want)
	}
}
