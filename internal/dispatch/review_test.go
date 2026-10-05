package dispatch

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/run"
)

// TestPlanSummaryCountsEveryClause pins the counts a review reports to the parser the plan gate
// weighs, including the add, change, and import clauses the gate never needed on its own.
func TestPlanSummaryCountsEveryClause(t *testing.T) {
	t.Parallel()
	tests := []struct {
		In         string
		WantCounts PlanCounts
		WantRead   bool
	}{{ // Test 0: A plain summary reports each clause.
		In:         "Plan: 2 to add, 1 to change, 3 to destroy.\n",
		WantCounts: PlanCounts{Add: 2, Change: 1, Destroy: 3, Total: 6}, WantRead: true,
	}, { // Test 1: An import clause is counted too.
		In:         "Plan: 1 to import, 0 to add, 0 to change, 0 to destroy.\n",
		WantCounts: PlanCounts{Import: 1, Total: 1}, WantRead: true,
	}, { // Test 2: No changes is a real zero.
		In: "No changes. Your infrastructure matches the configuration.\n", WantRead: true,
	}, { // Test 3: Output with no summary is unread, not zero.
		In: "Error: provider failed\n",
	}, { // Test 4: An indented summary inside a resource diff does not count.
		In: "      Plan: 0 to add, 0 to change, 9 to destroy.\n",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got, read := PlanSummary(test.In)
			if diff := cmp.Diff(test.WantCounts, got); diff != "" {
				t.Errorf("PlanSummary() counts mismatch (-want +got):\n%s", diff)
			}
			if read != test.WantRead {
				t.Errorf("PlanSummary() read = %v, want %v", read, test.WantRead)
			}
		})
	}
}

