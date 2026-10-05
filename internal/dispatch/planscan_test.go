package dispatch

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/project"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/tfscan"
)

// Configurations the plan gate tests submit.
const (
	// cleanConfig declares nothing that runs a program while it plans.
	cleanConfig = "resource \"terraform_data\" \"x\" {\n  input = \"y\"\n}\n"
	// externalConfig declares one external data source, which runs its program while it plans.
	externalConfig = "data \"external\" \"lookup\" {\n  program = [\"sh\", \"-c\", \"echo {}\"]\n}\n"
)

// rulesHolding returns a policy store holding the given rules.
func rulesHolding(t *testing.T, rules ...*policy.Policy) policy.Store {
	t.Helper()
	store := policy.NewMemStore()
	for _, p := range rules {
		if err := store.Save(context.Background(), p); err != nil {
			t.Fatalf("Save(policy) error = %v", err)
		}
	}
	return store
}

// excludePlans is the rule a team writes to let plans through and hold everything else.
func excludePlans(tool string) *policy.Policy {
	return &policy.Policy{
		ID: "pol_plans", Name: "prod infra", Tool: tool, ExcludeDryRun: true,
		Effect: policy.EffectRequireApproval, MaxDestroy: policy.DisabledMaxDestroy,
	}
}

// TestAPlanThatRunsAProgramIsHeld drives the bypass through the dispatcher, the path every
// submission takes, for Terraform and OpenTofu.
//
// A rule that excluded dry runs exempted every plan, and a plan runs the program each external data
// source names with the run's credentials. So a plan of such a configuration ran code while the
// rule waved it through as a preview. The gate now reads the configuration first, modules included,
// and fails closed on anything it cannot read. Each case is beside the clean plan the rule must
// still exempt.
//
//nolint:funlen // Test function.
func TestAPlanThatRunsAProgramIsHeld(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Tool       string
		Files      map[string]string
		Dir        string
		WantHeld   bool
		WantClass  string
		WantEntry  string
		WantFixTxt string
	}{{ // Test 0: A clean plan is still exempt.
		Tool: run.ToolTerraform, Files: map[string]string{"main.tf": cleanConfig},
		WantClass: run.DryRunChangeFree,
	}, { // Test 1: An external data source in the root module holds the plan, named by address.
		Tool: run.ToolTerraform, Files: map[string]string{"main.tf": externalConfig},
		WantHeld: true, WantClass: run.DryRunNotChangeFree,
		WantEntry:  "data.external.lookup runs a program during plan (main.tf line 1)",
		WantFixTxt: "replace the external data source with one that runs no program",
	}, { // Test 2: One behind a module is named by the module's address.
		Tool: run.ToolTerraform, Files: map[string]string{
			"main.tf":             "module \"net\" {\n  source = \"./modules/net\"\n}\n",
			"modules/net/main.tf": externalConfig,
		},
		WantHeld: true, WantClass: run.DryRunNotChangeFree,
		WantEntry:  "module.net.data.external.lookup runs a program during plan",
		WantFixTxt: "replace the external data source",
	}, { // Test 3: A registry module nobody downloaded leaves the plan unclassified.
		Tool: run.ToolTerraform, Files: map[string]string{"main.tf": "module \"vpc\" {\n" +
			"  source = \"terraform-aws-modules/vpc/aws\"\n}\n"},
		WantHeld: true, WantClass: run.DryRunIncomplete,
		WantEntry:  `could not read module.vpc from "terraform-aws-modules/vpc/aws"`,
		WantFixTxt: "downloads registry and remote modules with the run's own credentials",
	}, { // Test 4: OpenTofu's own files are read too.
		Tool: run.ToolOpenTofu, Files: map[string]string{"main.tofu": externalConfig},
		WantHeld: true, WantClass: run.DryRunNotChangeFree,
		WantEntry:  "data.external.lookup runs a program during plan (main.tofu line 1)",
		WantFixTxt: "while OpenTofu plans",
	}, { // Test 5: A configuration that does not parse leaves the plan unclassified.
		Tool: run.ToolTerraform, Files: map[string]string{"main.tf": "resource \"x\" \"y\" {\n"},
		WantHeld: true, WantClass: run.DryRunIncomplete,
		WantEntry:  `could not read file "main.tf" (it could not be parsed`,
		WantFixTxt: "drop exclude_dry_run",
	}, { // Test 6: A working directory that is not there is unread, never change free.
		Tool: run.ToolTerraform, Files: map[string]string{"main.tf": cleanConfig}, Dir: "absent",
		WantHeld: true, WantClass: run.DryRunIncomplete,
		WantEntry:  "(not on this server)",
		WantFixTxt: "drop exclude_dry_run",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			dir := t.TempDir()
			writeFiles(t, dir, test.Files)
			store := run.NewMemStore()
			d := New(store, okRunner(), zap.NewNop(),
				WithPolicies(rulesHolding(t, excludePlans(test.Tool))), WithNoJanitor())
			t.Cleanup(d.Close)
			workdir := dir
			if test.Dir != "" {
				workdir = filepath.Join(dir, test.Dir)
			}
			got, err := d.Submit(ctx, "", "", run.WithTool(test.Tool), run.WithCommand(workdir),
				run.WithDryRun(true))
			if err != nil {
				t.Fatalf("Submit() error = %v", err)
			}
			if held := got.Status == run.StatusPendingApproval; held != test.WantHeld {
				t.Fatalf("held = %v, want %v. Recorded: %q", held, test.WantHeld, got.DryRunFindings())
			}
			stored, err := store.Get(ctx, got.ID)
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			if !stored.DryRun {
				t.Error("the stored run lost its dry-run flag: the gate must change the judgment, " +
					"never what executes")
			}
			if len(stored.DryRunScans) != 1 {
				t.Fatalf("scans = %+v, want the gate's one scan recorded", stored.DryRunScans)
			}
			scan := stored.DryRunScans[0]
			if scan.Scanner != tfscan.Scanner || scan.Version != tfscan.Version ||
				scan.Tool != test.Tool || scan.Classification != test.WantClass {
				t.Errorf("scan = %s v%d for %s classified %q, want %s v%d for %s classified %q",
					scan.Scanner, scan.Version, scan.Tool, scan.Classification, tfscan.Scanner,
					tfscan.Version, test.Tool, test.WantClass)
			}
			if !test.WantHeld {
				if len(scan.Inputs) == 0 || stored.HoldNote != "" {
					t.Errorf("a clean plan recorded no inputs or a hold note: %+v %q", scan,
						stored.HoldNote)
				}
				return
			}
			if !strings.Contains(strings.Join(stored.DryRunFindings(), "\n"), test.WantEntry) {
				t.Errorf("recorded %q, want an entry naming %q", stored.DryRunFindings(), test.WantEntry)
			}
			for _, want := range []string{test.WantEntry, test.WantFixTxt, "Two clean fixes"} {
				if !strings.Contains(stored.HoldNote, want) {
					t.Errorf("hold note %q does not say %q", stored.HoldNote, want)
				}
			}
			if risk := run.AssessRisk(stored); risk.Level == run.RiskLow {
				t.Errorf("a plan that is not change free graded low risk: %v", risk.Reasons)
			}
			if undo := run.AssessReversibility(stored); undo.Class == run.Reversible {
				t.Errorf("a plan that is not change free graded with nothing to undo: %v", undo.Reasons)
			}
			if detail := heldDetail(stored); !strings.Contains(detail, stored.HoldNote) {
				t.Errorf("the held notification does not carry the hold note: %q", detail)
			}
		})
	}
}

