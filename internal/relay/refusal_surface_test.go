package relay_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/event"
	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/relay"
	"github.com/kordloom/switchtender/internal/run"
)

// namingStore is a run.Store whose every execution-path method fails with an error naming the method
// that was called. It is how the delegation tests tell "the call was forwarded" from "the call was
// forwarded to the right method": a transport that answered AppendLog by calling AppendEvents would
// satisfy every rule-level test and still scramble a run's record.
type namingStore struct {
	// Store supplies the rest of the method set; nothing in these tests calls it.
	run.Store
}

// errFrom is the error namingStore returns, naming the method the caller reached.
func errFrom(method string) error { return fmt.Errorf("namingStore: %s", method) }

// Claim reports that Claim was reached.
func (namingStore) Claim(context.Context, string, []string) (*run.Run, error) {
	return nil, errFrom("Claim")
}

// Heartbeat reports that Heartbeat was reached.
func (namingStore) Heartbeat(context.Context, string, string) error { return errFrom("Heartbeat") }

// Get reports that Get was reached.
func (namingStore) Get(context.Context, string) (*run.Run, error) { return nil, errFrom("Get") }

// Save reports that Save was reached.
func (namingStore) Save(context.Context, *run.Run) error { return errFrom("Save") }

// AppendLog reports that AppendLog was reached.
func (namingStore) AppendLog(context.Context, string, []byte) error { return errFrom("AppendLog") }

// AppendEvents reports that AppendEvents was reached.
func (namingStore) AppendEvents(context.Context, string, []event.Event) error {
	return errFrom("AppendEvents")
}

// SaveHostSummary reports that SaveHostSummary was reached.
func (namingStore) SaveHostSummary(context.Context, string, []run.HostSummary) error {
	return errFrom("SaveHostSummary")
}

// SaveHostFacts reports that SaveHostFacts was reached.
func (namingStore) SaveHostFacts(context.Context, string, []run.HostFacts) error {
	return errFrom("SaveHostFacts")
}

// SaveTaskSummary reports that SaveTaskSummary was reached.
func (namingStore) SaveTaskSummary(context.Context, string, []run.TaskSummary) error {
	return errFrom("SaveTaskSummary")
}

// TestLoopbackForwardsEachCallToItsOwnStoreMethod pins the wiring inside the loopback transport, one
// method at a time. A transport that forwarded a call to the wrong store method would pass every test
// that only checks a call succeeded, while writing a run's output into its event stream or its task
// timings over its host outcomes. The store answers with the name of whatever it was asked for, so a
// crossed wire is visible rather than merely survivable.
func TestLoopbackForwardsEachCallToItsOwnStoreMethod(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tr := relay.Loopback(namingStore{})
	tests := []struct {
		Call     func() error
		WantName string
	}{{ // Test 0: Claiming work.
		WantName: "Claim", Call: func() error { _, err := tr.Claim(ctx, "w", nil); return err },
	}, { // Test 1: Renewing the lease.
		WantName: "Heartbeat", Call: func() error { return tr.Heartbeat(ctx, "run_1", "w") },
	}, { // Test 2: Reading the claimed run and its cancel flag.
		WantName: "Get", Call: func() error { _, err := tr.Get(ctx, "run_1"); return err },
	}, { // Test 3: Reporting a status transition.
		WantName: "Save", Call: func() error { return tr.Save(ctx, &run.Run{ID: "run_1"}) },
	}, { // Test 4: Streaming captured output.
		WantName: "AppendLog", Call: func() error { return tr.AppendLog(ctx, "run_1", []byte("x")) },
	}, { // Test 5: Streaming structured events.
		WantName: "AppendEvents",
		Call: func() error {
			return tr.AppendEvents(ctx, "run_1", []event.Event{{Type: event.TypeRunnerOK}})
		},
	}, { // Test 6: Recording per-host outcomes.
		WantName: "SaveHostSummary",
		Call: func() error {
			return tr.SaveHostSummary(ctx, "run_1", []run.HostSummary{{Host: "web01"}})
		},
	}, { // Test 7: Recording gathered facts.
		WantName: "SaveHostFacts",
		Call: func() error {
			return tr.SaveHostFacts(ctx, "run_1", []run.HostFacts{{Host: "web01"}})
		},
	}, { // Test 8: Recording per-task durations.
		WantName: "SaveTaskSummary",
		Call: func() error {
			return tr.SaveTaskSummary(ctx, "run_1", []run.TaskSummary{{Task: "ping"}})
		},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.WantName), func(t *testing.T) {
			t.Parallel()
			err := test.Call()
			if err == nil {
				t.Fatalf("%s was not forwarded to the backing store at all", test.WantName)
			}
			if diff := cmp.Diff(errFrom(test.WantName).Error(), err.Error()); diff != "" {
				t.Errorf("the call reached the wrong store method (-want +got):\n%s", diff)
			}
		})
	}
}

