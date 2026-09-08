package receipt_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/receipt"
	"github.com/kordloom/switchtender/internal/run"
)

// errStore is what a wrapper store returns to stand in for a database that is unreachable while a
// receipt is being assembled.
var errStore = errors.New("test: the store refused this read")

// anchorReadFails is an audit store whose anchors cannot be read. A receipt drawn from a chain
// whose anchors are unknown must refuse rather than assume there are none.
type anchorReadFails struct {
	audit.Store
}

// SaveAnchor is never called; it exists so the type satisfies audit.AnchorStore.
func (s *anchorReadFails) SaveAnchor(context.Context, *audit.Anchor) error { return errStore }

// Anchors refuses the read.
func (s *anchorReadFails) Anchors(context.Context, int64) ([]*audit.Anchor, error) {
	return nil, errStore
}

// DeleteAnchor is never called; it exists so the type satisfies audit.AnchorStore.
func (s *anchorReadFails) DeleteAnchor(context.Context, string) error { return errStore }

// chainScanFails is an audit store whose chain cannot be streamed.
type chainScanFails struct {
	audit.Store
}

// ChainScan refuses the stream.
func (s *chainScanFails) ChainScan(context.Context, int64, func(*audit.Entry) error) error {
	return errStore
}

// summariesUnreadable is a run store that cannot answer for a run's per host summaries, which is
// what the outcome record is rebuilt from.
type summariesUnreadable struct {
	run.Store
}

// RunHostSummaries refuses the read.
func (s *summariesUnreadable) RunHostSummaries(context.Context, string) ([]run.HostSummary, error) {
	return nil, errStore
}

// busyInstall builds two runs on one chain, with the other run's outcome entry landing between this
// run's creation entry and its own outcome entry. It is the ordinary shape of an install that runs
// more than one thing at a time, and it returns the subject run first.
//
// The fixture matters because a receipt for a run nobody else was competing with proves nothing
// about how the builder picks entries out of a shared chain.
func busyInstall(t *testing.T) (run.Store, audit.Store, audit.Identity, *run.Run, *run.Run) {
	t.Helper()
	ctx := context.Background()
	runs := run.NewMemStore()
	audits := audit.NewMemStore()
	t.Setenv("SWITCHTENDER_AUDIT_KEY", "")
	id, err := audit.LoadIdentity(t.TempDir())
	if err != nil {
		t.Fatalf("LoadIdentity: %v", err)
	}

	start := func(runID string) *run.Run {
		t.Helper()
		creation := &audit.Entry{
			ID: audit.NewID(), At: time.Now(), Actor: "casey", ActorType: "session",
			Method: "POST", Path: "/v1/runs",
		}
		if err := audits.Append(ctx, creation); err != nil {
			t.Fatalf("append creation for %s: %v", runID, err)
		}
		r := &run.Run{
			ID: runID, Status: run.StatusRunning, CreatedAt: time.Now(),
			Tool: run.ToolBash, Command: "deploy " + runID, Actor: "casey", ActorType: "session",
			AuditReceipt: chainReceipt(creation),
		}
		if err := runs.Save(ctx, r); err != nil {
			t.Fatalf("save %s: %v", runID, err)
		}
		return r
	}
	finish := func(r *run.Run) {
		t.Helper()
		ended := time.Now()
		code := 0
		r.Status = run.StatusSucceeded
		r.ExitCode = &code
		r.EndedAt = &ended
		if err := runs.Save(ctx, r); err != nil {
			t.Fatalf("save terminal %s: %v", r.ID, err)
		}
		if err := outcome.Commit(ctx, audits, runs, r, "system:test", nil); err != nil {
			t.Fatalf("Commit outcome for %s: %v", r.ID, err)
		}
	}

	subject := start("run_aaaaaaaaaaaaaaaa")
	other := start("run_bbbbbbbbbbbbbbbb")
	finish(other)
	finish(subject)
	return runs, audits, id, subject, other
}

