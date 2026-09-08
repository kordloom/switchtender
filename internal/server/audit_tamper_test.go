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
)

// seedAuditChain returns a store holding an intact chain of n entries at whole-second times, the
// clean starting point every tamper in this file is applied to. The times carry no nanoseconds
// because the bundle builder refuses an entry recorded at nanosecond precision, and that refusal
// would stand in for the break a test is actually about.
func seedAuditChain(t *testing.T, n int) audit.Store {
	t.Helper()
	audits := audit.NewMemStore()
	for i := range n {
		if err := audits.Append(context.Background(), &audit.Entry{
			ID: audit.NewID(), At: time.Date(2026, 9, 8, 12, i, 0, 0, time.UTC),
			Actor: "alice", Method: "POST", Path: "/v1/runs",
		}); err != nil {
			t.Fatalf("Append() error = %v", err)
		}
	}
	return audits
}

// chainEdit rewrites one entry as the chain streams past, and returns nil to drop it. It is how a
// test stages the edit an attacker makes with a SQLite client under a running server: the stored
// row changes, the hash it committed to does not.
type chainEdit func(e *audit.Entry) *audit.Entry

// editedChain applies a chainEdit to every entry a scan of the store underneath yields.
type editedChain struct {
	// Store is the intact chain the edit is applied over.
	audit.Store
	// AnchorStore is the same store's anchor half, carried across so a handler that reaches for it
	// by type assertion still finds the anchors. Without it a wrapped store looks like an install
	// that has never anchored, which is the one condition anchoring exists to distinguish.
	audit.AnchorStore
	// edit rewrites or drops each entry as it streams.
	edit chainEdit
}

// newEditedChain wraps store so every scan streams through edit, keeping its anchors reachable.
func newEditedChain(t *testing.T, store audit.Store, edit chainEdit) *editedChain {
	t.Helper()
	anchors, ok := store.(audit.AnchorStore)
	if !ok {
		t.Fatal("the store under test keeps no anchors")
	}
	return &editedChain{Store: store, AnchorStore: anchors, edit: edit}
}

// ChainScan streams the underlying chain with the edit applied to every entry. Each entry is copied
// first so an edit cannot corrupt the store a later scan reads.
func (c *editedChain) ChainScan(ctx context.Context, afterSeq int64, fn func(*audit.Entry) error) error {
	return c.Store.ChainScan(ctx, afterSeq, func(e *audit.Entry) error {
		cp := *e
		out := c.edit(&cp)
		if out == nil {
			return nil
		}
		return fn(out)
	})
}

