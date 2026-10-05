package run

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

// Scans the cases below record, one per shape a gate's read can take.
var (
	// cleanPlan is a Terraform scan that read everything and found nothing.
	cleanPlan = DryRunScan{Tool: ToolTerraform, Scanner: "terraform-external", Version: 1,
		Inputs: []string{"main.tf"}}
	// programPlan is a Terraform scan that found an external data source.
	programPlan = DryRunScan{Tool: ToolTerraform, Scanner: "terraform-external", Version: 1,
		Inputs:   []string{"main.tf"},
		Findings: []string{"data.external.lookup runs a program during plan (main.tf line 1)"}}
	// unreadPlan is an OpenTofu scan that found nothing but could not read a module.
	unreadPlan = DryRunScan{Tool: ToolOpenTofu, Scanner: "terraform-external", Version: 1,
		Unread: []string{`module.vpc from "acme/vpc/aws" (not downloaded here)`}}
	// forcedPlaybook is an Ansible scan that found a task forcing real work.
	forcedPlaybook = DryRunScan{Tool: ToolAnsible, Scanner: CheckModeScanner, Version: 1,
		Findings: []string{`site.yml: task "Restart" sets check_mode to false`}}
	// unreadPlaybook is an Ansible scan that found nothing but could not read a role.
	unreadPlaybook = DryRunScan{Tool: ToolAnsible, Scanner: CheckModeScanner, Version: 1,
		Unread: []string{`role "db" (not in the project)`}}
)

// TestADryRunIsChangeFreeOnlyWhenEveryScanIs covers the one judgment every rule, grade, and view
// shares. A dry run changes nothing only when every scan the gate made of it read in full and
// found nothing, whichever tool it is, and a real run is never change free.
func TestADryRunIsChangeFreeOnlyWhenEveryScanIs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Run            *Run
		WantChangeFree bool
		WantFindings   []string
		WantSummary    string
	}{{ // Test 0: A dry run nothing scanned keeps the meaning its flag gives it.
		Run: &Run{Tool: ToolBash, DryRun: true}, WantChangeFree: true,
	}, { // Test 1: A plan whose scan read everything and found nothing is change free.
		Run:            &Run{Tool: ToolTerraform, DryRun: true, DryRunScans: []DryRunScan{cleanPlan}},
		WantChangeFree: true,
	}, { // Test 2: A plan that runs a program is not, and says what runs it.
		Run:          &Run{Tool: ToolTerraform, DryRun: true, DryRunScans: []DryRunScan{programPlan}},
		WantFindings: programPlan.Findings,
		WantSummary: "plan that may run a program while it plans: data.external.lookup runs a " +
			"program during plan (main.tf line 1)",
	}, { // Test 3: A plan the scan could not read in full is not either, and says why.
		Run: &Run{Tool: ToolOpenTofu, DryRun: true, DryRunScans: []DryRunScan{unreadPlan}},
		WantFindings: []string{`could not read module.vpc from "acme/vpc/aws" (not downloaded ` +
			`here), so it may run a program during plan`},
		WantSummary: `plan that may run a program while it plans: could not read module.vpc from ` +
			`"acme/vpc/aws" (not downloaded here), so it may run a program during plan`,
	}, { // Test 4: An Ansible dry run is worded for check mode.
		Run: &Run{DryRun: true, DryRunScans: []DryRunScan{forcedPlaybook, unreadPlaybook}},
		WantFindings: []string{`site.yml: task "Restart" sets check_mode to false`,
			`could not read role "db" (not in the project), so it may set check_mode to false`},
		WantSummary: `dry run that still runs work for real under check mode: site.yml: task ` +
			`"Restart" sets check_mode to false, and 1 more`,
	}, { // Test 5: A pipeline's scans name the step each belongs to.
		Run: &Run{Kind: KindPipeline, DryRun: true, DryRunScans: append(
			ScansForStep("check", []DryRunScan{cleanPlan}),
			ScansForStep("plan", []DryRunScan{programPlan})...)},
		WantFindings: []string{`step "plan": data.external.lookup runs a program during plan ` +
			`(main.tf line 1)`},
		WantSummary: `plan that may run a program while it plans: step "plan": ` +
			`data.external.lookup runs a program during plan (main.tf line 1)`,
	}, { // Test 6: A real run is never change free, whatever was scanned.
		Run: &Run{Tool: ToolTerraform, DryRunScans: []DryRunScan{cleanPlan}},
	}, { // Test 7: Nil is no run at all.
		Run: nil,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := test.Run.ChangeFree(); got != test.WantChangeFree {
				t.Errorf("ChangeFree() = %v, want %v", got, test.WantChangeFree)
			}
			if diff := cmp.Diff(test.WantFindings, test.Run.DryRunFindings(),
				cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("DryRunFindings() mismatch (-want +got):\n%s", diff)
			}
			if got := test.Run.NotChangeFreeSummary(); got != test.WantSummary {
				t.Errorf("NotChangeFreeSummary() = %q, want %q", got, test.WantSummary)
			}
		})
	}
}

