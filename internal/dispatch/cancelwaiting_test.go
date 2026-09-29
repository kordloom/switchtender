package dispatch

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/run"
)

// TestCancelingAWaitingRunSettlesIt pins what a cancel owes a run no executor has claimed. The
// store cancels it in one statement, so no claim can slip in between, but a run canceled that way
// skipped everything else a run's end does: its outcome never reached the chain and no channel
// heard about it. A held run was announced as waiting for approval and then never announced again,
// so the approver it was sent to found it gone. Every other end of a run, a rejection included, is
// settled through the dispatcher, and this one is too.
func TestCancelingAWaitingRunSettlesIt(t *testing.T) {
	t.Parallel()

	tests := []struct {
		// Name says what state the run is in when the cancel arrives.
		Name string
		// Status is the run's status when the cancel arrives.
		Status run.Status
		// ClaimedBy is the executor holding the run, empty when none does.
		ClaimedBy string
		// WantCanceled reports whether this cancel settles the run.
		WantCanceled bool
	}{{ // Test 0: A held run, whose hold was announced, is announced again when it ends.
		Name: "held", Status: run.StatusPendingApproval, WantCanceled: true,
	}, { // Test 1: A queued run nobody has claimed yet.
		Name: "queued", Status: run.StatusPending, WantCanceled: true,
	}, { // Test 2: A claimed run is left to its executor's cooperative cancel.
		Name: "claimed", Status: run.StatusPending, ClaimedBy: "worker-a",
	}, { // Test 3: A running run is left to its executor too.
		Name: "running", Status: run.StatusRunning, ClaimedBy: "worker-a",
	}, { // Test 4: A finished run has nothing left to cancel.
		Name: "finished", Status: run.StatusSucceeded,
	}}

	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			hookURL, events := captureEvents(t)
			audits := audit.NewMemStore()
			store := run.NewMemStore()
			// The claim loop is closed, so the queued run is still waiting when the cancel arrives.
			d := New(store, okRunner(), nil, WithNoJanitor(), WithAudits(audits),
				WithWebhooks([]string{hookURL}), WithNotifyClient(http.DefaultClient),
				WithClaimGate(func() error { return errors.New("closed for this test") }))
			defer d.Close()
			if err := store.Save(ctx, &run.Run{
				ID: "run_wait", Tool: run.ToolBash, Command: "echo hi", Status: test.Status,
				ClaimedBy: test.ClaimedBy, CreatedAt: time.Now(),
			}); err != nil {
				t.Fatalf("Save() error = %v", err)
			}

			got, err := d.CancelWaiting(ctx, "run_wait")
			if err != nil {
				t.Fatalf("CancelWaiting() error = %v", err)
			}
			if got != test.WantCanceled {
				t.Fatalf("CancelWaiting() = %v, want %v", got, test.WantCanceled)
			}
			outcomes := outcomeEntries(t, audits, "run_wait")
			if !test.WantCanceled {
				noneArrive(t, events, "a run this cancel did not settle")
				if outcomes != 0 {
					t.Errorf("%d outcome entries for a run this cancel did not settle", outcomes)
				}
				return
			}

			stored, err := store.Get(ctx, "run_wait")
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			if stored.Status != run.StatusCanceled {
				t.Errorf("stored status = %q, want canceled", stored.Status)
			}
			e := nextEvent(t, events)
			if e.Event != "run.finished" || e.Run.ID != "run_wait" || e.Run.Status != run.StatusCanceled {
				t.Errorf("webhook event = %+v, want run.finished canceled for run_wait", e)
			}
			if outcomes != 1 {
				t.Errorf("%d outcome entries, want exactly one: a canceled run is a finished run "+
					"and the chain records how every run ended", outcomes)
			}
		})
	}
}

// outcomeEntries counts the outcome entries the chain holds for the run.
func outcomeEntries(t *testing.T, audits audit.Store, id string) int {
	t.Helper()
	chain, err := audits.Chain(context.Background())
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	n := 0
	for _, e := range chain {
		if strings.HasPrefix(e.Path, "/runs/"+id+"/outcome/") {
			n++
		}
	}
	return n
}
