package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/audit"
)

// growingAudits appends one entry before every scan after the first, standing in for a request that
// lands between the walk and the window pass.
type growingAudits struct {
	// Store is the store underneath.
	audit.Store
	// scans is how many scans started.
	scans atomic.Int32
}

// ChainScan appends an entry first when it is not the first scan, then passes the scan through.
func (g *growingAudits) ChainScan(ctx context.Context, after int64, fn func(*audit.Entry) error) error {
	if g.scans.Add(1) > 1 {
		if err := g.Append(ctx, &audit.Entry{
			ID: audit.NewID(), At: time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC),
			Actor: "late", Method: "POST", Path: "/v1/runs",
		}); err != nil {
			return err
		}
	}
	return g.Store.ChainScan(ctx, after, fn)
}

// bundleSeqRange returns the sequences of the first and last claims a signed bundle carries, and how
// many it carries.
func bundleSeqRange(t *testing.T, signed []byte) (int64, int64, int) {
	t.Helper()
	var b audit.Bundle
	if err := json.Unmarshal(signed, &b); err != nil {
		t.Fatalf("unmarshal bundle: %v", err)
	}
	if len(b.Claims) == 0 {
		return 0, 0, 0
	}
	return b.Claims[0].Chain.Seq, b.Claims[len(b.Claims)-1].Chain.Seq, len(b.Claims)
}

// TestABundleWindowBySequence covers the older slice of a chain past the ceiling. Before it, the only
// window was the newest N, so a chain over 25,000 entries could never be bundled before its newest
// 25,000 from the API at all. A window names its slice by sequence, both ends inclusive, and every
// window that could never be served is refused before the chain is walked.
func TestABundleWindowBySequence(t *testing.T) {
	t.Parallel()
	id, err := audit.LoadIdentity(t.TempDir())
	if err != nil {
		t.Fatalf("LoadIdentity() error = %v", err)
	}
	tests := []struct {
		// Query is the query string on the bundle request.
		Query string
		// WantCode is the status the request must answer with.
		WantCode int
		// WantFirst and WantLast are the sequences of the first and last claims served.
		WantFirst, WantLast int64
		// WantScans is how many chain scans the request may cost.
		WantScans int
		// WantMsg is a distinctive part of the refusal, empty when the request is served.
		WantMsg string
	}{{ // Test 0: A slice from the middle carries exactly the entries it names.
		Query: "from_seq=20&to_seq=40", WantCode: http.StatusOK, WantFirst: 20, WantLast: 40,
		WantScans: 2,
	}, { // Test 1: The genesis entry alone.
		Query: "from_seq=1&to_seq=1", WantCode: http.StatusOK, WantFirst: 1, WantLast: 1, WantScans: 2,
	}, { // Test 2: The head alone.
		Query: "from_seq=60&to_seq=60", WantCode: http.StatusOK, WantFirst: 60, WantLast: 60,
		WantScans: 2,
	}, { // Test 3: Past the head is refused rather than cut short, after the one walk that found it.
		Query: "from_seq=50&to_seq=61", WantCode: http.StatusBadRequest, WantScans: 1,
		WantMsg: "past the newest entry in the chain, sequence 60",
	}, { // Test 4: Ends reversed.
		Query: "from_seq=40&to_seq=20", WantCode: http.StatusBadRequest, WantMsg: "is past to_seq",
	}, { // Test 5: One end alone.
		Query: "from_seq=20", WantCode: http.StatusBadRequest, WantMsg: "go together",
	}, { // Test 6: The other end alone.
		Query: "to_seq=20", WantCode: http.StatusBadRequest, WantMsg: "go together",
	}, { // Test 7: A window and a limit together.
		Query: "from_seq=1&to_seq=5&limit=5", WantCode: http.StatusBadRequest, WantMsg: "not both",
	}, { // Test 8: Sequence zero does not exist.
		Query: "from_seq=0&to_seq=5", WantCode: http.StatusBadRequest, WantMsg: "from 1",
	}, { // Test 9: Not a number.
		Query: "from_seq=first&to_seq=5", WantCode: http.StatusBadRequest, WantMsg: "from 1",
	}, { // Test 10: One past the ceiling, refused without a walk.
		Query:    fmt.Sprintf("from_seq=1&to_seq=%d", maxBundleEntries+1),
		WantCode: http.StatusBadRequest, WantMsg: "more than the 25000",
	}, { // Test 11: Exactly the ceiling passes the parse and is then held to the chain's head.
		Query:    fmt.Sprintf("from_seq=1&to_seq=%d", maxBundleEntries),
		WantCode: http.StatusBadRequest, WantScans: 1, WantMsg: "past the newest entry",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			audits := &countingAudits{Store: seedAuditChain(t, 60)}
			h := auditBundleHandler(audits, &id, "", "v-test", zap.NewNop())
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/audit/bundle?"+test.Query, nil))
			if rec.Code != test.WantCode {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, test.WantCode, rec.Body.String())
			}
			if got := audits.scans; got != test.WantScans {
				t.Errorf("the request cost %d chain scans, want %d", got, test.WantScans)
			}
			if test.WantMsg != "" {
				if !strings.Contains(rec.Body.String(), test.WantMsg) {
					t.Errorf("the refusal %s does not say %q", rec.Body.String(), test.WantMsg)
				}
				return
			}
			first, last, n := bundleSeqRange(t, rec.Body.Bytes())
			if diff := cmp.Diff([]int64{test.WantFirst, test.WantLast, test.WantLast - test.WantFirst + 1},
				[]int64{first, last, int64(n)}); diff != "" {
				t.Errorf("first, last, count mismatch (-want +got):\n%s", diff)
			}
			rep, err := audit.VerifyBundle(rec.Body.Bytes(), id.KeyID())
			if err != nil {
				t.Fatalf("VerifyBundle() error = %v", err)
			}
			if !rep.OK() {
				t.Errorf("the windowed bundle does not verify: %+v", rep)
			}
		})
	}
}