// TestAScanIsJudgedByWhatItHolds pins that a record's classification is derived from what it found
// and could not read, never trusted on its label alone. A label saying change_free over a finding
// must not exempt the run.
func TestAScanIsJudgedByWhatItHolds(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Scan           DryRunScan
		WantClass      string
		WantChangeFree bool
	}{{ // Test 0: Nothing found, nothing unread.
		Scan: cleanPlan, WantClass: DryRunChangeFree, WantChangeFree: true,
	}, { // Test 1: Something found.
		Scan: programPlan, WantClass: DryRunNotChangeFree,
	}, { // Test 2: Nothing found, something unread.
		Scan: unreadPlan, WantClass: DryRunIncomplete,
	}, { // Test 3: A finding under a label that says otherwise.
		Scan:      DryRunScan{Findings: []string{"x"}, Classification: DryRunChangeFree},
		WantClass: DryRunNotChangeFree,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := test.Scan.ChangeFree(); got != test.WantChangeFree {
				t.Errorf("ChangeFree() = %v, want %v", got, test.WantChangeFree)
			}
			if got := test.Scan.Classified().Classification; got != test.WantClass {
				t.Errorf("Classified() = %q, want %q", got, test.WantClass)
			}
		})
	}
}

// TestAStepTakesBackItsOwnScans covers the pipeline record: the pipeline holds every step's scans
// named by step, and a step run carries exactly its own, unnamed, so it reads like any other run.
func TestAStepTakesBackItsOwnScans(t *testing.T) {
	t.Parallel()
	parent := &Run{DryRunScans: append(ScansForStep("check", []DryRunScan{forcedPlaybook}),
		ScansForStep("plan", []DryRunScan{programPlan, unreadPlan})...)}
	got := parent.StepDryRunScans("plan")
	if diff := cmp.Diff([]DryRunScan{programPlan, unreadPlan}, got); diff != "" {
		t.Errorf("StepDryRunScans(plan) mismatch (-want +got):\n%s", diff)
	}
	if other := parent.StepDryRunScans("absent"); len(other) != 0 {
		t.Errorf("StepDryRunScans(absent) = %v, want none", other)
	}
	got[0].Findings[0] = "changed"
	if parent.DryRunScans[1].Findings[0] == "changed" {
		t.Error("a step's scans share memory with the pipeline's record")
	}
}

