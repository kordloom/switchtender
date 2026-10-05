package dispatch

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/decision"
	"github.com/kordloom/switchtender/internal/run"
)

// audPauseKey marks the context of the one call audPausedRuns holds, so a test can pause a single
// decision between its read of the run and everything it does next.
type audPauseKey struct{}

// audPausedRuns is a run store that answers Get from the store it wraps and then, for a context
// carrying audPauseKey, waits for the test before returning. It stands in for a replica whose
// decision read the run as held and then stalled, on a garbage collection pause, a slow database,
// or a suspended process, while another replica decided and the run executed.
type audPausedRuns struct {
	run.Store
	// read is closed once the paused call has read the run.
	read chan struct{}
	// release is closed by the test to let the paused call return.
	release chan struct{}
	// once makes only the first marked call pause.
	once sync.Once
}

// Get reads the run and, for the marked call, waits for release before returning what it read.
func (s *audPausedRuns) Get(ctx context.Context, id string) (*run.Run, error) {
	r, err := s.Store.Get(ctx, id)
	if ctx.Value(audPauseKey{}) != nil {
		s.once.Do(func() {
			close(s.read)
			<-s.release
		})
	}
	return r, err
}

// audClock is a settable clock both replicas read, so the times the chain records are chosen by
// the test rather than by scheduling.
type audClock struct {
	// mu guards at.
	mu sync.Mutex
	// at is the time now returns.
	at time.Time
}