// TestReceiptRefusesAnUnreadableCreationReceipt pins every way a stored creation receipt can be
// malformed, and pins that each one refuses rather than guessing a chain position. The receipt is
// the only thing tying a run row to the entry that recorded its request, so a builder that read a
// broken one loosely would place the run at some other entry and sign the result.
func TestReceiptRefusesAnUnreadableCreationReceipt(t *testing.T) {
	ctx := context.Background()
	runs, audits, id, r := held(t, "approved")

	tests := []struct {
		// Name says what is wrong with the stored receipt.
		Name string
		// Receipt is the value stamped on the run.
		Receipt string
		// WantMessage is a phrase the refusal must carry so an operator can act on it.
		WantMessage string
	}{{ // Test 0: No separator at all, so nothing names a position.
		Name: "no separator", Receipt: "41", WantMessage: "unreadable creation receipt",
	}, { // Test 1: A position that is not a number.
		Name: "non numeric", Receipt: "abc:9f2c", WantMessage: "not a chain position",
	}, { // Test 2: Zero, which is one below the first entry a chain ever assigns.
		Name: "zero position", Receipt: "0:9f2c", WantMessage: "not a chain position",
	}, { // Test 3: A negative position.
		Name: "negative position", Receipt: "-1:9f2c", WantMessage: "not a chain position",
	}, { // Test 4: A position wider than an int64, the shape an overflow attempt takes.
		Name: "overflowing position", Receipt: "99999999999999999999:9f2c",
		WantMessage: "not a chain position",
	}, { // Test 5: A position present but the link missing, so nothing pins the entry's content.
		Name: "missing link", Receipt: "41:", WantMessage: "missing its link",
	}, { // Test 6: Only a separator.
		Name: "separator only", Receipt: ":", WantMessage: "not a chain position",
	}, { // Test 7: Whitespace around the position, which is not a number Go parses.
		Name: "padded position", Receipt: " 41 :9f2c", WantMessage: "not a chain position",
	}, { // Test 8: Non-ASCII digits, which look numeric and are not.
		Name: "unicode digits", Receipt: "４１:9f2c", WantMessage: "not a chain position",
	}, { // Test 9: A very long value, the shape a hostile import takes.
		Name: "very long", Receipt: strings.Repeat("9", 5000) + ":9f2c",
		WantMessage: "not a chain position",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			broken := *r
			broken.ID = fmt.Sprintf("run_broken_%d", testNum)
			broken.AuditReceipt = test.Receipt
			if err := runs.Save(ctx, &broken); err != nil {
				t.Fatalf("save: %v", err)
			}
			res, err := receipt.Build(ctx, runs, audits, id, "test", broken.ID, receipt.Options{})
			if err == nil {
				t.Fatalf("%s: a receipt was built from a creation receipt of %q", test.Name,
					test.Receipt)
			}
			if res != nil {
				t.Errorf("%s: a refusal still returned a result", test.Name)
			}
			if !strings.Contains(err.Error(), test.WantMessage) {
				t.Errorf("%s: error = %v, want it to mention %q", test.Name, err, test.WantMessage)
			}
		})
	}
}

// TestReceiptRefusesAnOutcomeRecordedBeforeItsCreation pins the ordering check. A run whose outcome
// sits earlier on the chain than its creation describes a chain that cannot have happened, and a
// signed document built over that ordering would attest a history the install cannot stand behind.
func TestReceiptRefusesAnOutcomeRecordedBeforeItsCreation(t *testing.T) {
	ctx := context.Background()
	runs, audits, id, r := held(t, "approved")

	chain, err := audits.Chain(ctx)
	if err != nil {
		t.Fatalf("Chain: %v", err)
	}
	last := chain[len(chain)-1]
	// The run now claims it was created after everything on the chain, including its own outcome.
	r.AuditReceipt = strconv.FormatInt(last.Seq+1, 10) + ":" + last.Hash
	if err := runs.Save(ctx, r); err != nil {
		t.Fatalf("save: %v", err)
	}

	res, err := receipt.Build(ctx, runs, audits, id, "test", r.ID, receipt.Options{})
	if err == nil {
		t.Fatalf("a receipt was built for a run whose outcome precedes its creation: %+v", res)
	}
	if !strings.Contains(err.Error(), "broken chain") {
		t.Errorf("error = %v, want it to name the broken ordering", err)
	}
}

