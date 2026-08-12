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
	for _, e := range []struct{ Actor, Path string }{
		{"root", "/v1/runs/run_inside1/approve"},
		{"root", "/v1/runs/run_inside2/reject"},
		{"root", "/v1/projects"},
	} {
		if err := audits.Append(ctx, &audit.Entry{
			ID: audit.NewID(), At: base, Actor: e.Actor, Method: "POST", Path: e.Path,
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
	in, err := CollectRegister(context.Background(), runs, audits, from, to, to, 0)
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
	in, err := CollectRegister(context.Background(), runs, audits,
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
