package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/run"
)

// receiptChain is a seeded install: an intact chain holding one receiptable run whose own entries
// sit in the middle of it, so a tamper can be placed before that segment as well as after it. The
// middle is what matters. A run whose entries opened or closed the chain could not tell a check
// over the whole chain apart from one over the run's own segment, the difference under test here.
type receiptChain struct {
	// Runs holds the receiptable run.
	Runs run.Store
	// Audits is the intact chain every tamper is applied over.
	Audits audit.Store
	// RunID names the receiptable run.
	RunID string
	// CreationSeq and OutcomeSeq bound the run's own segment of the chain.
	CreationSeq int64
	OutcomeSeq  int64
	// Entries is how many entries the seeded chain holds.
	Entries int
}

// seedReceiptChain returns a run that can be receipted, on a five-entry chain that also holds work
// this run had nothing to do with. Times carry no nanoseconds, because the bundle builder refuses
// an entry recorded at nanosecond precision and that refusal would stand in for the break a test is
// about.
func seedReceiptChain(t *testing.T) receiptChain {
	t.Helper()
	ctx := context.Background()
	runs, audits := run.NewMemStore(), audit.NewMemStore()
	at := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)

	filler := func(minute int) {
		if err := audits.Append(ctx, &audit.Entry{
			ID: audit.NewID(), At: at.Add(time.Duration(minute) * time.Minute),
			Actor: "alice", ActorType: "session", Method: http.MethodPost, Path: "/v1/runs",
		}); err != nil {
			t.Fatalf("Append(filler at minute %d) error = %v", minute, err)
		}
	}

	filler(0)
	creation := &audit.Entry{
		ID: audit.NewID(), At: at.Add(time.Minute), Actor: "operator-jane", ActorType: "session",
		Method: http.MethodPost, Path: "/v1/runs",
	}
	if err := audits.Append(ctx, creation); err != nil {
		t.Fatalf("Append(creation) error = %v", err)
	}
	r := &run.Run{
		ID: "run_receipted", Playbook: "site.yml", Inventory: "prod", Status: run.StatusRunning,
		Actor: "operator-jane", ActorType: "session", AuditReceipt: audit.Receipt(creation),
		CreatedAt: at, StartedAt: &at,
	}
	if err := runs.Save(ctx, r); err != nil {
		t.Fatalf("Save(running) error = %v", err)
	}
	filler(2)

	ended := at.Add(3 * time.Minute)
	r.Status, r.EndedAt = run.StatusSucceeded, &ended
	if err := runs.Save(ctx, r); err != nil {
		t.Fatalf("Save(succeeded) error = %v", err)
	}
	commitAt := func() time.Time { return ended }
	if err := outcome.Commit(ctx, audits, runs, r, "system:dispatcher", commitAt); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	filler(4)

	return receiptChain{
		Runs: runs, Audits: audits, RunID: r.ID, CreationSeq: 2, OutcomeSeq: 4, Entries: 5,
	}
}

// receiptHandlerOver returns the receipt endpoint over the given stores, reachable at its real path
// so the run identifier arrives the way the router supplies it.
func receiptHandlerOver(t *testing.T, runs run.Store, audits audit.Store,
	id *audit.Identity) http.Handler {
	t.Helper()
	return New(runs, &fakeSubmitter{run: &run.Run{ID: "run_x"}}, zap.NewNop(),
		WithAudit(audits), WithProducerIdentity(id, "v-test")).Handler()
}