// TestReceiptRefusesWhenTheAnchorsCannotBeRead pins that an unreadable anchor table fails the build
// closed. The anchors are the only evidence that catches a chain whose tail was removed, so a
// builder that carried on when it could not read them would publish exactly the receipt the anchor
// check exists to withhold, and would do it whenever the anchor read happened to fail.
func TestReceiptRefusesWhenTheAnchorsCannotBeRead(t *testing.T) {
	ctx := context.Background()
	runs, audits, id, r := held(t, "approved")

	for testNum, opts := range []receipt.Options{{}, {Sparse: true}} {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			res, err := receipt.Build(ctx, runs, &anchorReadFails{Store: audits}, id, "test", r.ID, opts)
			if err == nil {
				t.Fatalf("sparse=%v: a receipt was built without reading the anchors: %+v",
					opts.Sparse, res)
			}
			if !errors.Is(err, errStore) {
				t.Errorf("sparse=%v: error = %v, want the store's own error wrapped", opts.Sparse, err)
			}
			if !strings.Contains(err.Error(), "anchors") {
				t.Errorf("sparse=%v: error = %v, want it to name the anchor read", opts.Sparse, err)
			}
		})
	}
}

// TestReceiptRefusesWhenTheChainCannotBeRead pins that a failed chain stream refuses rather than
// assembling a receipt from the entries it managed to read before the failure. A partial segment
// would be a signed document that silently omits entries, which is the one thing a hash chain
// cannot detect on its own.
func TestReceiptRefusesWhenTheChainCannotBeRead(t *testing.T) {
	ctx := context.Background()
	runs, audits, id, r := held(t, "approved")

	res, err := receipt.Build(ctx, runs, &chainScanFails{Store: audits}, id, "test", r.ID,
		receipt.Options{})
	if err == nil {
		t.Fatalf("a receipt was built from a chain that could not be read: %+v", res)
	}
	if !errors.Is(err, errStore) {
		t.Errorf("error = %v, want the store's own error wrapped", err)
	}
}

// TestReceiptRefusesARunItCannotFind pins the refusal for identifiers that name nothing, including
// the empty string and shapes an attacker would try. A builder that answered a lookup miss with a
// receipt over whatever it found would be a receipt for a run that does not exist.
func TestReceiptRefusesARunItCannotFind(t *testing.T) {
	ctx := context.Background()
	runs, audits, id, _ := held(t, "approved")

	tests := []struct {
		// Name says which identifier shape this is.
		Name string
		// RunID is what the caller asked for.
		RunID string
	}{
		{Name: "empty", RunID: ""},                               // Test 0: No identifier at all.
		{Name: "unknown", RunID: "run_nothinghere"},              // Test 1: A well formed miss.
		{Name: "path traversal", RunID: "../../etc/passwd"},      // Test 2: A path, not an id.
		{Name: "wildcard", RunID: "%"},                           // Test 3: A SQL wildcard.
		{Name: "unicode", RunID: "run_🚂"},                        // Test 4: Non-ASCII.
		{Name: "very long", RunID: strings.Repeat("r", 100_000)}, // Test 5: An unbounded id.
		{Name: "newline", RunID: "run_1\nrun_2"},                 // Test 6: An embedded newline.
		{Name: "null byte", RunID: "run_1\x00"},                  // Test 7: An embedded null.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			res, err := receipt.Build(ctx, runs, audits, id, "test", test.RunID, receipt.Options{})
			if err == nil {
				t.Fatalf("%s: a receipt was built for a run that does not exist: %+v", test.Name, res)
			}
			if !errors.Is(err, run.ErrNotFound) {
				t.Errorf("%s: error = %v, want it to wrap the store's not-found", test.Name, err)
			}
		})
	}
}