// now returns the clock's current time.
func (c *audClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

// set moves the clock to at.
func (c *audClock) set(at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = at
}

// audDecisionPaths returns the paths of the DECISION entries the chain holds for run id, in chain
// order.
func audDecisionPaths(t *testing.T, audits audit.Store, id string) []string {
	t.Helper()
	chain, err := audits.Chain(context.Background())
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	var out []string
	for _, e := range chain {
		if e.Method == audit.MethodDecision && strings.Contains(e.Path, "/runs/"+id+"/") {
			out = append(out, e.Path)
		}
	}
	return out
}

// audWaitOutcome waits until the chain holds the outcome entry of run id.
func audWaitOutcome(t *testing.T, audits audit.Store, id string) {
	t.Helper()
	deadline := time.Now().Add(waitBudget)
	for time.Now().Before(deadline) {
		chain, err := audits.Chain(context.Background())
		if err != nil {
			t.Fatalf("Chain() error = %v", err)
		}
		for _, e := range chain {
			if e.Method == audit.MethodRun && strings.HasPrefix(e.Path, "/runs/"+id+"/outcome/") {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("the chain never recorded the outcome of %s", id)
}

// audBundleReport builds, signs, and verifies a bundle over the whole chain the way an exported
// bundle is checked offline, and returns the verifier's report.
func audBundleReport(t *testing.T, audits audit.Store) *audit.BundleReport {
	t.Helper()
	chain, err := audits.Chain(context.Background())
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	id, err := audit.LoadIdentity(t.TempDir())
	if err != nil {
		t.Fatalf("LoadIdentity() error = %v", err)
	}
	doc, err := audit.BuildBundle(chain, id, "v-test", time.Now())
	if err != nil {
		t.Fatalf("BuildBundle() error = %v", err)
	}
	signed, err := audit.SignBundleDoc(doc, id.Private())
	if err != nil {
		t.Fatalf("SignBundleDoc() error = %v", err)
	}
	rep, err := audit.VerifyBundle(signed, id.KeyID())
	if err != nil {
		t.Fatalf("VerifyBundle() error = %v", err)
	}
	return rep
}

// TestChainHoldsNoDecisionThatLostTheRaceToAFinishedRun races two replicas deciding one held run.
//
// Replica B reads the run while it is still held, passes the "only while awaiting a decision"
// precondition, and stalls. Replica A approves, the run executes and succeeds, and its outcome is
// committed. Replica B then resumes a minute later: it appends its decision to the chain, loses the
// compare-and-set, and answers 409. The precondition that TestARefusedDecisionLeavesNoChainEntry
// pins for the sequential case is a read taken before the chain entry, not a guard on it, so across
// two replicas the refused decision is still written down.
//
// For a late approval the chain then holds an approval recorded after the outcome of the run it
// released, which the bundle verifier reads as the gate being bypassed: the honest install's
// whole-fleet bundle fails with ApprovalPrecedesRun false. For a late rejection the permanent
// record says an approver rejected a run that ran and succeeded. Both are the defects the
// precondition in approveRun and rejectRun was written to close, reopened by a second replica.
func TestChainHoldsNoDecisionThatLostTheRaceToAFinishedRun(t *testing.T) {
	// Not parallel: the bundle check isolates its signing identity through the environment.
	t.Setenv("SWITCHTENDER_AUDIT_KEY", "")
	base := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	tests := []struct {
		// Name says which late decision races the approval.
		Name string
		// Late is the decision replica B makes after reading the run as held.
		Late func(ctx context.Context, d *Dispatcher, id string) error
		// CheckBundle asks the bundle verifier for its verdict as well, for a late approval.
		CheckBundle bool
	}{{ // Test 0: Two approvers on two replicas, one a moment behind the other.
		Name: "late approve",
		Late: func(ctx context.Context, d *Dispatcher, id string) error {
			_, err := d.Approve(ctx, id, decider("approver-b", "session"))
			return err
		},
		CheckBundle: true,
	}, { // Test 1: A reject that read the run as held while another replica approved it.
		Name: "late reject",
		Late: func(ctx context.Context, d *Dispatcher, id string) error {
			_, err := d.Reject(ctx, id, "changed my mind", decider("approver-b", "session"))
			return err
		},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			ctx := context.Background()
			clock := &audClock{at: base}
			runs := run.NewMemStore()
			audits := audit.NewMemStore()
			decisions := decision.NewMemStore()
			paused := &audPausedRuns{Store: runs, read: make(chan struct{}),
				release: make(chan struct{})}

			replicaA := New(runs, okRunner(), nil, WithAudits(audits), WithDecisions(decisions),
				WithClock(clock.now), WithNoJanitor(), WithOwner("replica-a"))
			defer replicaA.Close()
			replicaB := New(paused, okRunner(), nil, WithAudits(audits), WithDecisions(decisions),
				WithClock(clock.now), WithNoJanitor(), WithOwner("replica-b"))
			defer replicaB.Close()

			id := fmt.Sprintf("run_race_%d", testNum)
			if err := runs.Save(ctx, &run.Run{
				ID: id, Playbook: "site.yml", Inventory: "prod",
				Status: run.StatusPendingApproval, CreatedAt: base, Actor: "operator",
			}); err != nil {
				t.Fatalf("Save() error = %v", err)
			}

			lateErr := make(chan error, 1)
			go func() {
				lateErr <- test.Late(context.WithValue(ctx, audPauseKey{}, true), replicaB, id)
			}()
			select {
			case <-paused.read:
			case <-time.After(waitBudget):
				t.Fatal("replica B never read the run")
			}

			if _, err := replicaA.Approve(ctx, id, decider("approver-a", "session")); err != nil {
				t.Fatalf("replica A Approve() error = %v", err)
			}
			if done := waitTerminal(t, runs, id); done.Status != run.StatusSucceeded {
				t.Fatalf("the approved run ended %s, want succeeded", done.Status)
			}
			audWaitOutcome(t, audits, id)
			before := audDecisionPaths(t, audits, id)

			// Replica B resumes a minute later and finishes the decision it started.
			clock.set(base.Add(time.Minute))
			close(paused.release)
			var err error
			select {
			case err = <-lateErr:
			case <-time.After(waitBudget):
				t.Fatal("replica B's decision never returned")
			}
			if !errors.Is(err, ErrNotPendingApproval) {
				t.Errorf("the late decision returned %v, want ErrNotPendingApproval", err)
			}

			after := audDecisionPaths(t, audits, id)
			if diff := cmp.Diff(before, after); diff != "" {
				t.Errorf("a decision refused with 409 was still written to the chain after the "+
					"run it named had executed (-before +after):\n%s", diff)
			}
			if test.CheckBundle {
				rep := audBundleReport(t, audits)
				if !rep.ApprovalPrecedesRun || !rep.OK() {
					t.Errorf("the bundle over an honest chain fails verification: "+
						"ApprovalPrecedesRun = %t, OK = %t, time problems %q",
						rep.ApprovalPrecedesRun, rep.OK(), rep.TimeProblems)
				}
			}
		})
	}
}
