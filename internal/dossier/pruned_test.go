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

	in, cerr := dossier.CollectRegister(ctx, runs, audits, audit.Identity{InstallID: "in_test"}, from, to, time.Now(), 0)
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

// TestATruncatedRegisterDoesNotBlameRetention pins the half of the pruned count that is easy to get
// backwards and worse than saying nothing.
//
// The count answers "what does the chain record here that the store no longer holds", and it answers
// it by comparing the chain against the page of runs this document lists. On a register that hit its
// cap that page is not the period: every change the cap dropped is missing from it for a reason that
// has nothing to do with retention, and counting those tells an auditor that runs still sitting in
// the store were destroyed. The document then contradicts itself in the same summary block, saying
// retention removed them and, two lines later, that the next register carries them.
//
// A truncated register says nothing here instead. It already says on its face that it is partial,
// which is the honest statement. Under-reporting a pruned period is silence; over-reporting one is
// an accusation the store itself disproves.
func TestATruncatedRegisterDoesNotBlameRetention(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	runs := run.NewMemStore()
	audits := audit.NewMemStore()

	from := time.Now().Add(-48 * time.Hour)
	to := time.Now().Add(time.Hour)

	// Four changes, all still in the store, none pruned.
	for i, id := range []string{"run_a", "run_b", "run_c", "run_d"} {
		code := 0
		created := from.Add(time.Duration(i+1) * time.Hour)
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

	// A cap below the number of changes, which is what a real period past the bound reaches.
	in, err := dossier.CollectRegister(ctx, runs, audits, audit.Identity{InstallID: "in_test"}, from, to, time.Now(), 2)
	if err != nil {
		t.Fatalf("CollectRegister: %v", err)
	}
	if !in.Truncated {
		t.Fatal("the fixture did not truncate, so this test is not exercising the case it exists for")
	}
	if in.Pruned != 0 {
		t.Errorf("a truncated register reports %d pruned changes while every run is still in the "+
			"store: the page the count compares against was cut by the cap, so the changes the cap "+
			"dropped are being blamed on retention", in.Pruned)
	}

	html, rerr := dossier.RenderRegister(in)
	if rerr != nil {
		t.Fatalf("RenderRegister: %v", rerr)
	}
	if strings.Contains(string(html), "retention removed") {
		t.Error("a truncated register tells its reader that retention removed changes that are " +
			"still in the store, in the same summary block that says the next register carries them")
	}
}
