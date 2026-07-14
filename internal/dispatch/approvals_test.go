package dispatch

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/dcadolph/railwarden/internal/roundhouse"
	"github.com/dcadolph/railwarden/internal/run"
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
	approved, err := d.Approve(context.Background(), created.ID)
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
	rejected, err := d.Reject(context.Background(), created.ID, "not allowed on prod")
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
	if got.Status != run.StatusRejected || got.Error != "not allowed on prod" {
		t.Errorf("stored = {status:%q error:%q}, want rejected with the reason", got.Status, got.Error)
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

	if _, err := d.Approve(context.Background(), created.ID); !errors.Is(err, ErrNotPendingApproval) {
		t.Errorf("Approve on a non-held run: err = %v, want ErrNotPendingApproval", err)
	}
	if _, err := d.Reject(context.Background(), created.ID, ""); !errors.Is(err, ErrNotPendingApproval) {
		t.Errorf("Reject on a non-held run: err = %v, want ErrNotPendingApproval", err)
	}
}
