package receipt

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/run"
)

// finishedRun builds the smallest receiptable thing there is: one creation entry, one run, one
// committed outcome. It exists inside the package so a test can reach the entry ceiling and the two
// bundle builders directly, which an external test cannot.
func finishedRun(t *testing.T) (run.Store, audit.Store, audit.Identity, *run.Run) {
	t.Helper()
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
	ended := time.Now()
	code := 0
	r := &run.Run{
		ID: "run_ceiling", Status: run.StatusSucceeded, CreatedAt: time.Now(), EndedAt: &ended,
		ExitCode: &code, Tool: run.ToolBash, Command: "deploy the thing", Actor: "casey",
		ActorType: "session", AuditReceipt: audit.Receipt(creation),
	}
	if err := runs.Save(ctx, r); err != nil {
		t.Fatalf("save run: %v", err)
	}
	if err := outcome.Commit(ctx, audits, runs, r, "system:test", nil); err != nil {
		t.Fatalf("Commit outcome: %v", err)
	}
	return runs, audits, id, r
}

// TestParseReceipt pins what a stored seq:link parses to, on both sides. The successful parse is
// what places a run on the chain, so the position and the link it returns are checked as values and
// not only as the absence of an error; a parse that returned the wrong sequence would build the
// receipt over a different segment of history and sign it.
func TestParseReceipt(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name says which shape this is.
		Name string
		// In is the stored receipt value.
		In string
		// WantSeq and WantLink are what a successful parse must return.
		WantSeq  int64
		WantLink string
		// WantMessage is a phrase a refusal must carry, empty when the parse succeeds.
		WantMessage string
	}{{ // Test 0: The ordinary form.
		Name: "ordinary", In: "41:9f2c", WantSeq: 41, WantLink: "9f2c",
	}, { // Test 1: The first entry a chain assigns.
		Name: "first entry", In: "1:00", WantSeq: 1, WantLink: "00",
	}, { // Test 2: A link containing the separator, which belongs to the link and not the position.
		Name: "link with colon", In: "7:ab:cd", WantSeq: 7, WantLink: "ab:cd",
	}, { // Test 3: The largest position an int64 holds.
		Name: "max position", In: "9223372036854775807:ff", WantSeq: 9223372036854775807,
		WantLink: "ff",
	}, { // Test 4: Empty, which is handled before this is reached but must still refuse.
		Name: "empty", In: "", WantMessage: "looks like seq:link",
	}, { // Test 5: No separator.
		Name: "no separator", In: "41", WantMessage: "looks like seq:link",
	}, { // Test 6: A position one past the top of an int64.
		Name: "overflow", In: "9223372036854775808:ff", WantMessage: "not a chain position",
	}, { // Test 7: Zero, one below the first position a chain assigns.
		Name: "zero", In: "0:ff", WantMessage: "not a chain position",
	}, { // Test 8: A hex position, which looks plausible and is not decimal.
		Name: "hex position", In: "0x29:ff", WantMessage: "not a chain position",
	}, { // Test 9: A position with no link.
		Name: "no link", In: "41:", WantMessage: "missing its link",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			seq, link, err := parseReceipt(test.In)
			if test.WantMessage != "" {
				if err == nil {
					t.Fatalf("%s: parseReceipt(%q) = (%d, %q), want a refusal",
						test.Name, test.In, seq, link)
				}
				if !strings.Contains(err.Error(), test.WantMessage) {
					t.Errorf("%s: error = %v, want it to mention %q", test.Name, err, test.WantMessage)
				}
				if seq != 0 || link != "" {
					t.Errorf("%s: a refusal still returned (%d, %q)", test.Name, seq, link)
				}
				return
			}
			if err != nil {
				t.Fatalf("%s: parseReceipt(%q): %v", test.Name, test.In, err)
			}
			if diff := cmp.Diff(test.WantSeq, seq); diff != "" {
				t.Errorf("%s: sequence mismatch (-want +got):\n%s", test.Name, diff)
			}
			if diff := cmp.Diff(test.WantLink, link); diff != "" {
				t.Errorf("%s: link mismatch (-want +got):\n%s", test.Name, diff)
			}
		})
	}
}

