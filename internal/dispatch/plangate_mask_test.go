package dispatch

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// planGateSecret is the credential value the plan gate's failure paths must never store.
const planGateSecret = "supersecretplanvalue"

// rejectProposalStore is a run store that fails to save a proposed apply, with a message that quotes
// the row it could not write. A database error naming its parameters is how a real one reads, and the
// row carries the run's resolved secrets, so the plan gate must mask what it reports.
type rejectProposalStore struct {
	// Store is the real store every other call passes through to.
	run.Store
}

// Save rejects a proposed apply and stores everything else.
func (s *rejectProposalStore) Save(ctx context.Context, r *run.Run) error {
	if r.ProposedFrom != "" {
		return errors.New("insert run: constraint violated by TF_TOKEN=" + planGateSecret)
	}
	return s.Store.Save(ctx, r)
}

// newPlanGateSecretDispatcher builds a dispatcher whose runs carry a credential resolving to
// planGateSecret and whose terraform applies are plan-gated, and returns it with the run store.
func newPlanGateSecretDispatcher(t *testing.T, store run.Store, runner roundhouse.Runner) *Dispatcher {
	t.Helper()
	sealer := credential.NewSealer("pass", "salt")
	sealed, err := sealer.Seal("TF_TOKEN=" + planGateSecret)
	if err != nil {
		t.Fatalf("Seal() error = %v", err)
	}
	creds := credential.NewMemStore()
	if err := creds.Save(context.Background(), &credential.Credential{
		ID: "cred_1", Name: "tf", Kind: credential.KindEnv,
		Source: credential.SourceLocal, Secret: sealed,
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	policies := policy.NewMemStore()
	if err := policies.Save(context.Background(), &policy.Policy{
		ID: policy.NewID(), Name: "tf-destroy-guard", Tool: run.ToolTerraform, MaxDestroy: 1,
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	return New(store, runner, nil, WithPolicies(policies), WithCredentials(creds, sealer))
}

// TestPlanGateMasksSecretInStoredError drives both plan-gate failure paths with a credential in
// scope and confirms the stored run's Error field carries the mask rather than the secret. The log
// line is not the only place this text lands: the run record is what an operator opens and what the
// API hands back, so an unmasked failure there is the leak.
func TestPlanGateMasksSecretInStoredError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		// Name labels the plan-gate failure path under test.
		Name string
		// Runner is the tool behavior that provokes the failure.
		Runner roundhouse.Runner
		// WrapStore reports whether the proposed apply must fail to save.
		WrapStore bool
		// WantContains is a fragment proving the intended path wrote the failure.
		WantContains string
	}{{ // Test 0: The plan itself fails and its error quotes the secret.
		Name: "plan fails",
		Runner: roundhouse.RunnerFunc(
			func(_ context.Context, _ roundhouse.Spec, _ io.Writer) (roundhouse.Result, error) {
				return roundhouse.Result{}, errors.New("terraform plan: TF_TOKEN=" + planGateSecret)
			}),
		WantContains: "terraform plan:",
	}, { // Test 1: The plan succeeds but the proposed apply cannot be written.
		Name:         "proposal fails",
		Runner:       planRunner("Plan: 0 to add, 0 to change, 3 to destroy"),
		WrapStore:    true,
		WantContains: "propose apply:",
	}}

	for testNum, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			mem := run.NewMemStore()
			store := run.Store(mem)
			if test.WrapStore {
				store = &rejectProposalStore{Store: mem}
			}
			d := newPlanGateSecretDispatcher(t, store, test.Runner)
			defer d.Close()

			created, err := d.Submit(context.Background(), "", "",
				run.WithTool(run.ToolTerraform), run.WithCommand("infra/prod"),
				run.WithCredentialIDs([]string{"cred_1"}))
			if err != nil {
				t.Fatalf("test %d: Submit() error = %v", testNum, err)
			}

			got := waitTerminal(t, mem, created.ID)
			if got.Status != run.StatusFailed {
				t.Fatalf("test %d: run status = %q, want failed", testNum, got.Status)
			}
			if !strings.Contains(got.Error, test.WantContains) {
				t.Fatalf("test %d: stored error = %q, want the %s failure",
					testNum, got.Error, test.Name)
			}
			if strings.Contains(got.Error, planGateSecret) {
				t.Errorf("test %d: secret leaked into the stored run error: %q", testNum, got.Error)
			}
			if !strings.Contains(got.Error, maskToken) {
				t.Errorf("test %d: stored error = %q, want the secret replaced by %q",
					testNum, got.Error, maskToken)
			}
		})
	}
}
