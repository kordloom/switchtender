package pgstore_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/decision"
	"github.com/kordloom/switchtender/internal/dispatch"
	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/run"
)

// apprFreshDatabase creates a database of the test's own on the server dsn names and returns a DSN
// for it, dropping it when the test ends, so the race below cannot be disturbed by a contract test
// truncating the shared database.
func apprFreshDatabase(t *testing.T, dsn string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse the test DSN: %v", err)
	}
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("connect to postgres: %v", err)
	}
	defer func() { _ = admin.Close() }()
	name := fmt.Sprintf("st_appr_%d", time.Now().UnixNano())
	if _, err := admin.Exec("CREATE DATABASE " + name); err != nil {
		t.Fatalf("create a database of this test's own: %v", err)
	}
	t.Cleanup(func() {
		drop, derr := sql.Open("pgx", dsn)
		if derr != nil {
			return
		}
		defer func() { _ = drop.Close() }()
		_, _ = drop.Exec("DROP DATABASE IF EXISTS " + name + " WITH (FORCE)")
	})
	u.Path = "/" + name
	return u.String()
}

// apprGatedDecisions holds the first decision record one named decider saves until released, the
// way a replica's decision stalls on its record insert after passing every check.
type apprGatedDecisions struct {
	decision.Store
	// actor is the decider whose save is held.
	actor string
	// reached is closed when the held save arrives.
	reached chan struct{}
	// release lets the held save continue.
	release chan struct{}
	// once makes only the first matching save wait.
	once sync.Once
}

// Save holds the named decider's first record until released, then saves it.
func (g *apprGatedDecisions) Save(ctx context.Context, r *decision.Record) error {
	if r.Actor == g.actor {
		g.once.Do(func() {
			close(g.reached)
			<-g.release
		})
	}
	return g.Store.Save(ctx, r)
}

// apprVerdicts lists the verdicts of the DECISION entries naming run id, approval steps' included,
// leaving out a step's request.
func apprVerdicts(t *testing.T, audits audit.Store, id string) []string {
	t.Helper()
	chain, err := audits.Chain(context.Background())
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	var out []string
	for _, e := range chain {
		if e.Method != audit.MethodDecision {
			continue
		}
		if runID, _, verdict, ok := outcome.ParseStepDecisionPath(e.Path); ok {
			if runID == id && verdict != outcome.StepRequested {
				out = append(out, verdict)
			}
			continue
		}
		if verdict, ok := strings.CutPrefix(e.Path, "/runs/"+id+"/decision/"); ok {
			out = append(out, verdict)
		}
	}
	return out
}

// apprHasOutcome reports whether the chain holds run id's outcome entry.
func apprHasOutcome(audits audit.Store, id string) bool {
	chain, err := audits.Chain(context.Background())
	if err != nil {
		return false
	}
	for _, e := range chain {
		if e.Method == audit.MethodRun && strings.HasPrefix(e.Path, "/runs/"+id+"/outcome/") {
			return true
		}
	}
	return false
}