// TestAuditBundleHandlerReportsATamperedChain pins what a bundle download answers when the stored
// chain does not verify.
//
// Every one of these came back as a bare 500 reading "could not assemble the bundle", which is the
// same answer a crashed server gives, on the one question this endpoint exists to answer: an
// operator could not tell a detected tamper from a fault. Worse, the last case came back 200 with a
// signed bundle, because the builder only ever saw the window and a break before it was never
// looked at, so the export quietly attested to the tail of a chain the server could see was broken.
func TestAuditBundleHandlerReportsATamperedChain(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Edit is the tamper applied to the intact chain.
		Edit chainEdit
		// Query is the query string on the bundle request.
		Query string
		// WantErrorHas is text the refusal message must carry.
		WantErrorHas []string
		// WantErrorLacks is text the refusal message must not carry.
		WantErrorLacks []string
		// Want is the refusal body, with Error compared through WantErrorHas instead.
		Want bundleRefusal
	}{{ // Test 0: An entry's payload is rewritten in place, so its hash no longer recomputes.
		Edit: func(e *audit.Entry) *audit.Entry {
			if e.Seq == 3 {
				e.Actor = "mallory"
			}
			return e
		},
		Want: bundleRefusal{
			Reason: reasonChainBreak, BrokeAt: 3, BrokeSeq: 3, Count: 5,
		},
		WantErrorHas: []string{
			"entry 3 of 5", "sequence 3", "not a fault in this server", "/v1/audit/verify",
		},
		WantErrorLacks: []string{"could not assemble the bundle"},
	}, { // Test 1: An entry is deleted, so the entry after it links to something no longer there.
		Edit: func(e *audit.Entry) *audit.Entry {
			if e.Seq == 3 {
				return nil
			}
			return e
		},
		Want: bundleRefusal{
			Reason: reasonChainBreak, BrokeAt: 3, BrokeSeq: 4, Count: 4,
		},
		WantErrorHas:   []string{"entry 3 of 4", "sequence 4", "altered, reordered, or removed"},
		WantErrorLacks: []string{"could not assemble the bundle"},
	}, { // Test 2: The head's hash is blanked, the shape that once panicked the export.
		Edit: func(e *audit.Entry) *audit.Entry {
			if e.Seq == 5 {
				e.Hash = ""
			}
			return e
		},
		Want: bundleRefusal{
			Reason: reasonChainBreak, BrokeAt: 5, BrokeSeq: 5, Count: 5,
		},
		WantErrorHas:   []string{"entry 5 of 5", "sequence 5"},
		WantErrorLacks: []string{"could not assemble the bundle"},
	}, { // Test 3: An entry's sequence is blanked, so the break has no coordinate to name.
		Edit: func(e *audit.Entry) *audit.Entry {
			if e.Seq == 2 {
				e.Seq = 0
			}
			return e
		},
		Want: bundleRefusal{
			Reason: reasonChainBreak, BrokeAt: 2, Count: 5,
		},
		WantErrorHas:   []string{"entry 2 of 5"},
		WantErrorLacks: []string{"sequence", "could not assemble the bundle"},
	}, { // Test 4: A window is asked for whose entries are all past the break. The break is still
		// reported, rather than a signed bundle over the clean tail of a broken chain.
		Query: "?limit=1",
		Edit: func(e *audit.Entry) *audit.Entry {
			if e.Seq == 2 {
				e.Actor = "mallory"
			}
			return e
		},
		Want: bundleRefusal{
			Reason: reasonChainBreak, BrokeAt: 2, BrokeSeq: 2, Count: 5,
		},
		WantErrorHas:   []string{"entry 2 of 5", "sequence 2"},
		WantErrorLacks: []string{"could not assemble the bundle"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			id, err := audit.LoadIdentity(t.TempDir())
			if err != nil {
				t.Fatalf("LoadIdentity() error = %v", err)
			}
			store := newEditedChain(t, seedAuditChain(t, 5), test.Edit)
			rec := httptest.NewRecorder()
			auditBundleHandler(store, &id, "v-test", zap.NewNop()).
				ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/audit/bundle"+test.Query, nil))

			if rec.Code != http.StatusConflict {
				t.Fatalf("status = %d, want 409; a detected tamper must not read as a server fault "+
					"or as a bundle (body %s)", rec.Code, rec.Body.String())
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

// TestAuditBundleHandlerStillExportsAnIntactChain is the control for the tamper table: the same
// handler over the same seeded chain, untouched, still signs and serves a bundle, windowed or not.
// A chain check that refused everything would satisfy the tests above and destroy the feature.
func TestAuditBundleHandlerStillExportsAnIntactChain(t *testing.T) {
	t.Parallel()
	id, err := audit.LoadIdentity(t.TempDir())
	if err != nil {
		t.Fatalf("LoadIdentity() error = %v", err)
	}
	audits := seedAuditChain(t, 5)
	h := auditBundleHandler(audits, &id, "v-test", zap.NewNop())

	for _, query := range []string{"", "?limit=2"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/audit/bundle"+query, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("query %q: status = %d, want 200 (body %s)", query, rec.Code, rec.Body.String())
		}
		rep, err := audit.VerifyBundle(rec.Body.Bytes(), "")
		if err != nil {
			t.Fatalf("query %q: VerifyBundle() error = %v", query, err)
		}
		if !rep.OK() {
			t.Errorf("query %q: the exported bundle does not verify: %+v", query, rep)
		}
	}
}

// TestAuditBundleHandlerNamesTheAnchorItCannotSatisfy proves a chain that hash-verifies but has
// lost its tail is refused with the anchors it fails named. A prefix of a valid chain is itself a
// valid chain, so the hash walk passes and only the anchor catches this, and the refusal used to
// say an anchor was unsatisfied without saying which or why. GET /v1/audit/verify has always
// reported those problems; the export dropped them on the floor.
func TestAuditBundleHandlerNamesTheAnchorItCannotSatisfy(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	id, err := audit.LoadIdentity(t.TempDir())
	if err != nil {
		t.Fatalf("LoadIdentity() error = %v", err)
	}
	audits := seedAuditChain(t, 5)
	chain, err := audits.Chain(ctx)
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	head := chain[len(chain)-1]
	anchors, ok := audits.(audit.AnchorStore)
	if !ok {
		t.Fatal("the seeded store keeps no anchors")
	}
	if err := anchors.SaveAnchor(ctx, &audit.Anchor{
		ID: audit.NewAnchorID(), Type: audit.AnchorHTTPS, Shape: audit.AnchorShapeLinear,
		Seq: head.Seq, Link: head.Hash, At: time.Now().UTC(), Ref: "https://anchors.example/head",
	}); err != nil {
		t.Fatalf("SaveAnchor() error = %v", err)
	}
	// The tail is cut back below the anchor. What remains verifies perfectly on its own.
	cut := newEditedChain(t, audits, func(e *audit.Entry) *audit.Entry {
		if e.Seq >= head.Seq {
			return nil
		}
		return e
	})

	rec := httptest.NewRecorder()
	auditBundleHandler(cut, &id, "v-test", zap.NewNop()).
		ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/audit/bundle", nil))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body %s)", rec.Code, rec.Body.String())
	}
	var got bundleRefusal
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode refusal: %v (body %s)", err, rec.Body.String())
	}
	if got.Reason != reasonAnchorUnsatisfied {
		t.Errorf("reason = %q, want %q", got.Reason, reasonAnchorUnsatisfied)
	}
	if len(got.AnchorProblems) == 0 {
		t.Errorf("the refusal names no anchor problem, so it says an anchor failed without saying "+
			"which: %s", rec.Body.String())
	}
	if strings.Contains(got.Error, "could not assemble the bundle") {
		t.Errorf("refusal message %q is the opaque one", got.Error)
	}
}

