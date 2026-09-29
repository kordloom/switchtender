package receipt_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/receipt"
)

// TestReceiptSurvivesRetentionPrune proves a contiguous receipt built after retention has pruned the
// run's logs does not read as forged. The outcome entry committed a digest over the record with the
// run's real log hash; after the sweep the record rebuilds with the empty-log hash, so attaching it
// produced a receipt whose own offline verifier rejected the disclosure. Routine, documented
// housekeeping must degrade the receipt honestly, to chain-only with a note, never to one that fails
// verification.
func TestReceiptSurvivesRetentionPrune(t *testing.T) {
	ctx := context.Background()
	runs, audits, id, r := held(t, "approve")

	// Retention prunes the terminal run's events and logs, keeping the run row, exactly what
	// `serve --retain-events` does after the window passes.
	if n, err := runs.PurgeEventsBefore(ctx, time.Now().Add(time.Hour)); err != nil || n == 0 {
		t.Fatalf("PurgeEventsBefore = (%d, %v), want the run's logs pruned", n, err)
	}

	res, err := receipt.Build(ctx, runs, audits, id, "test", r.ID, receipt.Options{})
	if err != nil {
		t.Fatalf("Build after retention: %v", err)
	}
	rep, err := audit.VerifyBundle(res.Signed, id.KeyID())
	if err != nil {
		t.Fatalf("VerifyBundle: %v", err)
	}
	if !rep.SignatureOK || !rep.ChainOK {
		t.Fatalf("receipt does not verify at all: sig=%v chain=%v", rep.SignatureOK, rep.ChainOK)
	}
	if rep.OutcomePresent && !rep.OutcomeDigestOK {
		t.Fatalf("the receipt discloses an outcome that fails its committed digest: routine " +
			"retention made the flagship evidence artifact read as forged")
	}
	// The degradation must be said out loud, not silent.
	found := false
	for _, n := range res.Notes {
		if strings.Contains(n, "retention") {
			found = true
		}
	}
	if !found {
		t.Errorf("receipt degraded without saying why; Notes = %q", res.Notes)
	}
}