// TestABundleWindowBySequenceStillWalksTheWholeChain pins that a window by sequence is cut only after
// the whole chain verified, as a window by limit is. A break below the slice is refused, because the
// slice recomputes on its own and would otherwise be signed over a chain this server saw was broken.
func TestABundleWindowBySequenceStillWalksTheWholeChain(t *testing.T) {
	t.Parallel()
	id, err := audit.LoadIdentity(t.TempDir())
	if err != nil {
		t.Fatalf("LoadIdentity() error = %v", err)
	}
	audits := newEditedChain(t, seedAuditChain(t, 60), func(e *audit.Entry) *audit.Entry {
		if e.Seq == 5 {
			e.Actor = "mallory"
		}
		return e
	})
	h := auditBundleHandler(audits, &id, "", "v-test", zap.NewNop())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/audit/bundle?from_seq=20&to_seq=40", nil))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 for a break below the window (body %s)", rec.Code,
			rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), reasonChainBreak) {
		t.Errorf("the refusal %s does not carry %s", rec.Body.String(), reasonChainBreak)
	}
}

// TestABundleNeverSignsPastTheWalk pins that the window pass stops at the newest entry the walk
// verified. An entry appended between the two passes was never checked, and the pass once carried it
// anyway, so a limit=5 bundle could carry six claims and its head was an entry no walk had seen.
func TestABundleNeverSignsPastTheWalk(t *testing.T) {
	t.Parallel()
	id, err := audit.LoadIdentity(t.TempDir())
	if err != nil {
		t.Fatalf("LoadIdentity() error = %v", err)
	}
	tests := []struct {
		// Query is the query string on the bundle request.
		Query string
		// WantLast and WantCount are the last sequence and the claim count served.
		WantLast int64
		// WantCount is how many claims the bundle carries.
		WantCount int
	}{{ // Test 0: The newest five of the ten the walk saw.
		Query: "limit=5", WantLast: 10, WantCount: 5,
	}, { // Test 1: No window, so the whole chain the walk saw.
		Query: "", WantLast: 10, WantCount: 10,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			audits := &growingAudits{Store: seedAuditChain(t, 10)}
			h := auditBundleHandler(audits, &id, "", "v-test", zap.NewNop())
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/audit/bundle?"+test.Query, nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
			}
			_, last, n := bundleSeqRange(t, rec.Body.Bytes())
			if diff := cmp.Diff([]int64{test.WantLast, int64(test.WantCount)},
				[]int64{last, int64(n)}); diff != "" {
				t.Errorf("last, count mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
