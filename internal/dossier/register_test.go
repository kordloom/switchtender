package dossier

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/run"
)

// seedRegister stores three runs across a window boundary and the decisions over them.
func seedRegister(t *testing.T) (run.Store, audit.Store, time.Time) {
	t.Helper()
	ctx := context.Background()
	runs := run.NewMemStore()
	audits := audit.NewMemStore()
	base := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	for _, r := range []*run.Run{
		{ID: "run_inside1", Playbook: "site.yml", Status: run.StatusSucceeded,
			Actor: "deploy-bot", Source: "template", SourceID: "tpl_web", CreatedAt: base},
		{ID: "run_inside2", Tool: "terraform", Command: "infra/network", Status: run.StatusFailed,
			Actor: "root", CreatedAt: base.Add(24 * time.Hour)},
		{ID: "run_before", Playbook: "old.yml", Status: run.StatusSucceeded,
			CreatedAt: base.Add(-30 * 24 * time.Hour)},
	} {
		if err := runs.Save(ctx, r); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
	}
	// Decisions are read from the committed DECISION entries. The HTTP attempts sit beside them
	// deliberately, under the wrong actor: an attempt is recorded whether or not the decision took,
	// so a register that reads attempts names whoever pressed the button, including a requester
	// whose self-approval the separation gate refused.
	for _, e := range []struct{ Actor, Method, Path string }{
		{"impostor", "POST", "/v1/runs/run_inside1/approve"},
		{"impostor", "POST", "/v1/runs/run_inside2/reject"},
		{"root", audit.MethodDecision, "/runs/run_inside1/decision/approved"},
		{"root", audit.MethodDecision, "/runs/run_inside2/decision/rejected"},
		{"root", "POST", "/v1/projects"},
	} {
		if err := audits.Append(ctx, &audit.Entry{
			ID: audit.NewID(), At: base, Actor: e.Actor, Method: e.Method, Path: e.Path,
		}); err != nil {
			t.Fatalf("Append() error = %v", err)
		}
	}
	return runs, audits, base
}

func TestRegisterWindowsAndDecisions(t *testing.T) {
	t.Parallel()
	runs, audits, base := seedRegister(t)
	from := base.Add(-time.Hour)
	to := base.Add(7 * 24 * time.Hour)
	in, err := CollectRegister(context.Background(), runs, audits, "", from, to, to, 0)
	if err != nil {
		t.Fatalf("CollectRegister() error = %v", err)
	}
	if len(in.Runs) != 2 {
		t.Fatalf("runs in window = %d, want 2, the run before the window stays out", len(in.Runs))
	}
	if in.Runs[0].ID != "run_inside1" {
		t.Errorf("first row = %s, want run_inside1 oldest first", in.Runs[0].ID)
	}
	if d := in.Decisions["run_inside1"]; d.Verdict != "Approved" || d.Actor != "root" {
		t.Errorf("decision for run_inside1 = %+v, want approved by root", d)
	}
	if d := in.Decisions["run_inside2"]; d.Verdict != "Rejected" {
		t.Errorf("decision for run_inside2 = %+v, want rejected", d)
	}

	doc, err := RenderRegister(in)
	if err != nil {
		t.Fatalf("RenderRegister() error = %v", err)
	}
	html := string(doc)
	for _, want := range []string{
		"run_inside1", "run_inside2", "Approved by root", "Rejected by root",
		"terraform infra/network", "SOC 2 CC8.1",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("register is missing %q", want)
		}
	}
	if strings.Contains(html, "run_before") {
		t.Error("register carries a run from outside the period")
	}
}

func TestRegisterTallies(t *testing.T) {
	t.Parallel()
	runs, audits, base := seedRegister(t)
	in, err := CollectRegister(context.Background(), runs, audits, "",
		base.Add(-time.Hour), base.Add(7*24*time.Hour), base, 0)
	if err != nil {
		t.Fatalf("CollectRegister() error = %v", err)
	}
	doc, err := RenderRegister(in)
	if err != nil {
		t.Fatalf("RenderRegister() error = %v", err)
	}
	html := string(doc)
	// Two changes, one approved, one rejected, one failed outcome.
	for _, want := range []string{
		`<p class="k">Changes</p><p class="v">2</p>`,
		`<p class="k">Approved</p><p class="v">1</p>`,
		`<p class="k">Rejected</p><p class="v">1</p>`,
		`<p class="k">Failed</p><p class="v">1</p>`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("register tallies are missing %q", want)
		}
	}
}

// TestRegisterRedactsAnInlineSecretInAScript pins that the change register does not publish a secret
// a run's own script carried.
//
// A bash, python, powershell, or go run stores its whole script in Command, and a script holds
// whatever it was written with. The register is the one document built to be mailed to an outside
// auditor, and it printed that field verbatim in the Change column and copied it into the CSV export,
// which is exactly the disclosure the dossier already redacts the same field to prevent. The two
// documents describe the same run to the same reader and must not disagree about what is safe to
// show.
func TestRegisterRedactsAnInlineSecretInAScript(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	runs := run.NewMemStore()
	audits := audit.NewMemStore()
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

	if err := runs.Save(ctx, &run.Run{
		ID: "run_secret", Tool: run.ToolBash, Status: run.StatusSucceeded,
		Command:   "export DB_PASSWORD=hunter2secret\npsql -c 'select 1'",
		CreatedAt: base, StartedAt: &base, EndedAt: &base,
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	in, err := CollectRegister(ctx, runs, audits, "", base.Add(-time.Hour), base.Add(time.Hour),
		base.Add(time.Hour), 0)
	if err != nil {
		t.Fatalf("CollectRegister() error = %v", err)
	}
	if len(in.Runs) != 1 {
		t.Fatalf("rows = %d, want 1", len(in.Runs))
	}

	// The rendered document is what reaches the auditor, and the CSV export is built from the same
	// Change column, so this covers both.
	html, err := RenderRegister(in)
	if err != nil {
		t.Fatalf("RenderRegister() error = %v", err)
	}
	if strings.Contains(string(html), "hunter2secret") {
		t.Error("the rendered change register publishes a secret from the run's script")
	}
}

// TestUnanchoredBannerNamesTheRemedy pins that the warning state teaches the one-command fix.
//
// The unanchored banner used to state the problem and stop: the record rests on this install
// alone. A reader holding an evidence document is exactly the person who should learn that fixing
// it is one command, and which command, or the warning is a shrug. The remedy is the product's own
// free command, deliberately not a link: this document is handed to auditors, and evidence that
// advertises is evidence that reads as marketing.
func TestUnanchoredBannerNamesTheRemedy(t *testing.T) {
	t.Parallel()
	in := &RegisterInput{ChainOK: true, ChainCount: 3, Anchored: 0}
	doc, err := RenderRegister(in)
	if err != nil {
		t.Fatalf("RenderRegister() error = %v", err)
	}
	for _, want := range []string{"unanchored", "switchtender audit anchor", "switchtender witness"} {
		if !strings.Contains(string(doc), want) {
			t.Errorf("the unanchored register does not mention %q", want)
		}
	}
	if strings.Contains(string(doc), "switchtender.com/pricing") {
		t.Error("an evidence document links to pricing, which turns evidence into marketing")
	}
}