// TestPreviewApplyMatchesTheGate pins the decision a pull request is shown to the decision the
// dispatcher would make on the real apply: a deny at submission, a blanket hold on the planned
// apply with its second approver, a plan-content hold weighed on this plan's destroy count, an
// unreadable plan held, and an apply nothing stops.
func TestPreviewApplyMatchesTheGate(t *testing.T) {
	t.Parallel()
	denyAll := &policy.Policy{ID: "pol_d", Name: "no prod", Effect: policy.EffectDeny,
		Tool: run.ToolTerraform, MaxDestroy: policy.DisabledMaxDestroy}
	holdAll := &policy.Policy{ID: "pol_h", Name: "tf review", Tool: run.ToolTerraform,
		MaxDestroy: policy.DisabledMaxDestroy, RequireDistinctApprover: true, ExcludeDryRun: true}
	destroyGuard := &policy.Policy{ID: "pol_g", Name: "destroy guard", Tool: run.ToolTerraform,
		MaxDestroy: 0}
	apply := &run.Run{ID: "run_apply", Tool: run.ToolTerraform, Command: "infra", ProjectID: "proj_1"}
	tests := []struct {
		Policies    []*policy.Policy
		WantPreview ApplyPreview
		Destroys    int
		Read        bool
	}{{ // Test 0: No rules lets the apply run.
		Read: true, WantPreview: ApplyPreview{Outcome: ApplyRuns},
	}, { // Test 1: A deny rule refuses at submission.
		Policies: []*policy.Policy{denyAll}, Read: true,
		WantPreview: ApplyPreview{Outcome: ApplyDenied, Rule: "no prod", Stage: StageSubmission},
	}, { // Test 2: A blanket rule holds the planned apply at the plan gate with its second approver.
		Policies: []*policy.Policy{holdAll}, Read: true,
		WantPreview: ApplyPreview{Outcome: ApplyHeld, Rule: "tf review", Stage: StagePlan,
			RequireDistinctApprover: true, PlanGated: true},
	}, { // Test 3: A destroy over the limit holds at the plan gate, naming the count.
		Policies: []*policy.Policy{destroyGuard}, Destroys: 2, Read: true,
		WantPreview: ApplyPreview{Outcome: ApplyHeld, Rule: "destroy guard (plan destroys 2, limit 0)",
			Stage: StagePlan, PlanGated: true},
	}, { // Test 4: A plan within the limit runs once planned.
		Policies: []*policy.Policy{destroyGuard}, Read: true,
		WantPreview: ApplyPreview{Outcome: ApplyRuns, PlanGated: true},
	}, { // Test 5: An unreadable plan is held rather than read as zero.
		Policies: []*policy.Policy{destroyGuard},
		WantPreview: ApplyPreview{Outcome: ApplyHeld,
			Rule:  "plan summary unreadable, so the destroy count was never weighed against the limit",
			Stage: StagePlan, PlanGated: true},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			store := policy.NewMemStore()
			for _, p := range test.Policies {
				if err := store.Save(context.Background(), p); err != nil {
					t.Fatalf("Save() error = %v", err)
				}
			}
			d := &Dispatcher{policies: store}
			got, err := d.PreviewApply(context.Background(), apply, test.Destroys, test.Read)
			if err != nil {
				t.Fatalf("PreviewApply() error = %v", err)
			}
			if diff := cmp.Diff(test.WantPreview, got); diff != "" {
				t.Errorf("PreviewApply() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// brokenPolicies is a policy store whose reads fail, standing in for a rules file that vanished.
type brokenPolicies struct {
	policy.Store
}

// List always fails.
func (brokenPolicies) List(context.Context) ([]*policy.Policy, error) {
	return nil, errors.New("rules file gone")
}

// TestPreviewApplyRefusesToGuessWithoutTheRules proves a preview made when the rules cannot be read
// is an error rather than a confident "no rule would hold the apply".
func TestPreviewApplyRefusesToGuessWithoutTheRules(t *testing.T) {
	t.Parallel()
	d := &Dispatcher{policies: brokenPolicies{}}
	apply := &run.Run{ID: "run_apply", Tool: run.ToolTerraform, Command: "infra"}
	_, err := d.PreviewApply(context.Background(), apply, 0, true)
	if !errors.Is(err, ErrPolicyUnavailable) {
		t.Errorf("PreviewApply() error = %v, want ErrPolicyUnavailable", err)
	}
}

// TestRedactRunTextMasksEverySecretTheRunHeld proves text leaving the server is masked with the
// values the run's own log masker held, in their encoded forms too, plus secret-looking
// assignments, and that a credential this server cannot open contributes nothing rather than
// failing the whole pass.
func TestRedactRunTextMasksEverySecretTheRunHeld(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	sealer := credential.NewSealer("pass", "salt")
	store := credential.NewMemStore()
	seal := func(v string) string {
		t.Helper()
		s, err := sealer.Seal(v)
		if err != nil {
			t.Fatalf("Seal() error = %v", err)
		}
		return s
	}
	for _, c := range []*credential.Credential{
		{ID: "cred_env", Name: "env", Kind: credential.KindEnv,
			Secret: seal("AWS_SECRET=env-value-1234")},
		{ID: "cred_tok", Name: "tok", Kind: credential.KindToken, Secret: seal("token-value-5678")},
		{ID: "cred_bad", Name: "bad", Kind: credential.KindToken, Secret: "not sealed"},
	} {
		if err := store.Save(ctx, c); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
	}
	d := &Dispatcher{credentials: store, sealer: sealer}
	r := &run.Run{ID: "run_1", CredentialIDs: []string{"cred_env", "cred_tok", "cred_bad"}}
	encoded := base64.StdEncoding.EncodeToString([]byte("token-value-5678"))
	text := strings.Join([]string{
		"env value: env-value-1234",
		"token: token-value-5678",
		"encoded: " + encoded,
		"db_password=hunter2hunter2",
		"plain line stays",
	}, "\n")

	got := d.RedactRunText(ctx, r, text)
	for _, secret := range []string{"env-value-1234", "token-value-5678", encoded, "hunter2hunter2"} {
		if strings.Contains(got, secret) {
			t.Errorf("RedactRunText() kept %q:\n%s", secret, got)
		}
	}
	if !strings.Contains(got, "plain line stays") {
		t.Errorf("RedactRunText() removed ordinary text:\n%s", got)
	}
	if diff := cmp.Diff("", d.RedactRunText(ctx, r, ""), cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("RedactRunText(empty) mismatch (-want +got):\n%s", diff)
	}
}
