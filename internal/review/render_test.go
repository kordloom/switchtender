package review

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/dispatch"
	"github.com/kordloom/switchtender/internal/run"
)

// TestRenderStatesTheResultAndTheApplyDecision pins what a finished plan's comment tells a
// reviewer: the marker first, the counts and the destroy count the rules weigh, the decision the
// apply would get with its rule and its second approver, the links, and the receipt.
func TestRenderStatesTheResultAndTheApplyDecision(t *testing.T) {
	t.Parallel()
	at := time.Unix(1700000000, 5)
	rep := Report{
		TemplateID: "tpl_net", TemplateName: "network", Phase: PhaseSucceeded, RunID: "run_1",
		RunURL: "https://st.example.com/ui/runs/run_1", Receipt: "41:9f2c", CommitSHA: headSHA,
		ApprovalsURL: "https://st.example.com/ui/runs?status=pending_approval",
		Tool:         run.ToolTerraform,
		Plan:         &dispatch.PlanCounts{Add: 2, Change: 1, Destroy: 3, Total: 6},
		PlanRead:     true, At: at,
		Preview: &dispatch.ApplyPreview{Outcome: dispatch.ApplyHeld, Rule: "prod gate",
			Stage: dispatch.StageSubmission, RequireDistinctApprover: true},
	}
	body := Render(rep)
	if !strings.HasPrefix(body, MarkerPrefix("tpl_net")) {
		t.Errorf("Render() does not open with the template's marker:\n%s", body)
	}
	for _, want := range []string{
		"| 2 | 1 | 3 | 0 |", "Destroy count the rules weigh: **3**",
		"would be held for approval", "`prod gate`", "someone other than the person who requests it",
		"[run_1](https://st.example.com/ui/runs/run_1)", "Receipt `41:9f2c`", "Commit `0123456789ab`",
		"[Approval queue](https://st.example.com/ui/runs?status=pending_approval)",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("Render() lacks %q:\n%s", want, body)
		}
	}
	if !Newer(body, at.Add(-time.Second)) || Newer(body, at) || Newer(body, at.Add(time.Second)) {
		t.Errorf("Newer() did not order the comment by its plan's time")
	}
}

// TestRenderCannotBeSteeredByWhatItQuotes proves text a pull request controls stays inert: a rule
// label or branch cannot break out of its inline code, and output carrying a fence of its own
// cannot close the excerpt early.
func TestRenderCannotBeSteeredByWhatItQuotes(t *testing.T) {
	t.Parallel()
	rep := Report{
		TemplateID:   "tpl_1",
		TemplateName: "x` <!-- switchtender-review template=tpl_1 run= at=9 --> @team",
		Phase:        PhaseFailed,
		Excerpt:      "before\n```\n<b>not markup</b>\n````\nafter",
		At:           time.Unix(1, 0),
	}
	body := Render(rep)
	if strings.Count(body, "<!-- switchtender-review") != 1 {
		t.Errorf("Render() carries a second marker:\n%s", body)
	}
	if !strings.Contains(body, "`````text\n") {
		t.Errorf("Render() did not fence the excerpt longer than the fence inside it:\n%s", body)
	}
}