// TestRunReceiptHandlerRefusesAChainThatDoesNotVerify pins what the receipt endpoint answers on an
// intact chain and on a tampered one.
//
// The contiguous shape showed its builder only the segment between the run's creation and its
// outcome, so a break anywhere outside that segment was never looked at: the endpoint answered 200
// with a signed receipt over entries genuinely past the break, while GET /v1/audit/verify was
// reporting the chain broken at a position and GET /v1/audit/bundle was refusing to publish it. The
// disclosed entries really were intact, which is what made it dangerous: the offline verifier reads
// such a file and reports that nothing has been altered, so the caveat had to be somewhere the
// relying party could not fail to see it, and the only place left was not signing at all.
//
// The sparse shape did refuse, because its builder hashes the whole chain into a tree and checks
// it, but it refused with a bare error string carrying the sentinel that names the operation rather
// than the problem, and with none of the coordinates the sibling endpoints report.
func TestRunReceiptHandlerRefusesAChainThatDoesNotVerify(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Edit is the tamper applied to the intact chain. Nil leaves the chain intact.
		Edit chainEdit
		// Query is the query string on the request, which chooses the receipt's shape.
		Query string
		// WantErrorHas is text the refusal message must carry.
		WantErrorHas []string
		// WantErrorLacks is text the refusal message must not carry.
		WantErrorLacks []string
		// Want is the refusal body, with Error compared through WantErrorHas instead. It is read
		// only when WantStatus is 409.
		Want bundleRefusal
		// WantStatus is the status the request must answer with.
		WantStatus int
		// WantOutcome requires the served receipt to disclose an outcome the chain committed, which
		// only the contiguous shape carries.
		WantOutcome bool
	}{{ // Test 0: The control. An intact chain still signs the contiguous shape, outcome and all.
		WantStatus: http.StatusOK, WantOutcome: true,
	}, { // Test 1: The control for the other shape. An intact chain still signs a sparse receipt.
		Query: "?sparse=1", WantStatus: http.StatusOK,
	}, { // Test 2: A break before the run's own entries. The receipt's claims are entirely past it,
		// so every disclosure in it is sound and the endpoint signed one anyway.
		Edit: func(e *audit.Entry) *audit.Entry {
			if e.Seq == 1 {
				e.Actor = "mallory"
			}
			return e
		},
		WantStatus: http.StatusConflict,
		Want: bundleRefusal{
			Reason: reasonChainBreak, BrokeAt: 1, BrokeSeq: 1, Count: 5,
		},
		WantErrorHas: []string{
			"entry 1 of 5", "sequence 1", "a receipt", "not a fault in this server",
			"/v1/audit/verify",
		},
		WantErrorLacks: []string{"audit export"},
	}, { // Test 3: A break after the run's own entries, which the segment walk equally never saw.
		Edit: func(e *audit.Entry) *audit.Entry {
			if e.Seq == 5 {
				e.Path = "/v1/runs/deleted"
			}
			return e
		},
		WantStatus: http.StatusConflict,
		Want: bundleRefusal{
			Reason: reasonChainBreak, BrokeAt: 5, BrokeSeq: 5, Count: 5,
		},
		WantErrorHas: []string{"entry 5 of 5", "sequence 5"},
	}, { // Test 4: A break inside the run's segment, on an entry belonging to other work. This one
		// the builder always caught, so it pins that the new walk did not move the coordinate.
		Edit: func(e *audit.Entry) *audit.Entry {
			if e.Seq == 3 {
				e.Actor = "mallory"
			}
			return e
		},
		WantStatus: http.StatusConflict,
		Want: bundleRefusal{
			Reason: reasonChainBreak, BrokeAt: 3, BrokeSeq: 3, Count: 5,
		},
		WantErrorHas: []string{"entry 3 of 5", "sequence 3"},
	}, { // Test 5: The same break under the sparse shape, which refused before but said only that a
		// bundle could not be built, with no reason code and no coordinate to act on.
		Query: "?sparse=1",
		Edit: func(e *audit.Entry) *audit.Entry {
			if e.Seq == 1 {
				e.Actor = "mallory"
			}
			return e
		},
		WantStatus: http.StatusConflict,
		Want: bundleRefusal{
			Reason: reasonChainBreak, BrokeAt: 1, BrokeSeq: 1, Count: 5,
		},
		WantErrorHas:   []string{"entry 1 of 5", "sequence 1"},
		WantErrorLacks: []string{"audit export"},
	}, { // Test 6: An entry is deleted, so the entry after it links to something no longer there and
		// the break is named at the entry that lost its link.
		Edit: func(e *audit.Entry) *audit.Entry {
			if e.Seq == 1 {
				return nil
			}
			return e
		},
		WantStatus: http.StatusConflict,
		Want: bundleRefusal{
			Reason: reasonChainBreak, BrokeAt: 1, BrokeSeq: 2, Count: 4,
		},
		WantErrorHas: []string{"entry 1 of 4", "sequence 2", "altered, reordered, or removed"},
	}, { // Test 7: A blanked hash, which is the tamper that leaves nothing to recompute.
		Edit: func(e *audit.Entry) *audit.Entry {
			if e.Seq == 5 {
				e.Hash = ""
			}
			return e
		},
		WantStatus: http.StatusConflict,
		Want: bundleRefusal{
			Reason: reasonChainBreak, BrokeAt: 5, BrokeSeq: 5, Count: 5,
		},
		WantErrorHas: []string{"entry 5 of 5"},
	}, { // Test 8: A blanked sequence, so the break has no coordinate in the trail to name and the
		// refusal must not print one anyway.
		Edit: func(e *audit.Entry) *audit.Entry {
			if e.Seq == 1 {
				e.Seq = 0
			}
			return e
		},
		WantStatus: http.StatusConflict,
		Want: bundleRefusal{
			Reason: reasonChainBreak, BrokeAt: 1, Count: 5,
		},
		WantErrorHas:   []string{"entry 1 of 5"},
		WantErrorLacks: []string{"sequence"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			id, err := audit.LoadIdentity(t.TempDir())
			if err != nil {
				t.Fatalf("LoadIdentity() error = %v", err)
			}
			seed := seedReceiptChain(t)
			audits := seed.Audits
			if test.Edit != nil {
				audits = newEditedChain(t, seed.Audits, test.Edit)
			}
			rec := httptest.NewRecorder()
			receiptHandlerOver(t, seed.Runs, audits, &id).ServeHTTP(rec,
				httptest.NewRequest(http.MethodGet, "/v1/runs/"+seed.RunID+"/receipt"+test.Query, nil))

			if rec.Code != test.WantStatus {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, test.WantStatus, rec.Body.String())
			}
			if test.WantStatus == http.StatusOK {
				report, verr := audit.VerifyBundle(rec.Body.Bytes(), id.KeyID())
				if verr != nil {
					t.Fatalf("VerifyBundle() error = %v", verr)
				}
				if !report.OK() {
					t.Errorf("the served receipt does not verify: %+v", report)
				}
				if test.WantOutcome && (!report.OutcomePresent || !report.OutcomeDigestOK) {
					t.Errorf("the served receipt does not disclose a verified outcome: %+v", report)
				}
				return
			}
			var got bundleRefusal
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode refusal: %v (body %s)", err, rec.Body.String())
			}
			message := got.Error
			got.Error = ""
			if diff := cmp.Diff(test.Want, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("refusal mismatch (-want +got):\n%s", diff)
			}
			for _, want := range test.WantErrorHas {
				if !strings.Contains(message, want) {
					t.Errorf("refusal message %q does not name %q", message, want)
				}
			}
			for _, unwanted := range test.WantErrorLacks {
				if strings.Contains(message, unwanted) {
					t.Errorf("refusal message %q still carries %q", message, unwanted)
				}
			}
		})
	}
}