// TestScansSurviveTheirColumn pins the encoding both stores use, and that a column that does not
// decode is reported rather than read as no scan, which would turn a held dry run into a preview.
func TestScansSurviveTheirColumn(t *testing.T) {
	t.Parallel()
	fetched := cleanPlan
	fetched.Fetch = &ModuleFetch{Command: "terraform get", ExitStatus: -1, Error: "stopped"}
	scans := []DryRunScan{programPlan.Classified(), ScansForStep("a, b", []DryRunScan{unreadPlan})[0],
		fetched.Classified()}
	got, err := ParseScansColumn(ScansColumn(scans))
	if err != nil {
		t.Fatalf("ParseScansColumn() error = %v", err)
	}
	if diff := cmp.Diff(scans, got); diff != "" {
		t.Errorf("round trip mismatch (-want +got):\n%s", diff)
	}
	if ScansColumn(nil) != "" {
		t.Error("ScansColumn(nil) is not empty, so a run with no scans would store one")
	}
	if none, err := ParseScansColumn(""); err != nil || none != nil {
		t.Errorf("ParseScansColumn(\"\") = %v, %v, want nothing", none, err)
	}
	if _, err := ParseScansColumn("[{"); err == nil {
		t.Error("ParseScansColumn() read a corrupted column as no scan")
	}
}

// TestAModuleFetchSaysHowItWent covers the record of the module download the gate runs before it
// reads a plan's configuration. It completed only with exit status 0 and no error, every view words
// it with the same phrase and the exit status, and a copy of a scan holds its own record, so a
// reader handed a stored run's scans cannot rewrite the evidence through them.
func TestAModuleFetchSaysHowItWent(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Fetch is the download's record.
		Fetch *ModuleFetch
		// WantFetched is whether the download completed.
		WantFetched bool
		// WantSummary is the phrase the views show.
		WantSummary string
	}{{ // Test 0: No download ran.
		Fetch: nil, WantFetched: false, WantSummary: "",
	}, { // Test 1: A download that completed.
		Fetch: &ModuleFetch{Command: "terraform get"}, WantFetched: true,
		WantSummary: "modules downloaded first by the gate's terraform get (exit status 0)",
	}, { // Test 2: A download that failed.
		Fetch:       &ModuleFetch{Command: "tofu get", ExitStatus: 1, Error: "Module not found"},
		WantSummary: "the gate's tofu get did not download its modules (exit status 1)",
	}, { // Test 3: A download stopped at a bound.
		Fetch:       &ModuleFetch{Command: "terraform get", ExitStatus: -1, Error: "too large"},
		WantSummary: "the gate's terraform get did not download its modules (exit status -1)",
	}, { // Test 4: An error with exit status 0 is not a completed download.
		Fetch:       &ModuleFetch{Command: "terraform get", Error: "the copy could not be made"},
		WantSummary: "the gate's terraform get did not download its modules (exit status 0)",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := test.Fetch.Fetched(); got != test.WantFetched {
				t.Errorf("Fetched() = %v, want %v", got, test.WantFetched)
			}
			if diff := cmp.Diff(test.WantSummary, test.Fetch.Summary()); diff != "" {
				t.Errorf("Summary() mismatch (-want +got):\n%s", diff)
			}
		})
	}
	orig := DryRunScan{Tool: ToolTerraform, Fetch: &ModuleFetch{Command: "terraform get"}}
	copied := orig.Clone()
	copied.Fetch.ExitStatus, copied.Fetch.Error = 9, "rewritten"
	if !orig.Fetch.Fetched() {
		t.Errorf("writing through a copy rewrote the original's record: %+v", orig.Fetch)
	}
}