// TestBuildRefusesPastTheEntryCeiling pins the bound on how much one receipt will assemble, and
// pins it on both sides of the boundary. The endpoint that serves receipts is reachable below
// admin and a receipt is one signed document, so it cannot be streamed: without this bound a single
// request on an install with a long chain assembles the whole history in memory, which is a denial
// of service one loop away. Refusing has to mean refusing, not truncating, because a receipt that
// quietly dropped entries would be a signed document with a hole in it.
//
// It is not parallel because it shrinks the package's own ceiling.
func TestBuildRefusesPastTheEntryCeiling(t *testing.T) {
	ctx := context.Background()
	runs, audits, id, r := finishedRun(t)

	chain, err := audits.Chain(ctx)
	if err != nil {
		t.Fatalf("Chain: %v", err)
	}
	collected := len(chain)

	original := maxReceiptEntries
	t.Cleanup(func() { maxReceiptEntries = original })

	// Test 0: A ceiling exactly as large as the segment still builds, so the refusal is off by
	// nothing.
	maxReceiptEntries = collected
	if _, err := Build(ctx, runs, audits, id, "test", r.ID, Options{}); err != nil {
		t.Fatalf("a ceiling of %d refused a receipt carrying exactly %d entries: %v",
			collected, collected, err)
	}

	// Test 1: One below refuses, and says what to do instead.
	maxReceiptEntries = collected - 1
	res, err := Build(ctx, runs, audits, id, "test", r.ID, Options{})
	if err == nil {
		t.Fatalf("a ceiling of %d assembled a receipt of %d entries anyway: %+v",
			collected-1, collected, res)
	}
	if res != nil {
		t.Error("a refusal still returned a result")
	}
	if !strings.Contains(err.Error(), "bundle command") {
		t.Errorf("error = %v, want it to name the windowed export an operator should use instead", err)
	}

	// Test 2: The sparse shape reads the whole chain, so it is bounded the same way.
	if _, err := Build(ctx, runs, audits, id, "test", r.ID, Options{Sparse: true}); err == nil {
		t.Error("the sparse shape assembled past the ceiling")
	}
}

// TestBundleBuildersRefuseAChainThatDoesNotVerify pins that neither receipt shape will build over a
// segment whose links do not hold together. A tree built over a broken chain still verifies against
// its own root, and a contiguous bundle built from a reordered segment is rejected by an outside
// auditor rather than here, which is worse: by then the claim about this install has already been
// handed over.
func TestBundleBuildersRefuseAChainThatDoesNotVerify(t *testing.T) {
	t.Parallel()
	// Two entries whose links name nothing, which is what a reordered or rewritten chain looks like.
	broken := []*audit.Entry{
		{ID: "aud_1", At: time.Now(), Actor: "casey", Method: "POST", Path: "/v1/runs",
			Seq: 1, PrevHash: "", Hash: "1111"},
		{ID: "aud_2", At: time.Now(), Actor: "casey", Method: audit.MethodRun,
			Path: "/runs/run_x/outcome/succeeded", Seq: 2, PrevHash: "9999", Hash: "2222"},
	}
	subject := audit.BundleSubject{Type: "run", ID: "run_x"}
	var id audit.Identity

	if doc, err := rangeBundle(broken, 1, 2, id, "test", subject); err == nil {
		t.Errorf("a contiguous receipt was built over a chain that does not verify: %+v", doc)
	}
	if doc, err := sparseBundle(broken, "run_x", 1, 2, id, "test", subject, 0); err == nil {
		t.Errorf("a sparse receipt was built over a chain that does not verify: %+v", doc)
	}
}

// TestClaimLookupsReturnNilWhenTheClaimIsWithheld pins that both claim lookups answer nil rather
// than the wrong claim when the bundle does not hold the entry being asked about. A lookup that
// fell back to some other claim would attach one entry's disclosed body to another entry's
// committed digest, which reads to a verifier as a receipt that fails its own check.
func TestClaimLookupsReturnNilWhenTheClaimIsWithheld(t *testing.T) {
	t.Parallel()
	doc := &audit.Bundle{Claims: []audit.BundleClaim{
		{Payload: map[string]any{"method": "POST", "path": "/v1/runs"}},
		{Payload: map[string]any{"method": audit.MethodDecision,
			"path": "/runs/run_other/decision/approved"}},
		// A payload with no method or path at all, the shape a claim decoded from a foreign
		// document can take.
		{Payload: map[string]any{}},
	}}

	if got := outcomeClaim(doc, "/runs/run_mine/outcome/succeeded"); got != nil {
		t.Errorf("outcomeClaim found %+v in a bundle that discloses no outcome", got)
	}
	if got := claimFor(doc, audit.MethodDecision, "/runs/run_mine/decision/approved"); got != nil {
		t.Errorf("claimFor found %+v for a decision the bundle withholds", got)
	}
	// The one it should find, so the nil answers above are not simply a lookup that never matches.
	if got := claimFor(doc, audit.MethodDecision, "/runs/run_other/decision/approved"); got == nil {
		t.Error("claimFor missed the decision claim the bundle does hold")
	}
	if got := outcomeClaim(&audit.Bundle{}, "/runs/run_mine/outcome/succeeded"); got != nil {
		t.Errorf("outcomeClaim found %+v in an empty bundle", got)
	}
}