// TestLoopbackRefusesWhatItCannotServe pins the two calls a loopback transport does not carry. It
// wraps a local store, so the dispatcher is handed the real policy store directly and the caller
// submits its own runs rather than asking anyone. Answering these with a nil error and an empty
// result would silently give a locally running worker no plan-content gate at all.
func TestLoopbackRefusesWhatItCannotServe(t *testing.T) {
	t.Parallel()
	tr := relay.Loopback(run.NewMemStore())
	got, err := tr.Policies(context.Background())
	if !errors.Is(err, relay.ErrUnsupported) {
		t.Errorf("Policies() error = %v, want ErrUnsupported", err)
	}
	if got != nil {
		t.Errorf("Policies() returned %v alongside its refusal", got)
	}
	proposal, err := tr.ProposeApply(context.Background(), "run_plan", 3, true)
	if !errors.Is(err, relay.ErrUnsupported) {
		t.Errorf("ProposeApply() error = %v, want ErrUnsupported", err)
	}
	if proposal != nil {
		t.Errorf("ProposeApply() returned %+v alongside its refusal", proposal)
	}
}

// TestClientRefusesEveryControlNodeCall pins the whole refusal surface a relay run store presents.
//
// A Client is handed to the dispatcher in place of a database-backed store, so every method the
// control node serves has to refuse rather than answer. A query that quietly returned an empty result
// would read as "there is nothing" on a machine that simply cannot see: an empty non-terminal list
// would tell a janitor sweep every run had settled, an empty worker list would report the fleet
// empty, and a false from a status transition would look like a lost race rather than a refusal.
// Each one also has to hand back the zero value, so a caller that ignores the error is not handed a
// half-built answer.
//
//nolint:funlen // Test function.
func TestClientRefusesEveryControlNodeCall(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := relay.NewClient(relay.Loopback(run.NewMemStore()))
	tests := []struct {
		Call func() error
		Name string
	}{{ // Test 0: Approving a held run happens where the policy and the approver are.
		Name: "TransitionStatusAndClaim",
		Call: func() error {
			ok, err := c.TransitionStatusAndClaim(ctx, "run_1", run.StatusPendingApproval,
				run.StatusPending, "w")
			return zeroOr(err, ok, false)
		},
	}, { // Test 1: The idempotency lookup is a control-node query.
		Name: "ByIdempotencyKey",
		Call: func() error { r, err := c.ByIdempotencyKey(ctx, "k"); return zeroOr(err, r, nil) },
	}, { // Test 2: Listing runs is a control-node query.
		Name: "List", Call: func() error { r, err := c.List(ctx); return zeroOr(err, r, nil) },
	}, { // Test 3: Paged listing is a control-node query.
		Name: "ListPage",
		Call: func() error { r, err := c.ListPage(ctx, run.ListFilter{}, 10, 0); return zeroOr(err, r, nil) },
	}, { // Test 4: Status counts are a control-node query.
		Name: "RunStatusCounts",
		Call: func() error { r, err := c.RunStatusCounts(ctx); return zeroOr(err, r, nil) },
	}, { // Test 5: Run timings are a control-node read.
		Name: "RunTimings", Call: func() error { r, err := c.RunTimings(ctx, 5); return zeroOr(err, r, nil) },
	}, { // Test 6: A split's shards are a control-node query.
		Name: "Shards", Call: func() error { r, err := c.Shards(ctx, "run_1"); return zeroOr(err, r, nil) },
	}, { // Test 7: A pipeline's steps are a control-node query.
		Name: "Steps", Call: func() error { r, err := c.Steps(ctx, "run_1"); return zeroOr(err, r, nil) },
	}, { // Test 8: The non-terminal sweep list must never read as empty on a worker.
		Name: "NonTerminal", Call: func() error { r, err := c.NonTerminal(ctx); return zeroOr(err, r, nil) },
	}, { // Test 9: Reclaiming stale leases is the control node's sweep.
		Name: "ReclaimStale",
		Call: func() error { n, err := c.ReclaimStale(ctx, time.Minute); return zeroOr(err, n, 0) },
	}, { // Test 10: Cancellation is issued by the control node's API.
		Name: "RequestCancel", Call: func() error { return c.RequestCancel(ctx, "run_1") },
	}, { // Test 11: A bare status transition is a control-node operation.
		Name: "TransitionStatus",
		Call: func() error {
			ok, err := c.TransitionStatus(ctx, "run_1", run.StatusPending, run.StatusRunning)
			return zeroOr(err, ok, false)
		},
	}, { // Test 12: Stamping an approved spec is a decision, made where the approver is.
		Name: "StampApprovedSpec", Call: func() error { return c.StampApprovedSpec(ctx, "run_1", "d") },
	}, { // Test 13: Finalizing is applied by the control node from a worker's report.
		Name: "FinalizeRunning",
		Call: func() error {
			ok, err := c.FinalizeRunning(ctx, "run_1", run.Finalization{})
			return zeroOr(err, ok, false)
		},
	}, { // Test 14: Progress is applied by the control node from a worker's report.
		Name: "ApplyRunningProgress",
		Call: func() error {
			ok, err := c.ApplyRunningProgress(ctx, "run_1", "w", run.Progress{})
			return zeroOr(err, ok, false)
		},
	}, { // Test 15: The worker listing is a control-node query.
		Name: "Workers", Call: func() error { r, err := c.Workers(ctx); return zeroOr(err, r, nil) },
	}, { // Test 16: Fleet health is a control-node analytic.
		Name: "FleetHealth", Call: func() error { r, err := c.FleetHealth(ctx, 5); return zeroOr(err, r, nil) },
	}, { // Test 17: Drift status is a control-node analytic.
		Name: "DriftStatus", Call: func() error { r, err := c.DriftStatus(ctx); return zeroOr(err, r, nil) },
	}, { // Test 18: Host costs are a control-node analytic.
		Name: "HostCosts", Call: func() error { r, err := c.HostCosts(ctx, 5); return zeroOr(err, r, nil) },
	}, { // Test 19: Host history is a control-node query.
		Name: "HostHistory",
		Call: func() error { r, err := c.HostHistory(ctx, "web01", 5); return zeroOr(err, r, nil) },
	}, { // Test 20: A run's stored host summaries are a control-node read.
		Name: "RunHostSummaries",
		Call: func() error { r, err := c.RunHostSummaries(ctx, "run_1"); return zeroOr(err, r, nil) },
	}, { // Test 21: A run's stored task summaries are a control-node read.
		Name: "RunTaskSummaries",
		Call: func() error { r, err := c.RunTaskSummaries(ctx, "run_1"); return zeroOr(err, r, nil) },
	}, { // Test 22: Stored facts for a host are a control-node read.
		Name: "HostFactsFor",
		Call: func() error { r, err := c.HostFactsFor(ctx, "web01"); return zeroOr(err, r, nil) },
	}, { // Test 23: Task trends are a control-node analytic.
		Name: "TaskTrends", Call: func() error { r, err := c.TaskTrends(ctx, 5); return zeroOr(err, r, nil) },
	}, { // Test 24: Reading a whole log back is a control-node query.
		Name: "Log", Call: func() error { r, err := c.Log(ctx, "run_1"); return zeroOr(err, r, nil) },
	}, { // Test 25: Tailing a log is a control-node query.
		Name: "LogAfter", Call: func() error { r, err := c.LogAfter(ctx, "run_1", 0, 10); return zeroOr(err, r, nil) },
	}, { // Test 26: The last log sequence is a control-node query.
		Name: "LastLogSeq", Call: func() error { n, err := c.LastLogSeq(ctx, "run_1"); return zeroOr(err, n, int64(0)) },
	}, { // Test 27: Canceling a pending run is a control-node mutation.
		Name: "CancelPending",
		Call: func() error { ok, err := c.CancelPending(ctx, "run_1"); return zeroOr(err, ok, false) },
	}, { // Test 28: Reading a run's events back is a control-node query.
		Name: "Events", Call: func() error { r, err := c.Events(ctx, "run_1"); return zeroOr(err, r, nil) },
	}, { // Test 29: Tailing events is a control-node query.
		Name: "EventsAfter",
		Call: func() error { r, err := c.EventsAfter(ctx, "run_1", 0, 10); return zeroOr(err, r, nil) },
	}, { // Test 30: The last event sequence is a control-node query.
		Name: "LastEventSeq",
		Call: func() error { n, err := c.LastEventSeq(ctx, "run_1"); return zeroOr(err, n, int64(0)) },
	}, { // Test 31: Purging events is a control-node retention sweep.
		Name: "PurgeEventsBefore",
		Call: func() error { n, err := c.PurgeEventsBefore(ctx, time.Now()); return zeroOr(err, n, 0) },
	}, { // Test 32: Purging runs is a control-node retention sweep.
		Name: "PurgeRunsBefore",
		Call: func() error { n, err := c.PurgeRunsBefore(ctx, time.Now()); return zeroOr(err, n, 0) },
	}, { // Test 33: Trimming summaries is a control-node retention sweep.
		Name: "TrimSummaries",
		Call: func() error { n, err := c.TrimSummaries(ctx, 100); return zeroOr(err, n, 0) },
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			err := test.Call()
			if !errors.Is(err, relay.ErrUnsupported) {
				t.Errorf("%s error = %v, want ErrUnsupported: a control-node read answered from a "+
					"worker reads as an empty estate rather than as a refusal", test.Name, err)
			}
		})
	}
}