// TestHAApprovalThatLosesItsRaceLeavesTheChainVerifiable is the two-replica race on PostgreSQL: two
// dispatchers with their own connection pools on one database, deciding the same held run, or the
// same workflow approval step, at once. The approval on the first replica passes its checks and
// stalls on its record insert while the second replica denies the work and commits its outcome.
// When the approval continues, the compare-and-set refuses it and the approver is told the work is
// not awaiting approval, but its DECISION entry is appended anyway, after the outcome. The
// whole-chain bundle then reads as a gate bypass and reports NOT VERIFIED, on an install where
// nothing ran without approval. The reliability page promises approve and reject are
// compare-and-set transitions that two admins on two replicas can race safely.
//
//nolint:funlen // Test function.
func TestHAApprovalThatLosesItsRaceLeavesTheChainVerifiable(t *testing.T) {
	t.Parallel()
	shared := testDSN(t)
	tests := []struct {
		// Name labels the case.
		Name string
		// Step decides a workflow approval step rather than a whole held run.
		Step bool
		// WantVerdicts are the decisions the chain should hold: only the one that took effect.
		WantVerdicts []string
	}{{ // Test 0: A held run another admin rejects while the approval is in flight.
		Name: "held run", WantVerdicts: []string{"rejected"},
	}, { // Test 1: An approval step another approver denies while the approval is in flight.
		Name: "approval step", Step: true, WantVerdicts: []string{outcome.StepRejected},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			dsn := apprFreshDatabase(t, shared)
			dbA, dbB := openReplica(t, dsn), openReplica(t, dsn)
			gated := &apprGatedDecisions{Store: dbA.Decisions(), actor: "approver-a",
				reached: make(chan struct{}), release: make(chan struct{})}
			runner := &commandCounter{seen: map[string]int{}}
			replicaA := dispatch.New(dbA.Runs(), runner, zap.NewNop(), dispatch.WithNoJanitor(),
				dispatch.WithAudits(dbA.Audits()), dispatch.WithDecisions(gated),
				dispatch.WithOwner("replica-a"), dispatch.WithClaimInterval(20*time.Millisecond))
			defer replicaA.Close()
			replicaB := dispatch.New(dbB.Runs(), runner, zap.NewNop(), dispatch.WithNoJanitor(),
				dispatch.WithAudits(dbB.Audits()), dispatch.WithDecisions(dbB.Decisions()),
				dispatch.WithOwner("replica-b"), dispatch.WithClaimInterval(20*time.Millisecond))
			defer replicaB.Close()
			store := dbB.Runs()

			var target, workID string
			if test.Step {
				parent, err := replicaB.SubmitPipeline(ctx, "release", "", []run.PipelineStep{
					{Name: "build", Tool: run.ToolBash, Command: "build"},
					{Name: "gate", Type: run.StepApproval, Description: "Ship it?",
						DependsOn: []string{"build"}},
					{Name: "deploy", Tool: run.ToolBash, Command: "deploy", DependsOn: []string{"gate"}},
				}, run.WithActor("requester"))
				if err != nil {
					t.Fatalf("SubmitPipeline() error = %v", err)
				}
				waitFor(t, "the workflow to park at its approval step", func() bool {
					p, gerr := store.Get(ctx, parent.ID)
					if gerr != nil || p.Status != run.StatusPendingApproval || p.ClaimedBy != "" {
						return false
					}
					pending, perr := run.PendingApprovalSteps(ctx, store, parent.ID)
					if perr != nil || len(pending) != 1 {
						return false
					}
					target = pending[0].ID
					return true
				})
				workID = parent.ID
			} else {
				held, err := replicaB.Submit(ctx, "play.yml", "inv", run.WithRequireApproval(true),
					run.WithActor("requester"))
				if err != nil {
					t.Fatalf("Submit() error = %v", err)
				}
				target, workID = held.ID, held.ID
			}

			approved := make(chan error, 1)
			go func() {
				by := outcome.Decider{Name: "approver-a", Type: "session"}
				var err error
				if test.Step {
					_, err = replicaA.DecideStep(ctx, target, dispatch.StepDecision{Approve: true, By: by})
				} else {
					_, err = replicaA.DecideRun(ctx, target, dispatch.RunDecision{Approve: true, By: by})
				}
				approved <- err
			}()
			select {
			case <-gated.reached:
			case <-time.After(30 * time.Second):
				t.Fatal("the approval never passed its checks")
			}
			denier := outcome.Decider{Name: "approver-b", Type: "session"}
			var err error
			if test.Step {
				_, err = replicaB.DecideStep(ctx, target, dispatch.StepDecision{By: denier})
			} else {
				_, err = replicaB.DecideRun(ctx, target, dispatch.RunDecision{By: denier})
			}
			if err != nil {
				t.Fatalf("deny on the second replica error = %v", err)
			}
			waitFor(t, "the denied work to commit its outcome", func() bool {
				r, gerr := store.Get(ctx, workID)
				return gerr == nil && r.Status.Terminal() && apprHasOutcome(dbB.Audits(), workID)
			})

			close(gated.release)
			if err := <-approved; !errors.Is(err, dispatch.ErrNotPendingApproval) {
				t.Fatalf("the late approval returned %v, want ErrNotPendingApproval", err)
			}
			if diff := cmp.Diff(test.WantVerdicts, apprVerdicts(t, dbB.Audits(), workID)); diff != "" {
				t.Errorf("the chain holds a decision the approver was told did not take effect "+
					"(-want +got):\n%s", diff)
			}
			id, err := audit.LoadIdentity(t.TempDir())
			if err != nil {
				t.Fatalf("LoadIdentity() error = %v", err)
			}
			chain, err := dbB.Audits().Chain(ctx)
			if err != nil {
				t.Fatalf("Chain() error = %v", err)
			}
			doc, err := audit.BuildBundle(chain, id, "test", time.Now())
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
			if !rep.OK() {
				t.Errorf("the whole-chain bundle of an install where no gate was bypassed reports "+
					"NOT VERIFIED: %q", rep.TimeProblems)
			}
		})
	}
}
