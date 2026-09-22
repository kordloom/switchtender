package dossier_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/dossier"
	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/run"
)

// TestARegisterSaysWhenItsPeriodWasPruned covers the one way this document can mislead an auditor
// while every check on it passes.
//
// The register is the account of a period, and it lists the runs the store holds. Retention deletes
// runs on a schedule an operator sets, and the chain keeps every entry forever, so a period older
// than the retention window renders as a document that lists nothing and calls itself verified. It
// is verified: the chain is intact. It just is not the account it appears to be, and an auditor
// reading a quarter that held forty changes, two of them failures, sees a quiet one.
//
// Saying how many the chain records that the store no longer holds turns a false account into an
// incomplete one, which is the difference between misleading an auditor and telling them where to
// look.
func TestARegisterSaysWhenItsPeriodWasPruned(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	runs := run.NewMemStore()
	audits := audit.NewMemStore()

	from := time.Now().Add(-48 * time.Hour)
	// The period runs to just past now: an outcome is committed when a run finishes, so the entry
	// that names a pruned run carries that instant and has to fall inside the window.
	to := time.Now().Add(time.Hour)
	at := from.Add(time.Hour)

	// Two changes happen in the period, an hour apart, and both are committed to the chain.
	when := map[string]time.Time{"run_pruned": at, "run_kept": at.Add(2 * time.Hour)}
	for _, id := range []string{"run_pruned", "run_kept"} {
		code := 0
		created := when[id]
		started, ended := created, created.Add(time.Minute)
		r := &run.Run{
			ID: id, Playbook: "site.yml", Tool: run.ToolAnsible, Status: run.StatusSucceeded,
			CreatedAt: created, StartedAt: &started, EndedAt: &ended, ExitCode: &code,
		}
		if err := runs.Save(ctx, r); err != nil {
			t.Fatalf("save %s: %v", id, err)
		}
		if err := outcome.Commit(ctx, audits, runs, r, "system:test", nil); err != nil {
			t.Fatalf("commit outcome for %s: %v", id, err)
		}
	}

	// Retention sweeps the older one. The chain still records it, which is the whole point: the
	// evidence outlives the run, and the document has to say the run is gone rather than omit it.
	purged, err := runs.PurgeRunsBefore(ctx, at.Add(time.Hour))
	if err != nil {
		t.Fatalf("PurgeRunsBefore: %v", err)
	}
	if purged == 0 {
		t.Fatal("retention removed nothing, so this test is not exercising a pruned period")
	}

	in, cerr := dossier.CollectRegister(ctx, runs, audits, "in_test", from, to, time.Now(), 0)
	if cerr != nil {
		t.Fatalf("CollectRegister: %v", cerr)
	}
	if in.Pruned != 1 {
		t.Errorf("the register counts %d pruned changes, want 1: the chain records a change in "+
			"this period whose run the store no longer holds, and a document that does not say so "+
			"reads as an account of a period where that change never happened", in.Pruned)
	}

	// And the rendered document tells the reader, rather than only the struct knowing.
	html, rerr := dossier.RenderRegister(in)
	if rerr != nil {
		t.Fatalf("RenderRegister: %v", rerr)
	}
	if !strings.Contains(string(html), "incomplete") {
		t.Error("the rendered register does not tell its reader that the period is incomplete, so " +
			"an auditor reads a pruned quarter as a quiet one")
	}
}