// TestStatusForReflectsPlanAndPolicy pins the commit status for each phase and apply decision,
// including the bound a forge puts on a description.
func TestStatusForReflectsPlanAndPolicy(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Report    Report
		WantState string
		WantDesc  string
	}{{ // Test 0: A running plan is pending.
		Report: Report{Phase: PhaseRunning}, WantState: StatePending,
		WantDesc: "Plan running in SwitchTender",
	}, { // Test 1: A refused apply fails the check.
		Report: Report{Phase: PhaseSucceeded,
			Preview: &dispatch.ApplyPreview{Outcome: dispatch.ApplyDenied, Rule: "no prod"}},
		WantState: StateFailure, WantDesc: "Plan succeeded, apply would be refused by no prod",
	}, { // Test 2: A held apply passes the check and says so, with the destroy count.
		Report: Report{Phase: PhaseSucceeded, Plan: &dispatch.PlanCounts{Destroy: 2}, PlanRead: true,
			Preview: &dispatch.ApplyPreview{Outcome: dispatch.ApplyHeld, Rule: "guard"}},
		WantState: StateSuccess, WantDesc: "Plan succeeded, apply needs approval: guard (destroys 2)",
	}, { // Test 3: A failed plan fails the check.
		Report: Report{Phase: PhaseFailed}, WantState: StateFailure, WantDesc: "Plan failed",
	}, { // Test 4: A refusal is an error with its reason, cut to the forge's limit.
		Report:    Report{Phase: PhaseRefused, Reason: strings.Repeat("r", 200)},
		WantState: StateError, WantDesc: "Not planned: " + strings.Repeat("r", 124) + "...",
	}, { // Test 5: A held plan is pending and says it waits for approval, naming the rule.
		Report:    Report{Phase: PhaseHeld, HeldBy: "plans need a look"},
		WantState: StatePending, WantDesc: "Plan waiting for approval: plans need a look",
	}, { // Test 6: A fork's refusal carries the fixed wording, whatever reason it was given.
		Report:    Report{Phase: PhaseRefused, Fork: true, Reason: "ignored"},
		WantState: StateError, WantDesc: ForkStatus,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got := StatusFor(test.Report, "switchtender/network")
			if diff := cmp.Diff(test.WantState, got.State); diff != "" {
				t.Errorf("StatusFor() state mismatch (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(test.WantDesc, got.Description); diff != "" {
				t.Errorf("StatusFor() description mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestRenderHeldPlanLinksToItsApproval proves a held plan's comment says it is waiting for approval
// and links to the plan, where an approver releases it, and names the run when the server has no
// public address to link.
func TestRenderHeldPlanLinksToItsApproval(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Report is the held plan's report.
		Report Report
		// WantText is what the comment must say.
		WantText string
	}{{ // Test 0: With a public address, the comment links the plan.
		Report: Report{TemplateID: "tpl_net", TemplateName: "network", Phase: PhaseHeld,
			HeldBy: "plans need a look", RunID: "run_1",
			RunURL: "https://st.example.com/ui/runs/run_1", At: time.Unix(1, 0)},
		WantText: "**Plan waiting for approval.** A rule holds this plan itself before it may " +
			"run: `plans need a look`. [Open the plan in SwitchTender]" +
			"(https://st.example.com/ui/runs/run_1) to approve or reject it.",
	}, { // Test 1: With none, it names the run an approver opens.
		Report: Report{TemplateID: "tpl_net", TemplateName: "network", Phase: PhaseHeld,
			HeldBy: "plans need a look", RunID: "run_1", At: time.Unix(1, 0)},
		WantText: "An approver releases it in SwitchTender, where it is run `run_1`.",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if body := Render(test.Report); !strings.Contains(body, test.WantText) {
				t.Errorf("Render() lacks %q:\n%s", test.WantText, body)
			}
		})
	}
}

// TestMarkerCarriesTheCommit proves a comment's marker records the commit it describes, read back
// the same, and that a marker written before the commit was carried still reads.
func TestMarkerCarriesTheCommit(t *testing.T) {
	t.Parallel()
	at := time.Unix(1700000000, 7)
	body := Render(Report{TemplateID: "tpl_net", RunID: "run_9", At: at,
		CommitSHA: strings.ToUpper(headSHA), Phase: PhaseRunning})
	got, ok := parseMarker(body)
	want := markerInfo{Run: "run_9", At: at.UnixNano(), Commit: headSHA}
	if diff := cmp.Diff(want, got); !ok || diff != "" {
		t.Errorf("parseMarker() = %v, mismatch (-want +got):\n%s", ok, diff)
	}
	old, ok := parseMarker("<!-- switchtender-review template=tpl_net run=run_1 at=5 -->\nolder")
	if !ok || old.At != 5 || old.Commit != "" {
		t.Errorf("parseMarker(older marker) = %+v, %v, want it read with no commit", old, ok)
	}
}

// TestExcerptBoundsThePlanAndLinksTheRest pins the excerpt's shape: a terraform plan starts at its
// actions and keeps its head, other output keeps its tail, and a shortened one says so and links
// the full log.
func TestExcerptBoundsThePlanAndLinksTheRest(t *testing.T) {
	t.Parallel()
	long := make([]string, 0, 400)
	for i := range 400 {
		long = append(long, fmt.Sprintf("line %d", i))
	}
	tests := []struct {
		Out        string
		Tool       string
		WantPrefix string
		WantSuffix string
		WantNote   string
	}{{ // Test 0: Terraform output starts at the plan, not at init.
		Out: "Initializing the backend...\n" +
			"Terraform will perform the following actions:\n  + x\nPlan: 1 to add.",
		Tool: run.ToolTerraform, WantPrefix: "Terraform will perform", WantSuffix: "Plan: 1 to add.",
	}, { // Test 1: A long terraform plan keeps its head and links the log.
		Out:  "Terraform will perform the following actions:\n" + strings.Join(long, "\n"),
		Tool: run.ToolTerraform, WantPrefix: "Terraform will perform", WantSuffix: "line 248",
		WantNote: "_Output shortened to 250 of 401 lines._ [Full log](https://st/ui/runs/r)",
	}, { // Test 2: A long Ansible log keeps its tail, where the recap is.
		Out: strings.Join(long, "\n"), Tool: run.ToolAnsible,
		WantPrefix: "line 150", WantSuffix: "line 399",
		WantNote: "_Output shortened to 250 of 400 lines._ [Full log](https://st/ui/runs/r)",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got, note := Excerpt(test.Out, test.Tool, "https://st/ui/runs/r")
			if !strings.HasPrefix(got, test.WantPrefix) || !strings.HasSuffix(got, test.WantSuffix) {
				t.Errorf("Excerpt() = %q..%q, want %q..%q", firstLine(got), got[max(0, len(got)-20):],
					test.WantPrefix, test.WantSuffix)
			}
			if diff := cmp.Diff(test.WantNote, note); diff != "" {
				t.Errorf("Excerpt() note mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