// TestReceiptAttachesTheAnchorsCoveringIt pins the anchor wiring on the path where the chain does
// satisfy its anchors. The refusal is already covered elsewhere; what is checked here is that a
// satisfied anchor is actually carried into the signed document, because an anchor that is verified
// and then dropped leaves the receipt resting on this install's word with nothing saying so.
func TestReceiptAttachesTheAnchorsCoveringIt(t *testing.T) {
	ctx := context.Background()
	runs, audits, id, r := held(t, "approved")

	chain, err := audits.Chain(ctx)
	if err != nil {
		t.Fatalf("Chain: %v", err)
	}
	head := chain[len(chain)-1]
	anchors, ok := audits.(audit.AnchorStore)
	if !ok {
		t.Fatal("the audit store keeps no anchors, so this wiring cannot be exercised")
	}
	if err := anchors.SaveAnchor(ctx, &audit.Anchor{
		ID: "anc_ok", Type: audit.AnchorHTTPS, Shape: audit.AnchorShapeLinear,
		Seq: head.Seq, Link: head.Hash, At: time.Now(),
		Ref: "https://example.com/head", InstallID: id.InstallID,
	}); err != nil {
		t.Fatalf("SaveAnchor: %v", err)
	}

	res, err := receipt.Build(ctx, runs, audits, id, "test", r.ID, receipt.Options{})
	if err != nil {
		t.Fatalf("Build with a satisfied anchor: %v", err)
	}
	if res.Anchors != 1 {
		t.Errorf("Anchors = %d, want the one anchor covering the receipt", res.Anchors)
	}
	if res.UnanchoredSparse {
		t.Error("a contiguous receipt reported itself as an unanchored sparse one")
	}
	rep, err := audit.VerifyBundle(res.Signed, id.KeyID())
	if err != nil {
		t.Fatalf("VerifyBundle: %v", err)
	}
	if !rep.AnchorsOK {
		t.Errorf("the attached anchor does not verify: %+v", rep)
	}
}

// TestSparseReceiptUnderATreeAnchorIsNotFlaggedUnanchored pins the other side of the
// UnanchoredSparse flag. The flag is how a caller decides whether to warn that the root rests on
// this install's word alone, so it has to clear once a real tree anchor covers the root, or every
// receipt would carry a warning that means nothing.
func TestSparseReceiptUnderATreeAnchorIsNotFlaggedUnanchored(t *testing.T) {
	ctx := context.Background()
	runs, audits, id, r := held(t, "approved")

	chain, err := audits.Chain(ctx)
	if err != nil {
		t.Fatalf("Chain: %v", err)
	}
	size, root, err := audit.TreeHead(chain, id.InstallID)
	if err != nil {
		t.Fatalf("TreeHead: %v", err)
	}
	anchors, ok := audits.(audit.AnchorStore)
	if !ok {
		t.Fatal("the audit store keeps no anchors, so this wiring cannot be exercised")
	}
	if err := anchors.SaveAnchor(ctx, &audit.Anchor{
		ID: "anc_tree", Type: audit.AnchorHTTPS, Shape: audit.AnchorShapeTree,
		Seq: size, Link: root, At: time.Now(),
		Ref: "https://example.com/root", InstallID: id.InstallID,
	}); err != nil {
		t.Fatalf("SaveAnchor: %v", err)
	}

	res, err := receipt.Build(ctx, runs, audits, id, "test", r.ID, receipt.Options{Sparse: true})
	if err != nil {
		t.Fatalf("Build sparse under a tree anchor: %v", err)
	}
	if res.Anchors != 1 {
		t.Errorf("Anchors = %d, want the tree anchor over the root", res.Anchors)
	}
	if res.UnanchoredSparse {
		t.Error("a sparse receipt covered by a tree anchor still reported that nothing outside " +
			"this install fixes its root")
	}
}

