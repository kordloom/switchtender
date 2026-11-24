package dispatch

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/go-cmp/cmp"
	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// newPublishedEnvDispatcher returns a dispatcher holding an env credential that sets TF_VAR_api_url
// and a workflow parent approved with region and the secret answer token.
func newPublishedEnvDispatcher(t *testing.T) *Dispatcher {
	t.Helper()
	ctx := context.Background()
	sealer := credential.NewSealer("pass", "salt")
	sealed, err := sealer.Seal("TF_VAR_api_url=https://real.example\nAWS_REGION=us-east-1")
	if err != nil {
		t.Fatalf("Seal() error = %v", err)
	}
	creds := credential.NewMemStore()
	if err := creds.Save(ctx, &credential.Credential{ID: "cred_env", Name: "cloud",
		Kind: credential.KindEnv, Secret: sealed}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	runs := run.NewMemStore()
	if err := runs.Save(ctx, &run.Run{ID: "run_parent", Tool: run.ToolBash,
		ExtraVars: map[string]any{"region": "us"}, SealedNames: []string{"token"}}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	return &Dispatcher{credentials: creds, sealer: sealer, store: runs,
		runFilesRoot: t.TempDir()}
}

// TestAPublishedVarCannotReplaceACredentialsEnvironment pins the execution check: a Terraform or
// OpenTofu step cannot take a published var whose TF_VAR_ entry replaces one its credentials set.
func TestAPublishedVarCannotReplaceACredentialsEnvironment(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Tool   string
		Parent string
		Vars   map[string]any
		Want   error
	}{{ // Test 0: A published api_url on a Terraform step is refused.
		Tool: run.ToolTerraform, Parent: "run_parent", Vars: map[string]any{"api_url": "https://evil"},
		Want: ErrPublishedEnv,
	}, { // Test 1: The same on OpenTofu.
		Tool: run.ToolOpenTofu, Parent: "run_parent", Vars: map[string]any{"api_url": "https://evil"},
		Want: ErrPublishedEnv,
	}, { // Test 2: A published name the environment does not hold runs.
		Tool: run.ToolTerraform, Parent: "run_parent", Vars: map[string]any{"other": "x"},
	}, { // Test 3: An approved var that is not in the environment runs beside a published one.
		Tool: run.ToolTerraform, Parent: "run_parent",
		Vars: map[string]any{"region": "us", "other": "x"},
	}, { // Test 4: A key that is not a Terraform identifier is refused.
		Tool: run.ToolTerraform, Parent: "run_parent",
		Vars: map[string]any{"api_url=https://evil": "x"}, Want: ErrPublishedEnv,
	}, { // Test 5: A parent that cannot be read makes every var published.
		Tool: run.ToolTerraform, Parent: "run_missing", Vars: map[string]any{"api_url": "https://x"},
		Want: ErrPublishedEnv,
	}, { // Test 6: An Ansible step is not judged by this rule.
		Tool: run.ToolAnsible, Parent: "run_parent", Vars: map[string]any{"api_url": "https://evil"},
	}, { // Test 7: A run that is not a step keeps the person's own value, as AWX does.
		Tool: run.ToolTerraform, Vars: map[string]any{"api_url": "https://mine"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			d := newPublishedEnvDispatcher(t)
			r := &run.Run{ID: "run_step", Tool: test.Tool, CredentialIDs: []string{"cred_env"},
				ExtraVars: test.Vars}
			if test.Parent != "" {
				r.ParentID = &test.Parent
			}
			spec := &roundhouse.Spec{ExtraVars: test.Vars}
			cleanup, _, err := d.materializeFrom(context.Background(), storeSource{d: d}, r, spec)
			defer cleanup()
			if !errors.Is(err, test.Want) {
				t.Fatalf("materializeFrom() error = %v, want %v", err, test.Want)
			}
		})
	}
}

// TestAnApprovedVarKeepsItsPlaceInTheEnvironment pins the parity case: a var the workflow itself
// was approved with is a choice a person saw and may set a name the credential also sets.
func TestAnApprovedVarKeepsItsPlaceInTheEnvironment(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := newPublishedEnvDispatcher(t)
	if err := d.store.Save(ctx, &run.Run{ID: "run_owner", Tool: run.ToolBash,
		ExtraVars: map[string]any{"api_url": "https://mine"}, SealedNames: []string{"token"}}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	parent := "run_owner"
	vars := map[string]any{"api_url": "https://mine", "token": "answer"}
	r := &run.Run{ID: "run_step", Tool: run.ToolTerraform, ParentID: &parent,
		CredentialIDs: []string{"cred_env"}, ExtraVars: vars}
	cleanup, _, err := d.materializeFrom(ctx, storeSource{d: d}, r, &roundhouse.Spec{ExtraVars: vars})
	defer cleanup()
	if err != nil {
		t.Fatalf("materializeFrom() error = %v, want the approved var to run", err)
	}
}

// TestADeliveredStepCarriesItsApprovedNames pins the relay path: the control node reads the parent
// at claim, and a worker refuses a published var by that list, or every var when it is missing.
func TestADeliveredStepCarriesItsApprovedNames(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := newPublishedEnvDispatcher(t)
	if err := d.store.Save(ctx, &run.Run{ID: "run_owner", Tool: run.ToolBash,
		ExtraVars: map[string]any{"api_url": "https://mine"}}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	step := func(parent string, vars map[string]any) *run.Run {
		return &run.Run{ID: "run_step", Tool: run.ToolTerraform, ParentID: &parent,
			CredentialIDs: []string{"cred_env"}, ExtraVars: vars}
	}
	p, _, err := d.OpenSecrets(ctx, step("run_parent", nil))
	if err != nil {
		t.Fatalf("OpenSecrets() error = %v", err)
	}
	if diff := cmp.Diff([]string{"region", "token"}, p.ApprovedVars); diff != "" || !p.VarsApproved {
		t.Fatalf("approved names (-want +got), filled %v:\n%s", p.VarsApproved, diff)
	}
	tests := []struct {
		Parent string
		Vars   map[string]any
		Strip  bool
		Want   error
	}{{ // Test 0: A published api_url is refused on the worker.
		Parent: "run_parent", Vars: map[string]any{"api_url": "https://evil"}, Want: ErrPublishedEnv,
	}, { // Test 1: A published name the environment does not hold runs on the worker.
		Parent: "run_parent", Vars: map[string]any{"other": "x"},
	}, { // Test 2: An api_url the workflow was approved with runs on the worker.
		Parent: "run_owner", Vars: map[string]any{"api_url": "https://mine"},
	}, { // Test 3: A payload without the names makes the approved one published.
		Parent: "run_owner", Vars: map[string]any{"api_url": "https://mine"}, Strip: true,
		Want: ErrPublishedEnv,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			r := step(test.Parent, test.Vars)
			opened, _, err := d.OpenSecrets(ctx, r)
			if err != nil {
				t.Fatalf("OpenSecrets() error = %v", err)
			}
			if test.Strip {
				opened.ApprovedVars, opened.VarsApproved = nil, false
			}
			held := newHeldDeliveries()
			held.payloads["run_step"] = opened
			worker := New(run.NewMemStore(), roundhouse.RunnerFunc(nil), zap.NewNop(), WithNoJanitor(),
				WithSecretDelivery(held), WithRunFilesRoot(t.TempDir()))
			t.Cleanup(worker.Close)
			cleanup, _, err := worker.materializeFrom(ctx, worker.secretsFor(r), r,
				&roundhouse.Spec{ExtraVars: test.Vars})
			defer cleanup()
			if !errors.Is(err, test.Want) {
				t.Fatalf("materializeFrom() error = %v, want %v", err, test.Want)
			}
		})
	}
}

// TestTerraformIdentifier pins which keys name a Terraform variable.
func TestTerraformIdentifier(t *testing.T) {
	t.Parallel()
	tests := []struct {
		In   string
		Want bool
	}{
		{In: "api_url", Want: true}, // Test 0: Plain name.
		{In: "_x-1", Want: true},    // Test 1: Leading underscore, hyphen, and digit.
		{In: "", Want: false},       // Test 2: Empty.
		{In: "1abc", Want: false},   // Test 3: Leading digit.
		{In: "a=b", Want: false},    // Test 4: An equals sign.
		{In: "a b", Want: false},    // Test 5: A space.
		{In: "-a", Want: false},     // Test 6: Leading hyphen.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := terraformIdentifier(test.In); got != test.Want {
				t.Errorf("terraformIdentifier(%q) = %v, want %v", test.In, got, test.Want)
			}
		})
	}
}