// nanosecondChain returns n entries carrying times at nanosecond precision, each hashed so every
// link recomputes. The chain walk passes it, because nothing about it is tampered; the bundle
// builder still refuses it, because a verifier carrying time at microsecond precision recomputes a
// different link. It is the one refusal that reaches the caller through the builder rather than
// through the walk.
func nanosecondChain(t *testing.T, n int) []*audit.Entry {
	t.Helper()
	entries := make([]*audit.Entry, 0, n)
	prev := ""
	for i := range n {
		e := &audit.Entry{
			ID: audit.NewID(), At: time.Date(2026, 9, 8, 12, i, 0, 500, time.UTC),
			Actor: "alice", Method: "POST", Path: "/v1/runs",
			Seq: int64(i + 1), PrevHash: prev,
		}
		e.Hash = audit.EntryHash(e)
		prev = e.Hash
		entries = append(entries, e)
	}
	return entries
}

// TestAuditBundleHandlerReportsAChainItCannotBundle pins the refusal for a chain that verifies but
// cannot be published: the builder's own words reach the caller, under a reason code of their own,
// rather than the opaque 500 a crash gives. The sentinel prefix is trimmed because it names the
// operation, not the problem.
func TestAuditBundleHandlerReportsAChainItCannotBundle(t *testing.T) {
	t.Parallel()
	id, err := audit.LoadIdentity(t.TempDir())
	if err != nil {
		t.Fatalf("LoadIdentity() error = %v", err)
	}
	nano := nanosecondChain(t, 3)
	store := newEditedChain(t, seedAuditChain(t, 3), func(e *audit.Entry) *audit.Entry {
		return nano[e.Seq-1]
	})

	rec := httptest.NewRecorder()
	auditBundleHandler(store, &id, "v-test", zap.NewNop()).
		ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/audit/bundle", nil))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body %s)", rec.Code, rec.Body.String())
	}
	var got bundleRefusal
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode refusal: %v (body %s)", err, rec.Body.String())
	}
	if got.Reason != reasonChainUnbundlable {
		t.Errorf("reason = %q, want %q", got.Reason, reasonChainUnbundlable)
	}
	if got.Count != len(nano) {
		t.Errorf("count = %d, want %d", got.Count, len(nano))
	}
	if !strings.Contains(got.Error, "nanosecond precision") {
		t.Errorf("refusal message %q does not carry the builder's reason", got.Error)
	}
	if strings.HasPrefix(got.Error, audit.ErrExport.Error()) {
		t.Errorf("refusal message %q still opens with the sentinel, which names the operation "+
			"rather than the problem", got.Error)
	}
	if strings.Contains(got.Error, "could not assemble the bundle") {
		t.Errorf("refusal message %q is the opaque one", got.Error)
	}
}