// TestSparseConsistencyWindow pins what From accepts and what it refuses. The proof shows the log a
// reader already saw is a prefix of the log there is now, so a size the log never had must be a
// refusal: silently omitting the proof would leave the reader believing they had checked something
// they had not.
func TestSparseConsistencyWindow(t *testing.T) {
	ctx := context.Background()
	runs, audits, id, r := held(t, "approved")
	chain, err := audits.Chain(ctx)
	if err != nil {
		t.Fatalf("Chain: %v", err)
	}
	size := int64(len(chain))

	tests := []struct {
		// Name says which end of the window this is.
		Name string
		// From is the earlier log size to prove against.
		From int64
		// WantProof says the signed receipt must carry a consistency member.
		WantProof bool
		// WantRefusal says Build must refuse.
		WantRefusal bool
	}{
		{Name: "unset", From: 0, WantProof: false},                 // Test 0: No proof asked for.
		{Name: "negative", From: -1, WantProof: false},             // Test 1: Not a size, so ignored.
		{Name: "first entry", From: 1, WantProof: true},            // Test 2: The smallest prefix.
		{Name: "one below head", From: size - 1, WantProof: true},  // Test 3: Just inside.
		{Name: "whole log", From: size, WantProof: true},           // Test 4: The log itself.
		{Name: "past the head", From: size + 1, WantRefusal: true}, // Test 5: One past the end.
		{Name: "far past", From: 1 << 40, WantRefusal: true},       // Test 6: Absurdly past the end.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			res, err := receipt.Build(ctx, runs, audits, id, "test", r.ID,
				receipt.Options{Sparse: true, From: test.From})
			if test.WantRefusal {
				if err == nil {
					t.Fatalf("%s: From=%d produced a receipt instead of refusing", test.Name, test.From)
				}
				return
			}
			if err != nil {
				t.Fatalf("%s: Build: %v", test.Name, err)
			}
			if got := strings.Contains(string(res.Signed), `"consistency"`); got != test.WantProof {
				t.Errorf("%s: consistency member present = %v, want %v", test.Name, got, test.WantProof)
			}
			rep, err := audit.VerifyBundle(res.Signed, id.KeyID())
			if err != nil {
				t.Fatalf("%s: VerifyBundle: %v", test.Name, err)
			}
			if !rep.OK() {
				t.Errorf("%s: the receipt does not verify: %+v", test.Name, rep)
			}
		})
	}
}

// TestSparseReceiptCarriesNoDisclosures pins the rule the sparse shape is built on. A tree leaf's
// hash covers its claim's whole payload, so anything added to a disclosed claim recomputes to a
// different leaf than the one the signed root was folded from, and the receipt reads as broken. The
// sparse shape therefore proves membership and discloses nothing, and that has to be checked on the
// bytes rather than trusted to stay true.
func TestSparseReceiptCarriesNoDisclosures(t *testing.T) {
	ctx := context.Background()
	runs, audits, id, r := held(t, "approved")

	res, err := receipt.Build(ctx, runs, audits, id, "test", r.ID, receipt.Options{Sparse: true})
	if err != nil {
		t.Fatalf("Build sparse: %v", err)
	}
	for _, member := range []string{"outcome_body", "outcome_nonce", "spec_body", "decision_body",
		"decision_nonce"} {
		if strings.Contains(string(res.Signed), member) {
			t.Errorf("a sparse receipt carries %q, which makes its own leaf recompute differently "+
				"and the receipt read as broken", member)
		}
	}
	rep, err := audit.VerifyBundle(res.Signed, id.KeyID())
	if err != nil {
		t.Fatalf("VerifyBundle: %v", err)
	}
	if !rep.OK() {
		t.Fatalf("a sparse receipt does not verify: %+v", rep)
	}
	if rep.OutcomePresent || rep.DecisionsPresent != 0 {
		t.Errorf("a sparse receipt disclosed an outcome (%v) or %d decisions",
			rep.OutcomePresent, rep.DecisionsPresent)
	}
}