// TestARuleHoldingEveryPlanNamesNoFix covers the hold the scan did not cause. A rule that holds
// every plan holds one that runs a program too, and naming fixes for an exemption the rule never
// offered would send the operator after the wrong thing. The approver still reads what the plan
// runs.
func TestARuleHoldingEveryPlanNamesNoFix(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"main.tf": externalConfig})
	every := excludePlans(run.ToolTerraform)
	every.ExcludeDryRun = false
	d := New(run.NewMemStore(), okRunner(), zap.NewNop(), WithPolicies(rulesHolding(t, every)),
		WithNoJanitor())
	t.Cleanup(d.Close)
	got, err := d.Submit(ctx, "", "", run.WithTool(run.ToolTerraform), run.WithCommand(dir),
		run.WithDryRun(true))
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	if got.Status != run.StatusPendingApproval || got.HoldNote != "" {
		t.Fatalf("status = %q, hold note %q, want held with no note", got.Status, got.HoldNote)
	}
	if detail := heldDetail(got); !strings.Contains(detail, "data.external.lookup") {
		t.Errorf("the held notification does not say what the plan runs: %q", detail)
	}
}

// TestARegoExemptionHoldsAPlanThatRunsAProgram covers the same exemption written in Rego, the way
// a porter writes an exclude_dry_run rule: exempt input.run.dry_run. The plan is judged as the real
// run it may be, so the module holds it, and the note's second fix is worded for a module.
func TestARegoExemptionHoldsAPlanThatRunsAProgram(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rules := regoStore(t, `hold contains "infrastructure waits for a person" if {
	input.run.tool == "terraform"
	not input.run.dry_run
}`)
	tests := []struct {
		Config   string
		WantHeld bool
	}{{ // Test 0: A clean plan passes.
		Config: cleanConfig,
	}, { // Test 1: A plan that runs a program is held.
		Config: externalConfig, WantHeld: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			writeFiles(t, dir, map[string]string{"main.tf": test.Config})
			d := New(run.NewMemStore(), okRunner(), zap.NewNop(), WithPolicies(rules),
				WithNoJanitor())
			t.Cleanup(d.Close)
			got, err := d.Submit(ctx, "", "", run.WithTool(run.ToolTerraform), run.WithCommand(dir),
				run.WithDryRun(true))
			if err != nil {
				t.Fatalf("Submit() error = %v", err)
			}
			if held := got.Status == run.StatusPendingApproval; held != test.WantHeld {
				t.Fatalf("held = %v, want %v", held, test.WantHeld)
			}
			if test.WantHeld && !strings.Contains(got.HoldNote, "stop exempting dry runs in the "+
				`module of "rego gate"`) {
				t.Errorf("hold note %q does not name the module's fix", got.HoldNote)
			}
		})
	}
}