// zeroOr returns err when the call refused, or a complaint when it handed back something other than
// the zero value alongside its refusal. A caller that ignores the error must not receive an answer.
func zeroOr[T any](err error, got, want T) error {
	if diff := cmp.Diff(want, got); diff != "" {
		return fmt.Errorf("refused but returned a value (-want +got):\n%s", diff)
	}
	return err
}

// TestPolicyClientGet pins the single-policy read a worker makes across the relay. It is built on the
// list, so it inherits the list's fail-closed rule: a control node that cannot answer must not look
// like a control node holding no such policy, because the caller treats not-found as "no rule applies
// here" and an unreachable store as a gate that has not been passed.
func TestPolicyClientGet(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	backing := policy.NewMemStore()
	want := &policy.Policy{ID: "pol_destroy", Name: "large destroy", Tool: "terraform",
		MaxDestroy: 5}
	if err := backing.Save(ctx, want); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	c := newPolicyRelay(t, backing)

	got, err := c.Get(ctx, "pol_destroy")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if diff := cmp.Diff(want.ID, got.ID); diff != "" {
		t.Errorf("policy id mismatch (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(want.MaxDestroy, got.MaxDestroy); diff != "" {
		t.Errorf("destroy threshold mismatch (-want +got):\n%s", diff)
	}

	// A policy the control node does not hold is not found, which is a real answer.
	if _, err := c.Get(ctx, "pol_absent"); !errors.Is(err, policy.ErrNotFound) {
		t.Errorf("Get(absent) error = %v, want ErrNotFound", err)
	}
	// An empty id matches nothing rather than the first policy in the list.
	if _, err := c.Get(ctx, ""); !errors.Is(err, policy.ErrNotFound) {
		t.Errorf("Get(\"\") error = %v, want ErrNotFound", err)
	}
}

// TestPolicyClientGetFailsClosedWhenItCannotAsk pins that a severed link makes a single-policy read an
// error rather than a not-found. The two answers mean opposite things to a gate: not-found says no
// such rule exists, and an error says the rule could not be read. Collapsing them would let a worker
// treat an unreachable control node as an install with no rules at all.
func TestPolicyClientGetFailsClosedWhenItCannotAsk(t *testing.T) {
	t.Parallel()
	tr, _ := scriptedTransport(t, 503, `{"error":"the policy store is down"}`)
	c := relay.NewPolicyClient(tr)

	got, err := c.Get(context.Background(), "pol_destroy")
	if err == nil {
		t.Fatalf("Get() answered %+v with the control node unable to list", got)
	}
	if errors.Is(err, policy.ErrNotFound) {
		t.Error("Get() reported not-found for a policy it could not read, so an unreachable " +
			"control node reads as an install holding no such rule")
	}
	if !errors.Is(err, policy.ErrUnreachable) {
		t.Errorf("Get() error = %v, want it to wrap ErrUnreachable so callers fail closed", err)
	}
}