// TestRunReceiptHandlerAgreesWithTheSiblingEndpoints pins that one tampered chain produces the same
// coordinates from all three answers. The receipt's refusal points an operator at GET
// /v1/audit/verify, so the two disagreeing about where the break is would send them looking in the
// wrong place, and a bundle that refuses beside a receipt that signs is the state this change
// exists to end.
func TestRunReceiptHandlerAgreesWithTheSiblingEndpoints(t *testing.T) {
	t.Parallel()
	id, err := audit.LoadIdentity(t.TempDir())
	if err != nil {
		t.Fatalf("LoadIdentity() error = %v", err)
	}
	seed := seedReceiptChain(t)
	tampered := newEditedChain(t, seed.Audits, func(e *audit.Entry) *audit.Entry {
		if e.Seq == 1 {
			e.Actor = "mallory"
		}
		return e
	})

	verify := httptest.NewRecorder()
	auditVerifyHandler(tampered, id.InstallID, zap.NewNop()).
		ServeHTTP(verify, httptest.NewRequest(http.MethodGet, "/v1/audit/verify", nil))
	var report auditVerifyResponse
	if err := json.Unmarshal(verify.Body.Bytes(), &report); err != nil {
		t.Fatalf("decode verify report: %v (body %s)", err, verify.Body.String())
	}
	if report.OK {
		t.Fatalf("the seeded tamper did not break the chain: %+v", report)
	}

	bundle := httptest.NewRecorder()
	auditBundleHandler(tampered, &id, "v-test", zap.NewNop()).
		ServeHTTP(bundle, httptest.NewRequest(http.MethodGet, "/v1/audit/bundle", nil))
	var bundleSays bundleRefusal
	if err := json.Unmarshal(bundle.Body.Bytes(), &bundleSays); err != nil {
		t.Fatalf("decode bundle refusal: %v (body %s)", err, bundle.Body.String())
	}

	receipt := httptest.NewRecorder()
	receiptHandlerOver(t, seed.Runs, tampered, &id).ServeHTTP(receipt,
		httptest.NewRequest(http.MethodGet, "/v1/runs/"+seed.RunID+"/receipt", nil))
	if receipt.Code != http.StatusConflict {
		t.Fatalf("receipt status = %d, want 409; the bundle refused the same chain (body %s)",
			receipt.Code, receipt.Body.String())
	}
	var receiptSays bundleRefusal
	if err := json.Unmarshal(receipt.Body.Bytes(), &receiptSays); err != nil {
		t.Fatalf("decode receipt refusal: %v (body %s)", err, receipt.Body.String())
	}

	if receiptSays.BrokeAt != report.BrokeAt || receiptSays.Count != report.Count {
		t.Errorf("receipt says broke_at %d of %d, verify says %d of %d",
			receiptSays.BrokeAt, receiptSays.Count, report.BrokeAt, report.Count)
	}
	want := bundleSays
	want.Error = ""
	got := receiptSays
	got.Error = ""
	if diff := cmp.Diff(want, got, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("the receipt and the bundle disagree about the same chain (-bundle +receipt):\n%s", diff)
	}
}