// TestAProjectPlanIsReadAtTheCommitItRuns covers a plan drawn from a project, which is how a
// template, a schedule, a webhook, or an agent proposes one. The gate fetches the project and reads
// the configuration at the commit about to run, so an external data source pushed a moment before
// the launch is seen.
func TestAProjectPlanIsReadAtTheCommitItRuns(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := newTreeRepo(t, map[string]string{"infra/main.tf": cleanConfig})
	syncer, err := project.NewSyncer(t.TempDir())
	if err != nil {
		t.Fatalf("NewSyncer() error = %v", err)
	}
	projects := project.NewMemStore()
	p := &project.Project{ID: "proj_plan", Name: "infra", RepoURL: repo, Branch: "main"}
	if err := projects.Save(ctx, p); err != nil {
		t.Fatalf("Save(project) error = %v", err)
	}
	d := New(run.NewMemStore(), okRunner(), zap.NewNop(), WithProjects(projects, syncer),
		WithPolicies(rulesHolding(t, excludePlans(run.ToolTerraform))), WithNoJanitor())
	t.Cleanup(d.Close)
	submit := func() *run.Run {
		t.Helper()
		r, err := d.Submit(ctx, "", "", run.WithTool(run.ToolTerraform), run.WithCommand("infra"),
			run.WithProject(p.ID), run.WithDryRun(true))
		if err != nil {
			t.Fatalf("Submit() error = %v", err)
		}
		return r
	}
	wt, err := syncer.Sync(p, "")
	if err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	wt.Cleanup()
	if clean := submit(); clean.Status == run.StatusPendingApproval {
		t.Fatalf("a clean project plan was held: %q", clean.DryRunFindings())
	}
	commitTree(t, repo, map[string]string{"infra/lookup.tf": externalConfig})
	held := submit()
	if held.Status != run.StatusPendingApproval {
		t.Fatalf("a project plan running a program, pushed before the launch, was exempt")
	}
	scan := held.DryRunScans[0]
	if !strings.Contains(scan.Source, "read at commit ") ||
		!slices.Contains(scan.Inputs, "infra/lookup.tf") {
		t.Errorf("scan = %+v, want the commit read and the file pushed named", scan)
	}
	if held.PinnedCommit == "" {
		t.Error("the held plan is not pinned, so approval could release a commit nobody scanned")
	}
}