// TestReceiptNotesAnOutcomeItCannotRebuild pins the degradation path when the store cannot answer
// for the facts the outcome record is built from. The receipt must still be issued, proving the
// chain, with a note saying what it does not show. Refusing outright would deny an operator the
// evidence they do have, and attaching an outcome rebuilt from an incomplete read would hand out a
// document whose own verifier calls it forged.
func TestReceiptNotesAnOutcomeItCannotRebuild(t *testing.T) {
	ctx := context.Background()
	runs, audits, id, r := held(t, "approved")

	res, err := receipt.Build(ctx, &summariesUnreadable{Store: runs}, audits, id, "test", r.ID,
		receipt.Options{})
	if err != nil {
		t.Fatalf("Build with unreadable summaries: %v", err)
	}
	rep, err := audit.VerifyBundle(res.Signed, id.KeyID())
	if err != nil {
		t.Fatalf("VerifyBundle: %v", err)
	}
	if !rep.SignatureOK || !rep.ChainOK {
		t.Fatalf("the chain proof was lost along with the outcome: sig=%v chain=%v",
			rep.SignatureOK, rep.ChainOK)
	}
	if rep.OutcomePresent {
		t.Error("an outcome that could not be rebuilt was disclosed anyway")
	}
	found := false
	for _, n := range res.Notes {
		if strings.Contains(n, "could not be rebuilt") {
			found = true
		}
	}
	if !found {
		t.Errorf("the receipt degraded without saying why; Notes = %q", res.Notes)
	}
}