// TestTheHoldNoteNamesTheTwoCleanFixes covers the message a dry run held only for its scan carries.
// It names what was found, or what could not be read, and the two fixes that are clean: change the
// input so it is safe, or drop the dry-run exemption from the rule. A Rego rule has no
// exclude_dry_run, so its second fix is worded for its module.
func TestTheHoldNoteNamesTheTwoCleanFixes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Scan     DryRunScan
		Rego     bool
		WantText []string
	}{{ // Test 0: An Ansible task that forces real work.
		Scan: forcedPlaybook,
		WantText: []string{`task "Restart" sets check_mode to false`,
			"rework the task so check mode is safe", `drop exclude_dry_run from "prod"`},
	}, { // Test 1: A role the gate could not read.
		Scan: unreadPlaybook,
		WantText: []string{`could not read role "db"`, "committed to the project or installed",
			`drop exclude_dry_run from "prod"`},
	}, { // Test 2: An external data source, named by its address.
		Scan: programPlan,
		WantText: []string{"data.external.lookup runs a program during plan (main.tf line 1)",
			"replace the external data source", "while Terraform plans",
			`drop exclude_dry_run from "prod"`},
	}, { // Test 3: A module the gate could not read.
		Scan: unreadPlan,
		WantText: []string{`could not read module.vpc from "acme/vpc/aws"`,
			"downloads registry and remote modules with the run's own credentials",
			`drop exclude_dry_run from "prod"`},
	}, { // Test 4: A Rego rule's second fix is in its module.
		Scan: forcedPlaybook, Rego: true,
		WantText: []string{`stop exempting dry runs in the module of "prod"`},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			r := &Run{DryRun: true, Tool: test.Scan.Tool, DryRunScans: []DryRunScan{test.Scan}}
			note := ExemptionHoldNote(r, "prod", test.Rego)
			for _, want := range test.WantText {
				if !strings.Contains(note, want) {
					t.Errorf("hold note %q does not say %q", note, want)
				}
			}
			if !strings.Contains(note, "Two clean fixes") {
				t.Errorf("hold note %q does not name the two clean fixes", note)
			}
		})
	}
	if note := ExemptionHoldNote(&Run{DryRun: true}, "prod", false); note != "" {
		t.Errorf("a dry run with nothing recorded carries a hold note: %q", note)
	}
}

// TestAPlanNotChangeFreeIsGradedAsTheApplyItMayBe covers the grades for a Terraform plan. A plan
// that changes nothing is low risk with nothing to undo. One that runs a program is graded as the
// real run it may be, and both grades say why.
func TestAPlanNotChangeFreeIsGradedAsTheApplyItMayBe(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Scans     []DryRunScan
		WantLevel string
		WantClass string
	}{{ // Test 0: A change-free plan.
		Scans: []DryRunScan{cleanPlan}, WantLevel: RiskLow, WantClass: Reversible,
	}, { // Test 1: A plan that runs a program.
		Scans: []DryRunScan{programPlan}, WantLevel: RiskMedium, WantClass: ReversibleCostly,
	}, { // Test 2: A plan the scan could not read in full.
		Scans: []DryRunScan{unreadPlan}, WantLevel: RiskMedium, WantClass: ReversibleCostly,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			r := &Run{Tool: ToolTerraform, Command: "infra", DryRun: true, DryRunScans: test.Scans}
			risk, undo := AssessRisk(r), AssessReversibility(r)
			if risk.Level != test.WantLevel || undo.Class != test.WantClass {
				t.Errorf("graded %s and %s, want %s and %s: %v %v", risk.Level, undo.Class,
					test.WantLevel, test.WantClass, risk.Reasons, undo.Reasons)
			}
			if r.ChangeFree() {
				return
			}
			summary := r.NotChangeFreeSummary()
			if !slices.Contains(risk.Reasons, summary) || !slices.Contains(undo.Reasons, summary) {
				t.Errorf("the grades do not say why the plan is not change free: %v %v",
					risk.Reasons, undo.Reasons)
			}
		})
	}
}

// TestAnUnreadPlaybookIsIncomplete covers the Ansible dry run whose playbook could not be opened at
// all, which records why rather than nothing.
func TestAnUnreadPlaybookIsIncomplete(t *testing.T) {
	t.Parallel()
	scan := UnreadPlaybookScan("site.yml", "not in the project")
	if scan.ChangeFree() || scan.Classification != DryRunIncomplete {
		t.Fatalf("an unread playbook classified %q, want incomplete", scan.Classification)
	}
	want := []string{`could not read playbook "site.yml" (not in the project), so it may set ` +
		`check_mode to false`}
	if diff := cmp.Diff(want, scan.Entries()); diff != "" {
		t.Errorf("Entries() mismatch (-want +got):\n%s", diff)
	}
}