// TestAPipelinePlanStepThatRunsAProgramHoldsThePipeline covers the workflow door. A pipeline whose
// plan step runs a program is held as a whole, and the record names the step.
func TestAPipelinePlanStepThatRunsAProgramHoldsThePipeline(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"main.tf": externalConfig})
	d := New(run.NewMemStore(), okRunner(), zap.NewNop(),
		WithPolicies(rulesHolding(t, excludePlans(run.ToolTerraform))), WithNoJanitor())
	t.Cleanup(d.Close)
	got, err := d.SubmitPipeline(ctx, "nightly", "", []run.PipelineStep{{
		Name: "plan", Tool: run.ToolTerraform, Command: dir, DryRun: true,
	}}, run.WithDryRun(true))
	if err != nil {
		t.Fatalf("SubmitPipeline() error = %v", err)
	}
	if got.Status != run.StatusPendingApproval {
		t.Fatalf("status = %q, want the pipeline held for its plan step", got.Status)
	}
	want := `step "plan": data.external.lookup runs a program during plan (main.tf line 1)`
	if findings := got.DryRunFindings(); !slices.Contains(findings, want) {
		t.Errorf("findings = %q, want %q", findings, want)
	}
	if !strings.Contains(got.HoldNote, want) {
		t.Errorf("hold note %q does not name the step", got.HoldNote)
	}
	if step := stepRun(got, got.Steps[0], 0, 0, nil); len(step.DryRunScans) != 1 ||
		step.DryRunScans[0].Step != "" || step.ChangeFree() {
		t.Errorf("the step run's scans = %+v, want its own share of the pipeline's record",
			step.DryRunScans)
	}
}

// TestThePlanGateStillPlansAnApplyThatRunsAProgram pins where the scan stops. An apply a
// plan-content rule scopes was asked for as a real run, and the rule lets an apply within its limit
// run without waiting, programs and all. The plan the gate runs ahead of it is therefore not held
// for what the scan would find: holding it would stop nothing the apply does not do anyway, and it
// would hold every apply calling a registry module, which a checkout never holds. The apply is
// planned, and its proposal faces the rule as before.
func TestThePlanGateStillPlansAnApplyThatRunsAProgram(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"main.tf": externalConfig})
	store := run.NewMemStore()
	runner := &specRecorder{}
	limit := &policy.Policy{ID: "pol_limit", Name: "prod applies", Tool: run.ToolTerraform,
		Effect: policy.EffectRequireApproval, MaxDestroy: 5}
	d := New(store, runner, zap.NewNop(), WithPolicies(rulesHolding(t, limit)), WithNoJanitor())
	t.Cleanup(d.Close)
	got, err := d.Submit(ctx, "", "", run.WithTool(run.ToolTerraform), run.WithCommand(dir))
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	if got.Status == run.StatusPendingApproval || got.HoldNote != "" || len(got.DryRunScans) != 0 {
		t.Fatalf("status %q, note %q, scans %v: the apply was held or scanned before its plan",
			got.Status, got.HoldNote, got.DryRunScans)
	}
	waitTerminal(t, store, got.ID)
	specs := runner.executed()
	if len(specs) == 0 || !specs[0].DryRun {
		t.Errorf("executed %+v, want the plan gate to plan the apply first", specs)
	}
}