// TestRunReceiptHandlerNamesTheAnchorItCannotSatisfy pins the refusal for a chain that
// hash-verifies but has lost its tail. A prefix of a valid chain is itself a valid chain, so the
// link walk passes and only the anchor catches it. The builder already refused this, but as a bare
// error string with no reason code and no named anchor, so a caller could not tell it from a run
// that simply had nothing to attest yet. The bundle has always named those problems.
func TestRunReceiptHandlerNamesTheAnchorItCannotSatisfy(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	id, err := audit.LoadIdentity(t.TempDir())
	if err != nil {
		t.Fatalf("LoadIdentity() error = %v", err)
	}
	seed := seedReceiptChain(t)
	chain, err := seed.Audits.Chain(ctx)
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	head := chain[len(chain)-1]
	anchors, ok := seed.Audits.(audit.AnchorStore)
	if !ok {
		t.Fatal("the seeded store keeps no anchors")
	}
	if err := anchors.SaveAnchor(ctx, &audit.Anchor{
		ID: audit.NewAnchorID(), Type: audit.AnchorHTTPS, Shape: audit.AnchorShapeLinear,
		Seq: head.Seq, Link: head.Hash, At: time.Now().UTC(), Ref: "https://anchors.example/head",
	}); err != nil {
		t.Fatalf("SaveAnchor() error = %v", err)
	}
	// The tail is cut back below the anchor. What remains verifies perfectly on its own, and still
	// holds every entry this run's receipt would disclose.
	cut := newEditedChain(t, seed.Audits, func(e *audit.Entry) *audit.Entry {
		if e.Seq >= head.Seq {
			return nil
		}
		return e
	})

	rec := httptest.NewRecorder()
	receiptHandlerOver(t, seed.Runs, cut, &id).ServeHTTP(rec,
		httptest.NewRequest(http.MethodGet, "/v1/runs/"+seed.RunID+"/receipt", nil))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body %s)", rec.Code, rec.Body.String())
	}
	var got bundleRefusal
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode refusal: %v (body %s)", err, rec.Body.String())
	}
	if got.Reason != reasonAnchorUnsatisfied {
		t.Errorf("reason = %q, want %q (body %s)", got.Reason, reasonAnchorUnsatisfied, rec.Body.String())
	}
	if len(got.AnchorProblems) == 0 {
		t.Errorf("the refusal names no anchor problem, so it says an anchor failed without saying "+
			"which: %s", rec.Body.String())
	}
}

