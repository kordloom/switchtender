package dispatch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// TestRunApprovalGate confirms a run submitted with approval required is held, not executed, until
// it is approved, after which it runs to completion.
func TestRunApprovalGate(t *testing.T) {
	t.Parallel()
	store := run.NewMemStore()
	ran := make(chan struct{}, 1)
	runner := roundhouse.RunnerFunc(
		func(_ context.Context, _ roundhouse.Spec, _ io.Writer) (roundhouse.Result, error) {
			select {
			case ran <- struct{}{}:
			default:
			}
			return roundhouse.Result{ExitCode: 0}, nil
		},
	)
	d := New(store, runner, nil)
	defer d.Close()

	created, err := d.Submit(context.Background(), "play.yml", "inv", run.WithRequireApproval(true))
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	if created.Status != run.StatusPendingApproval {
		t.Fatalf("submitted status = %q, want pending_approval", created.Status)
	}

	// The held run must not be claimed or executed.
	time.Sleep(50 * time.Millisecond)
	held, err := store.Get(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if held.Status != run.StatusPendingApproval {
		t.Fatalf("held status = %q, want still pending_approval", held.Status)
	}
	select {
	case <-ran:
		t.Fatal("held run should not have executed before approval")
	default:
	}

	// Approval releases it to run.
	approved, err := d.Approve(context.Background(), created.ID, decider("approver-pat", "session"))
	if err != nil {
		t.Fatalf("Approve() error = %v", err)
	}
	if approved.Status != run.StatusPending {
		t.Errorf("approved status = %q, want pending", approved.Status)
	}
	final := waitTerminal(t, store, created.ID)
	if final.Status != run.StatusSucceeded {
		t.Errorf("final status = %q, want succeeded", final.Status)
	}
}

// TestRunReject confirms rejecting a held run makes it terminal with the reason recorded.
func TestRunReject(t *testing.T) {
	t.Parallel()
	store := run.NewMemStore()
	d := New(store, roundhouse.RunnerFunc(
		func(_ context.Context, _ roundhouse.Spec, _ io.Writer) (roundhouse.Result, error) {
			return roundhouse.Result{}, nil
		}), nil)
	defer d.Close()

	created, err := d.Submit(context.Background(), "play.yml", "inv", run.WithRequireApproval(true))
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	rejected, err := d.Reject(context.Background(), created.ID, "not allowed on prod",
		decider("approver-pat", "session"))
	if err != nil {
		t.Fatalf("Reject() error = %v", err)
	}
	if rejected.Status != run.StatusRejected || !rejected.Status.Terminal() {
		t.Errorf("rejected status = %q, want a terminal rejected", rejected.Status)
	}
	got, err := store.Get(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	// The run's error says it was rejected and does not carry the reason: the reason is kept in the
	// decision record, where a redaction can remove it, and never in the run's committed outcome.
	if got.Status != run.StatusRejected || got.Error != "rejected by an approver" {
		t.Errorf("stored = {status:%q error:%q}, want rejected by an approver", got.Status, got.Error)
	}
	records, err := d.Decisions(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("Decisions() error = %v", err)
	}
	if len(records) != 1 || records[0].Reason == nil ||
		records[0].Reason.Text != "not allowed on prod" {
		t.Errorf("decision records = %+v, want one rejection carrying the reason", records)
	}
}

// TestApproveRejectRequireHeld confirms approve and reject only apply to a run awaiting approval.
func TestApproveRejectRequireHeld(t *testing.T) {
	t.Parallel()
	store := run.NewMemStore()
	d := New(store, roundhouse.RunnerFunc(
		func(_ context.Context, _ roundhouse.Spec, _ io.Writer) (roundhouse.Result, error) {
			return roundhouse.Result{}, nil
		}), nil)
	defer d.Close()

	created, err := d.Submit(context.Background(), "play.yml", "inv")
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	waitTerminal(t, store, created.ID)

	if _, err := d.Approve(context.Background(), created.ID,
		decider("approver-pat", "session")); !errors.Is(err, ErrNotPendingApproval) {
		t.Errorf("Approve on a non-held run: err = %v, want ErrNotPendingApproval", err)
	}
	if _, err := d.Reject(context.Background(), created.ID, "",
		decider("approver-pat", "session")); !errors.Is(err, ErrNotPendingApproval) {
		t.Errorf("Reject on a non-held run: err = %v, want ErrNotPendingApproval", err)
	}
}

// TestAnApprovedParentThatCannotStartClosesItsStream pins that a split or pipeline that fails as it
// starts after approval tells its live viewers the run ended. Every other path that settles a parent
// closes its stream; these three did not, so a browser watching the parent waited on a stream for a
// run that was already failed.
func TestAnApprovedParentThatCannotStartClosesItsStream(t *testing.T) {
	t.Parallel()
	two := 2
	tests := []struct {
		Parent     *run.Run
		Shards     int
		Split      bool
		WantReason string
	}{{ // Test 0: A split whose shards are gone.
		Parent: &run.Run{Kind: run.KindSplit, ShardCount: &two}, Split: true,
		WantReason: "split has no stored shards to run",
	}, { // Test 1: A split holding fewer shards than it counts.
		Parent: &run.Run{Kind: run.KindSplit, ShardCount: &two}, Shards: 1, Split: true,
		WantReason: "only 1 of 2 shards were stored",
	}, { // Test 2: A pipeline whose steps were never stored.
		Parent:     &run.Run{Kind: run.KindPipeline},
		WantReason: "pipeline has no stored steps to run",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			store := run.NewMemStore()
			pub := newRecordingPublisher()
			idle := roundhouse.RunnerFunc(
				func(context.Context, roundhouse.Spec, io.Writer) (roundhouse.Result, error) {
					return roundhouse.Result{}, nil
				})
			d := New(store, idle, nil, WithPublisher(pub))
			defer d.Close()
			parent := test.Parent.Clone()
			parent.ID, parent.Playbook, parent.Status = "run_parent", "site.yml", run.StatusPending
			if err := store.Save(ctx, parent); err != nil {
				t.Fatalf("Save(parent) error = %v", err)
			}
			for i := range test.Shards {
				idx, pid := i, parent.ID
				if err := store.Save(ctx, &run.Run{
					ID: fmt.Sprintf("run_shard_%d", i), Playbook: "site.yml", ParentID: &pid,
					ShardIndex: &idx, ShardCount: parent.ShardCount, Status: run.StatusPendingApproval,
				}); err != nil {
					t.Fatalf("Save(shard) error = %v", err)
				}
			}
			if test.Split {
				d.startSplit(ctx, parent)
			} else {
				d.startPipeline(parent)
			}
			got, err := store.Get(ctx, parent.ID)
			if err != nil {
				t.Fatalf("Get(parent) error = %v", err)
			}
			if got.Status != run.StatusFailed || !strings.Contains(got.Error, test.WantReason) {
				t.Errorf("parent = %s %q, want failed with %q", got.Status, got.Error, test.WantReason)
			}
			pub.mu.Lock()
			defer pub.mu.Unlock()
			if n := pub.closed[parent.ID]; n != 1 {
				t.Errorf("CloseRun(parent) called %d time(s), want exactly 1", n)
			}
		})
	}
}
