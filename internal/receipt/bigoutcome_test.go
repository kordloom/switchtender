package receipt_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/receipt"
	"github.com/kordloom/switchtender/internal/run"
)

// TestAnOversizedOutcomeStillVerifies covers the flagship artifact for a fleet large enough to
// matter.
//
// The outcome record is disclosed in a receipt beside the digest the chain committed, so a verifier
// can confirm what the run did. It was disclosed as a re-parsed tree, and a verifier re-marshaled
// that tree to re-digest it. Below the canonical size cap the re-marshal matched, because both
// sides were canonicalized to sorted key order. Above the cap the digest is taken over the raw
// bytes, which are the struct's field order, while the tree re-marshals in key order, so the two
// disagreed and an honest receipt for a large run reported NOT VERIFIED.
//
// A run over a real fleet reaches the cap without anything unusual: host summaries are read
// unbounded, and a few thousand hosts cross a megabyte. The disclosure now carries the exact bytes
// the digest was taken over, so key order never enters into it.
func TestAnOversizedOutcomeStillVerifies(t *testing.T) {
	ctx := context.Background()
	runs := run.NewMemStore()
	audits := audit.NewMemStore()
	t.Setenv("SWITCHTENDER_AUDIT_KEY", "")
	id, err := audit.LoadIdentity(t.TempDir())
	if err != nil {
		t.Fatalf("LoadIdentity: %v", err)
	}

	creation := &audit.Entry{
		ID: audit.NewID(), At: time.Now(), Actor: "casey", ActorType: "session",
		Method: "POST", Path: "/v1/runs",
	}
	if err := audits.Append(ctx, creation); err != nil {
		t.Fatalf("append creation: %v", err)
	}

	r := &run.Run{
		ID: "run_big", Status: run.StatusRunning, CreatedAt: time.Now(),
		Tool: run.ToolAnsible, Playbook: "site.yml", Inventory: "prod",
		Actor: "casey", ActorType: "session", AuditReceipt: chainReceipt(creation),
	}
	if err := runs.Save(ctx, r); err != nil {
		t.Fatalf("save run: %v", err)
	}

	// Enough hosts to push the outcome record past the canonical size cap. Thirty-character names
	// so the count needed is a real fleet's rather than a synthetic extreme.
	const hosts = 12000
	summaries := make([]run.HostSummary, hosts)
	for i := range summaries {
		summaries[i] = run.HostSummary{
			Host: fmt.Sprintf("host-%025d.prod.internal", i),
			OK:   3, Changed: 1, Worst: "changed",
		}
	}
	if err := runs.SaveHostSummary(ctx, r.ID, summaries); err != nil {
		t.Fatalf("save host summaries: %v", err)
	}

	// Confirm the fixture actually crosses the cap this test exists for, so it cannot pass by
	// staying under it after some future change to the record shape.
	body, err := outcome.Body(ctx, runs, r)
	if err != nil {
		t.Fatalf("outcome.Body: %v", err)
	}
	if len(body) <= audit.MaxCanonicalDigestBytes {
		t.Fatalf("the outcome is %d bytes, not over the %d cap this test needs",
			len(body), audit.MaxCanonicalDigestBytes)
	}

	code := 0
	ended := time.Now()
	r.Status = run.StatusSucceeded
	r.ExitCode = &code
	r.EndedAt = &ended
	if err := runs.Save(ctx, r); err != nil {
		t.Fatalf("save terminal run: %v", err)
	}
	if err := outcome.Commit(ctx, audits, runs, r, "system:test", nil); err != nil {
		t.Fatalf("Commit outcome: %v", err)
	}

	res, err := receipt.Build(ctx, runs, audits, id, "test", r.ID, receipt.Options{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	rep, err := audit.VerifyBundle(res.Signed, id.KeyID())
	if err != nil {
		t.Fatalf("VerifyBundle: %v", err)
	}
	if !rep.OutcomePresent || !rep.OutcomeDigestOK {
		t.Errorf("a receipt for a run over a large fleet reports its own outcome as tampered: "+
			"present=%v ok=%v. The disclosed bytes must be exactly the bytes the digest covers, "+
			"whatever the size", rep.OutcomePresent, rep.OutcomeDigestOK)
	}
	if !rep.OK() {
		t.Errorf("the receipt does not verify: %+v", rep)
	}
}
