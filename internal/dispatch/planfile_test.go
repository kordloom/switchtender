package dispatch

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// planApplyRunner counts the applies that reached the tool and records whether each carried a plan
// file, failing any plan, so a test can prove a refused apply neither ran nor planned again.
type planApplyRunner struct {
	// applies counts executions that were not dry runs.
	applies atomic.Int64
	// plans counts dry runs, which a gated apply must never fall back to.
	plans atomic.Int64
	// secret, when set, is handed to the masker as a sensitive plan value and then printed.
	secret string
}

// Run counts the execution and, for an apply carrying a plan file, reveals and prints the secret.
func (p *planApplyRunner) Run(_ context.Context, spec roundhouse.Spec, out io.Writer) (roundhouse.Result,
	error) {
	if spec.DryRun {
		p.plans.Add(1)
		return roundhouse.Result{ExitCode: 1}, nil
	}
	p.applies.Add(1)
	if spec.PlanFile == "" {
		return roundhouse.Result{ExitCode: 1}, nil
	}
	if p.secret != "" && spec.AddSecrets != nil {
		spec.AddSecrets([]string{p.secret})
		_, _ = io.WriteString(out, "applying with password "+p.secret+"\n")
	}
	return roundhouse.Result{ExitCode: 0}, nil
}

// TestAGatedApplyWhosePlanFileCannotBeTrustedIsRefused covers the ways the plan file an apply's
// approval bound can be missing or wrong. Each refuses the apply before the tool starts, and none
// falls back to planning again, which would apply something nobody weighed or approved.
func TestAGatedApplyWhosePlanFileCannotBeTrustedIsRefused(t *testing.T) {
	t.Parallel()
	const planBytes = "approved plan file"
	sealed, err := snapshotSealer.Seal(base64.StdEncoding.EncodeToString([]byte(planBytes)))
	if err != nil {
		t.Fatalf("Seal() error = %v", err)
	}
	other, err := snapshotSealer.Seal(base64.StdEncoding.EncodeToString([]byte("another plan")))
	if err != nil {
		t.Fatalf("Seal() error = %v", err)
	}
	foreign, err := credential.NewSealer("another-pass", "another-salt").Seal(
		base64.StdEncoding.EncodeToString([]byte(planBytes)))
	if err != nil {
		t.Fatalf("Seal() error = %v", err)
	}
	plain := plainPrefix + base64.StdEncoding.EncodeToString([]byte(planBytes))
	tests := []struct {
		// Name labels the case.
		Name string
		// Bound is the sealed plan the approval binds.
		Bound string
		// Stored is the sealed plan stored on the run, empty for none.
		Stored string
		// WantError is what the refusal must say.
		WantError string
	}{{ // Test 0: The plan file is gone.
		Name: "missing", Bound: sealed, WantError: "is missing",
	}, { // Test 1: Another plan file stored under the digest that bound the first.
		Name: "changed", Bound: sealed, Stored: other, WantError: "changed after",
	}, { // Test 2: A plan file sealed under another key does not open.
		Name: "foreign key", Bound: foreign, Stored: foreign, WantError: "does not open",
	}, { // Test 3: A plan file stored plain on an install that holds a key does not open.
		Name: "stored plain", Bound: plain, Stored: plain, WantError: "does not open",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			store := run.NewMemStore()
			runner := &planApplyRunner{}
			d := New(store, runner, nil, WithAudits(audit.NewMemStore()),
				WithCredentials(credential.NewMemStore(), snapshotSealer), WithRunFilesRoot(t.TempDir()),
				WithNoJanitor(), WithClaimGate(func() error { return errNoClaim }))
			t.Cleanup(d.Close)
			apply := &run.Run{
				ID: "run_apply", Tool: run.ToolTerraform, Command: "infra/prod", ProposedFrom: "run_plan",
				Status: run.StatusPendingApproval, CreatedAt: time.Now(),
				PlanSHA256: run.SealedBlobSHA256(test.Bound), PlanSealed: test.Stored,
			}
			if err := store.Save(ctx, apply); err != nil {
				t.Fatalf("Save() error = %v", err)
			}
			if _, err := d.Approve(ctx, apply.ID, decider("approver-1", "session")); err != nil {
				t.Fatalf("Approve() error = %v", err)
			}
			executor := New(store, runner, nil, WithCredentials(credential.NewMemStore(), snapshotSealer),
				WithRunFilesRoot(t.TempDir()), WithNoJanitor())
			t.Cleanup(executor.Close)
			final := waitTerminal(t, store, apply.ID)
			if final.Status != run.StatusFailed || !strings.Contains(final.Error, test.WantError) {
				t.Errorf("apply ended %q (%q), want failed saying %q", final.Status, final.Error,
					test.WantError)
			}
			if n := runner.applies.Load() + runner.plans.Load(); n != 0 {
				t.Errorf("the tool ran %d times for an apply whose plan file could not be trusted", n)
			}
		})
	}
}

// TestAPlansSensitiveValuesAreMaskedWhileItApplies pins the masking half of treating a plan file as a
// secret. The tool reveals the values a saved plan marks sensitive, and they join the run's masker
// before the tool prints anything, so a value the apply then prints is stored masked.
func TestAPlansSensitiveValuesAreMaskedWhileItApplies(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const secret = "plan-sensitive-value-7731"
	sealed, err := snapshotSealer.Seal(base64.StdEncoding.EncodeToString([]byte("plan")))
	if err != nil {
		t.Fatalf("Seal() error = %v", err)
	}
	store := run.NewMemStore()
	runner := &planApplyRunner{secret: secret}
	if err := store.Save(ctx, &run.Run{
		ID: "run_apply", Tool: run.ToolTerraform, Command: "infra/prod", ProposedFrom: "run_plan",
		Status: run.StatusPending, CreatedAt: time.Now(),
		PlanSHA256: run.SealedBlobSHA256(sealed), PlanSealed: sealed,
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	d := New(store, runner, nil, WithCredentials(credential.NewMemStore(), snapshotSealer),
		WithRunFilesRoot(t.TempDir()), WithNoJanitor())
	t.Cleanup(d.Close)
	final := waitTerminal(t, store, "run_apply")
	if final.Status != run.StatusSucceeded {
		t.Fatalf("apply ended %q (%s), want succeeded", final.Status, final.Error)
	}
	log, err := store.Log(ctx, "run_apply")
	if err != nil {
		t.Fatalf("Log() error = %v", err)
	}
	if strings.Contains(string(log), secret) || !strings.Contains(string(log), "applying with password") {
		t.Errorf("the stored log = %q, want the plan's sensitive value masked", log)
	}
}
