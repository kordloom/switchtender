package audit_test

import (
	"strings"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
)

// governedChain returns a run's three entries: the proposal, the approval, and the outcome. The
// caller sets each time, which is the whole point: the defect this file exists for was a receipt
// whose approval was stamped twenty minutes after the run it released.
func governedChain(t *testing.T, proposed, approved, ran time.Time) []*audit.Entry {
	t.Helper()
	const runID = "run_timeorder"
	entries := []*audit.Entry{{
		ID: audit.NewID(), At: proposed, Actor: "deploy-bot", ActorType: "token",
		Method: "POST", Path: "/v1/runs",
	}, {
		ID: audit.NewID(), At: approved, Actor: "admin", ActorType: "user",
		Method: audit.MethodDecision, Path: "/runs/" + runID + "/decision/approved",
	}, {
		ID: audit.NewID(), At: ran, Actor: "system:dispatcher", ActorType: "system",
		Method: audit.MethodRun, Path: "/runs/" + runID + "/outcome/succeeded",
	}}
	var prev *audit.Entry
	for _, e := range entries {
		audit.Link(prev, e)
		prev = e
	}
	return entries
}

// verifyGoverned builds and signs a bundle over the given entries and returns its report.
func verifyGoverned(t *testing.T, entries []*audit.Entry) *audit.BundleReport {
	t.Helper()
	t.Setenv("SWITCHTENDER_AUDIT_KEY", "")
	id, err := audit.LoadIdentity(t.TempDir())
	if err != nil {
		t.Fatalf("LoadIdentity() error = %v", err)
	}
	b, err := audit.BuildBundle(entries, id, "v-test", time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("BuildBundle() error = %v", err)
	}
	signed, err := audit.SignBundleDoc(b, id.Private())
	if err != nil {
		t.Fatalf("SignBundleDoc() error = %v", err)
	}
	rep, err := audit.VerifyBundle(signed, id.KeyID())
	if err != nil {
		t.Fatalf("VerifyBundle() error = %v", err)
	}
	return rep
}

// TestApprovalAfterTheRunFailsTheReceipt is the defect this check was written for. The demo's
// flagship governed receipt published a run executed at 07:42:51 and approved at 08:02:42, twenty
// minutes after it ran, and the shipping verifier printed VERIFIED over it. Digests agreeing was
// never enough, because the same spec can be approved after the fact, and a receipt that shows the
// gate being bypassed is the one artifact this product cannot get wrong.
func TestApprovalAfterTheRunFailsTheReceipt(t *testing.T) {
	// Not parallel: verifyGoverned sets an environment variable to isolate the signing identity.
	base := time.Date(2026, 9, 9, 7, 42, 42, 0, time.UTC)
	tests := []struct {
		Name        string
		Approved    time.Time
		Ran         time.Time
		WantOK      bool
		WantOrdered bool
	}{{ // Test 0: The gate holding. Approved, then run.
		Name: "approved then run", Approved: base.Add(5 * time.Second), Ran: base.Add(9 * time.Second),
		WantOK: true, WantOrdered: true,
	}, { // Test 1: The demo's real shape. Run, then approved twenty minutes later.
		Name: "run then approved", Approved: base.Add(20 * time.Minute), Ran: base.Add(9 * time.Second),
		WantOK: false, WantOrdered: false,
	}, { // Test 2: Same instant is not an inversion. A fast approval and dispatch can share a second.
		Name: "same instant", Approved: base.Add(9 * time.Second), Ran: base.Add(9 * time.Second),
		WantOK: true, WantOrdered: true,
	}, { // Test 3: One second late is still late. The check is not a tolerance.
		Name: "one second after", Approved: base.Add(10 * time.Second), Ran: base.Add(9 * time.Second),
		WantOK: false, WantOrdered: false,
	}}
	for testNum, test := range tests {
		t.Run(strings.ReplaceAll(test.Name, " ", "_"), func(t *testing.T) {
			rep := verifyGoverned(t, governedChain(t, base, test.Approved, test.Ran))
			if rep.ApprovalPrecedesRun != test.WantOrdered {
				t.Errorf("test %d: ApprovalPrecedesRun = %v, want %v",
					testNum, rep.ApprovalPrecedesRun, test.WantOrdered)
			}
			if rep.OK() != test.WantOK {
				t.Errorf("test %d: OK() = %v, want %v; time problems: %v",
					testNum, rep.OK(), test.WantOK, rep.TimeProblems)
			}
			if !test.WantOrdered && len(rep.TimeProblems) == 0 {
				t.Errorf("test %d: an out-of-order approval reported no problem, so a reader is "+
					"told nothing about why the receipt failed", testNum)
			}
		})
	}
}

// TestTimeGoingBackwardIsReportedNotFailed pins a deliberate decision. A written link commits to the
// time it holds and cannot be corrected afterward, so a clock that stepped backward between two
// unrelated entries is surfaced for a reader to judge rather than treated as tampering. Only the
// approval-after-run case fails the receipt, because only that one says the gate did not hold.
func TestTimeGoingBackwardIsReportedNotFailed(t *testing.T) {
	// Not parallel: verifyGoverned sets an environment variable to isolate the signing identity.
	base := time.Date(2026, 9, 9, 7, 42, 42, 0, time.UTC)
	// The proposal is dated after the approval, which is backward, but the approval still precedes
	// the run so the gate itself held.
	entries := governedChain(t, base.Add(time.Hour), base.Add(5*time.Second), base.Add(9*time.Second))
	rep := verifyGoverned(t, entries)

	if len(rep.TimeProblems) == 0 {
		t.Error("a chain whose time steps backward reported no problem")
	}
	if !rep.ApprovalPrecedesRun {
		t.Error("ApprovalPrecedesRun = false, but the approval does precede the run here")
	}
	if !rep.OK() {
		t.Errorf("OK() = false, want true: time going backward is reported, not fatal. problems: %v",
			rep.TimeProblems)
	}
}
