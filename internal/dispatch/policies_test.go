package dispatch

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/dcadolph/railwarden/internal/policy"
	"github.com/dcadolph/railwarden/internal/roundhouse"
	"github.com/dcadolph/railwarden/internal/run"
)

// TestPolicyHoldsMatchingRun confirms a run matching a stored policy is held for approval at submit
// without any opt-in, while a non-matching run runs normally.
func TestPolicyHoldsMatchingRun(t *testing.T) {
	t.Parallel()
	store := run.NewMemStore()
	policies := policy.NewMemStore()
	if err := policies.Save(context.Background(), &policy.Policy{
		ID: policy.NewID(), Name: "tf-destroy", Tool: "terraform", CommandContains: "destroy",
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	runner := roundhouse.RunnerFunc(
		func(_ context.Context, _ roundhouse.Spec, _ io.Writer) (roundhouse.Result, error) {
			return roundhouse.Result{ExitCode: 0}, nil
		},
	)
	d := New(store, runner, nil, WithPolicies(policies))
	defer d.Close()

	// A matching run is held for approval with no opt-in.
	held, err := d.Submit(context.Background(), "", "",
		run.WithTool("terraform"), run.WithCommand("terraform destroy prod"))
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	if held.Status != run.StatusPendingApproval {
		t.Fatalf("matching run status = %q, want pending_approval", held.Status)
	}
	time.Sleep(50 * time.Millisecond)
	got, err := store.Get(context.Background(), held.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Status != run.StatusPendingApproval {
		t.Errorf("held run status = %q, want still pending_approval", got.Status)
	}

	// A non-matching run is not held and runs to completion.
	free, err := d.Submit(context.Background(), "", "",
		run.WithTool("terraform"), run.WithCommand("terraform apply"))
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	if free.Status == run.StatusPendingApproval {
		t.Fatal("non-matching run should not be held")
	}
	final := waitTerminal(t, store, free.ID)
	if final.Status != run.StatusSucceeded {
		t.Errorf("non-matching run status = %q, want succeeded", final.Status)
	}
}