// TestAuditVerifyHandlerReportsATamperAsAnAnswer pins the sibling endpoint against the same
// tampers. A broken chain is a successful report of a broken chain, not a server fault, so verify
// must answer 200 with the break located. It is the endpoint the bundle refusal now points an
// operator at, so a 500 here would leave them with nowhere to go.
func TestAuditVerifyHandlerReportsATamperAsAnAnswer(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Edit is the tamper applied to the intact chain.
		Edit chainEdit
		// WantOK is whether the chain is reported intact.
		WantOK bool
		// WantBrokeAt is the one-based position the report must name.
		WantBrokeAt int
		// WantCount is the number of entries the report must say it checked.
		WantCount int
	}{{ // Test 0: An intact chain verifies, which is the control.
		Edit:   func(e *audit.Entry) *audit.Entry { return e },
		WantOK: true, WantCount: 5,
	}, { // Test 1: A rewritten payload is located rather than raised as a fault.
		Edit: func(e *audit.Entry) *audit.Entry {
			if e.Seq == 4 {
				e.Path = "/v1/runs/deleted"
			}
			return e
		},
		WantBrokeAt: 4, WantCount: 5,
	}, { // Test 2: A deleted entry is located at the entry that lost its link.
		Edit: func(e *audit.Entry) *audit.Entry {
			if e.Seq == 2 {
				return nil
			}
			return e
		},
		WantBrokeAt: 2, WantCount: 4,
	}, { // Test 3: A blanked hash is located rather than crashing the walk.
		Edit: func(e *audit.Entry) *audit.Entry {
			if e.Seq == 1 {
				e.Hash = ""
			}
			return e
		},
		WantBrokeAt: 1, WantCount: 5,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			store := newEditedChain(t, seedAuditChain(t, 5), test.Edit)
			rec := httptest.NewRecorder()
			auditVerifyHandler(store, "", zap.NewNop()).
				ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/audit/verify", nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; a tampered chain is an answer, not a fault "+
					"(body %s)", rec.Code, rec.Body.String())
			}
			var got auditVerifyResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode report: %v (body %s)", err, rec.Body.String())
			}
			want := auditVerifyResponse{OK: test.WantOK, BrokeAt: test.WantBrokeAt, Count: test.WantCount}
			if diff := cmp.Diff(want, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("report mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