// TestReceiptWithholdsWhatItCannotCanonicalize pins the other degradation: a run whose spec cannot
// be reduced to canonical bytes at all. Nothing that depends on those bytes may be disclosed, and
// the receipt must still prove the chain and say what is missing, rather than disclosing a body
// that no longer matches the digest the chain committed.
func TestReceiptWithholdsWhatItCannotCanonicalize(t *testing.T) {
	ctx := context.Background()
	runs, audits, id, r := held(t, "approved")

	// A value JSON cannot represent, arriving on the run after the decision and the outcome were
	// committed. Nothing rebuilt from this run can reproduce the committed bytes.
	r.ExtraVars = map[string]any{"threshold": math.NaN()}
	if err := runs.Save(ctx, r); err != nil {
		t.Fatalf("save: %v", err)
	}

	res, err := receipt.Build(ctx, runs, audits, id, "test", r.ID, receipt.Options{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	rep, err := audit.VerifyBundle(res.Signed, id.KeyID())
	if err != nil {
		t.Fatalf("VerifyBundle: %v", err)
	}
	if !rep.SignatureOK || !rep.ChainOK {
		t.Fatalf("the chain proof was lost: sig=%v chain=%v", rep.SignatureOK, rep.ChainOK)
	}
	if rep.OutcomePresent {
		t.Error("an outcome was disclosed for a run whose canonical bytes cannot be produced")
	}
	if rep.DecisionsPresent != 0 {
		t.Error("a decision was disclosed for a run whose canonical bytes cannot be produced")
	}
	if len(res.Notes) == 0 {
		t.Error("the receipt disclosed nothing and said nothing about why")
	}
}

// TestReceiptForAnUngatedRunDisclosesNoDecision pins that a run nobody had to approve still
// receipts cleanly. Most runs are not gated, so a builder that treated a missing decision as a
// fault would refuse the common case, and one that reported a decision anyway would put an approval
// on the record that never happened.
func TestReceiptForAnUngatedRunDisclosesNoDecision(t *testing.T) {
	ctx := context.Background()
	runs, audits, id, subject, _ := busyInstall(t)

	res, err := receipt.Build(ctx, runs, audits, id, "test", subject.ID, receipt.Options{Sparse: true})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	rep, err := audit.VerifyBundle(res.Signed, id.KeyID())
	if err != nil {
		t.Fatalf("VerifyBundle: %v", err)
	}
	if !rep.OK() {
		t.Fatalf("an ungated run's receipt does not verify: %+v", rep)
	}
	if rep.DecisionsPresent != 0 {
		t.Errorf("decisions = %+v, want none on a run that was never gated", rep.Decisions)
	}
	if !rep.DecisionsOK {
		t.Error("a receipt with no decisions was judged as having a bad one")
	}
}

// TestReceiptResultDescribesWhatItProduced pins the fields a caller reports to an operator and
// writes to a file. The trailing newline matters because both the command and the HTTP handler
// write these bytes straight out, and the key id matters because it is what a verifier pins; a
// result that named a different key would send a reader to check the wrong producer.
func TestReceiptResultDescribesWhatItProduced(t *testing.T) {
	ctx := context.Background()
	runs, audits, id, r := held(t, "approved")

	res, err := receipt.Build(ctx, runs, audits, id, "test", r.ID, receipt.Options{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if diff := cmp.Diff(id.KeyID(), res.KeyID); diff != "" {
		t.Errorf("KeyID mismatch (-want +got):\n%s", diff)
	}
	if len(res.Signed) == 0 || res.Signed[len(res.Signed)-1] != '\n' {
		t.Error("the signed receipt does not end with a newline, so writing it produces a file " +
			"without a final line break")
	}
	if res.Claims <= 0 {
		t.Errorf("Claims = %d, want the entries the receipt carries", res.Claims)
	}
	if strings.Count(string(res.Signed), "\n") != 1 {
		t.Error("the signed receipt is not one line, so it cannot be appended to a stream of them")
	}
}

// TestReceiptBuildsConcurrentlyForTheSameRun pins that the builder is safe to call from many
// requests at once, which is what the endpoint serving receipts does. Under the race detector this
// proves the shared chain read and the shared identity are not being mutated, and the equal claim
// counts prove two concurrent builds see the same chain rather than interleaving each other's.
func TestReceiptBuildsConcurrentlyForTheSameRun(t *testing.T) {
	ctx := context.Background()
	runs, audits, id, r := held(t, "approved")

	const callers = 16
	type result struct {
		claims int
		err    error
	}
	out := make([]result, callers)
	var wg sync.WaitGroup
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			opts := receipt.Options{}
			if i%2 == 1 {
				opts.Sparse = true
			}
			res, err := receipt.Build(ctx, runs, audits, id, "test", r.ID, opts)
			if err != nil {
				out[i] = result{err: err}
				return
			}
			out[i] = result{claims: res.Claims}
		}()
	}
	wg.Wait()

	var wantContiguous, wantSparse int
	for i, got := range out {
		if got.err != nil {
			t.Fatalf("caller %d: %v", i, got.err)
		}
		if i%2 == 1 {
			if wantSparse == 0 {
				wantSparse = got.claims
			}
			if got.claims != wantSparse {
				t.Errorf("caller %d built %d sparse claims, want %d", i, got.claims, wantSparse)
			}
			continue
		}
		if wantContiguous == 0 {
			wantContiguous = got.claims
		}
		if got.claims != wantContiguous {
			t.Errorf("caller %d built %d claims, want %d", i, got.claims, wantContiguous)
		}
	}
}

// TestReceiptOnABusyInstallAttachesTheOutcomeToItsOwnEntry pins that a run's disclosed outcome is
// attached to that run's own chain entry, on an install where another run finished in between. This
// is the ordinary case for a fleet control plane: any second run that completes between this run's
// request and its outcome puts another RUN entry inside the segment the contiguous receipt covers.
//
// It is skipped because it currently fails. The builder picks the first claim whose path merely
// contains "/outcome/", so this run's outcome body, nonce, and redacted spec are attached to the
// other run's entry. The verifier then checks this run's body against that entry's committed digest
// and reports the disclosure as not matching the chain, which is the flagship evidence artifact
// accusing itself of tampering.
func TestReceiptOnABusyInstallAttachesTheOutcomeToItsOwnEntry(t *testing.T) {
	ctx := context.Background()
	runs, audits, id, subject, other := busyInstall(t)

	res, err := receipt.Build(ctx, runs, audits, id, "test", subject.ID, receipt.Options{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	rep, err := audit.VerifyBundle(res.Signed, id.KeyID())
	if err != nil {
		t.Fatalf("VerifyBundle: %v", err)
	}
	if !rep.OK() {
		t.Fatalf("a receipt for a run on a busy install does not verify: %+v", rep)
	}
	if !rep.OutcomePresent || !rep.OutcomeDigestOK {
		t.Errorf("the outcome is not disclosed against the digest its own entry committed: "+
			"present=%v ok=%v", rep.OutcomePresent, rep.OutcomeDigestOK)
	}
	if !strings.Contains(string(rep.OutcomeBody), subject.ID) {
		t.Errorf("the disclosed outcome is not this run's:\n%s", rep.OutcomeBody)
	}
	// The other run's entry must carry no disclosure of its own, since this receipt is not about it.
	if strings.Contains(string(res.Signed),
		`"path":"/runs/`+other.ID+`/outcome/succeeded","spec_body"`) {
		t.Error("this run's redacted spec was attached to another run's chain entry")
	}
}

// TestSparseReceiptDoesNotDiscloseARunWhoseIDMerelyContainsThisOne pins the boundary of the sparse
// shape's entry selection. Entries are chosen by whether the run's identifier appears anywhere in
// the path, so a second run whose identifier contains this one's is disclosed too, and the whole
// point of the sparse shape is that nothing about other runs travels.
//
// It is skipped because it currently fails. Minted run identifiers are a fixed width today, so no
// real identifier can contain another, which is what keeps this latent rather than live. Nothing in
// the builder relies on that, and an identifier arriving from an import, a migration, or a future
// format is enough to turn it live.
func TestSparseReceiptDoesNotDiscloseARunWhoseIDMerelyContainsThisOne(t *testing.T) {
	ctx := context.Background()
	runs, audits, id, r := held(t, "approved")

	// The fixture's run is run_1. A second run named run_12 records a decision of its own, whose
	// path contains run_1 as a substring.
	neighbor := &run.Run{
		ID: r.ID + "2", Status: run.StatusSucceeded, CreatedAt: time.Now(),
		Tool: run.ToolBash, Command: "someone else's change", Actor: "stranger", ActorType: "session",
	}
	if err := runs.Save(ctx, neighbor); err != nil {
		t.Fatalf("save neighbor: %v", err)
	}
	_, err := outcome.CommitDecision(ctx, audits, neighbor, "approved", "stranger", "session")
	if err != nil {
		t.Fatalf("CommitDecision for the neighbor: %v", err)
	}

	res, err := receipt.Build(ctx, runs, audits, id, "test", r.ID, receipt.Options{Sparse: true})
	if err != nil {
		t.Fatalf("Build sparse: %v", err)
	}
	if strings.Contains(string(res.Signed), neighbor.ID) {
		t.Errorf("the sparse receipt discloses run %s, whose id merely contains %s", neighbor.ID, r.ID)
	}
	if strings.Contains(string(res.Signed), "stranger") {
		t.Error("the sparse receipt names an actor from another run")
	}
}

// TestReceiptOmitsADecisionRecordedAfterTheOutcome pins that the disclosure only attaches to claims
// the receipt actually carries. The contiguous receipt covers the segment from the run's creation
// through the entry recording what it did, so a decision entry written after that outcome is
// outside the document. Attaching its body to some other claim, rather than skipping it, would give
// the receipt a disclosure whose committed digest belongs to a different entry.
func TestReceiptOmitsADecisionRecordedAfterTheOutcome(t *testing.T) {
	ctx := context.Background()
	runs, audits, id, r := held(t, "approved")

	// A second decision lands on the chain after the run already finished.
	if _, err := outcome.CommitDecision(ctx, audits, r, "rejected", "dana", "session"); err != nil {
		t.Fatalf("CommitDecision after the outcome: %v", err)
	}

	res, err := receipt.Build(ctx, runs, audits, id, "test", r.ID, receipt.Options{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	rep, err := audit.VerifyBundle(res.Signed, id.KeyID())
	if err != nil {
		t.Fatalf("VerifyBundle: %v", err)
	}
	if !rep.OK() {
		t.Fatalf("the receipt does not verify: %+v", rep)
	}
	if rep.DecisionsPresent != 1 {
		t.Fatalf("decisions = %+v, want only the one inside the receipted segment", rep.Decisions)
	}
	if rep.Decisions[0].Verdict != "approved" {
		t.Errorf("decision = %+v, want the approval the segment covers", rep.Decisions[0])
	}
}