// nanosecondCopy rehashes a chain at nanosecond precision, keeping every entry's actor, method,
// path, and committed digest, so every link recomputes and the walk passes while a verifier
// carrying time at microsecond precision would recompute different ones. Rebuilding the seeded
// chain rather than substituting a generic one is what keeps the run's own creation and outcome
// entries in it, which the receipt needs before the builder can get as far as refusing.
func nanosecondCopy(t *testing.T, chain []*audit.Entry) []*audit.Entry {
	t.Helper()
	out := make([]*audit.Entry, 0, len(chain))
	prev := ""
	for _, e := range chain {
		cp := *e
		cp.At = e.At.Add(500 * time.Nanosecond)
		cp.PrevHash = prev
		cp.Hash = audit.EntryHash(&cp)
		prev = cp.Hash
		out = append(out, &cp)
	}
	return out
}

// TestRunReceiptHandlerReportsAChainItCannotReceipt pins the refusal for a chain that verifies but
// cannot be published: the builder's own words reach the caller under a reason code of their own,
// rather than as an error string opening with the sentinel that names the operation instead of the
// problem. It is the one refusal that still reaches the caller through the builder rather than
// through the walk, so it is what keeps that path speaking the same vocabulary as the rest.
func TestRunReceiptHandlerReportsAChainItCannotReceipt(t *testing.T) {
	t.Parallel()
	id, err := audit.LoadIdentity(t.TempDir())
	if err != nil {
		t.Fatalf("LoadIdentity() error = %v", err)
	}
	seed := seedReceiptChain(t)
	chain, err := seed.Audits.Chain(context.Background())
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	nano := nanosecondCopy(t, chain)
	store := newEditedChain(t, seed.Audits, func(e *audit.Entry) *audit.Entry {
		return nano[e.Seq-1]
	})

	rec := httptest.NewRecorder()
	receiptHandlerOver(t, seed.Runs, store, &id).ServeHTTP(rec,
		httptest.NewRequest(http.MethodGet, "/v1/runs/"+seed.RunID+"/receipt", nil))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body %s)", rec.Code, rec.Body.String())
	}
	var got bundleRefusal
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode refusal: %v (body %s)", err, rec.Body.String())
	}
	if got.Reason != reasonChainUnbundlable {
		t.Errorf("reason = %q, want %q (body %s)", got.Reason, reasonChainUnbundlable, rec.Body.String())
	}
	// The walk counted the chain before the builder refused it, so the refusal reports the same
	// number of entries the bundle download reports for the same chain. Leaving it zero said the
	// chain held nothing, which is the one thing this refusal knows to be untrue.
	if got.Count != seed.Entries {
		t.Errorf("count = %d, want %d (body %s)", got.Count, seed.Entries, rec.Body.String())
	}
	if strings.HasPrefix(got.Error, audit.ErrExport.Error()) {
		t.Errorf("refusal message %q still opens with the sentinel, which names the operation "+
			"rather than the problem", got.Error)
	}
}
