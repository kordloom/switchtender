package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/run"
)

// seedAuditTrail appends n entries to a fresh memory trail and returns it. Entry i carries a path
// naming its position, so a test can tell which end of the trail it was handed.
func seedAuditTrail(t *testing.T, n int) audit.Store {
	t.Helper()
	audits := audit.NewMemStore()
	base := time.Date(2026, 8, 11, 9, 0, 0, 0, time.UTC)
	for i := range n {
		err := audits.Append(context.Background(), &audit.Entry{
			ID: audit.NewID(), At: base.Add(time.Duration(i) * time.Second),
			Actor: "root", Method: "POST", Path: fmt.Sprintf("/v1/runs/run_%d", i),
		})
		if err != nil {
			t.Fatalf("Append() error = %v", err)
		}
	}
	return audits
}

// TestAuditPageSaysWhenTheTrailIsCut pins that the audit read reports being a page.
//
// The handler took any positive limit, returned whatever the store gave back, and set count to the
// length of that same slice. A reader who asked for five hundred entries of a hundred thousand
// entry trail got five hundred entries and a count of five hundred, with nothing anywhere in the
// answer saying the trail went on. That is the failure mode that matters for an audit artifact:
// not a wrong number, but a complete-looking one.
func TestAuditPageSaysWhenTheTrailIsCut(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name        string
		Query       string
		Stored      int
		WantCount   int
		WantHasMore bool
	}{{ // Test 0: A trail shorter than the page is whole and says so.
		Name: "shorter than the page", Stored: 4, Query: "?limit=5", WantCount: 4,
	}, { // Test 1: Exactly one page is still whole. Reading one past the page is what proves it.
		Name: "exactly one page", Stored: 5, Query: "?limit=5", WantCount: 5,
	}, { // Test 2: The page the audit view asks for, with the trail one entry longer.
		Name: "cut by a single entry", Stored: 501, Query: "?limit=500", WantCount: 500,
		WantHasMore: true,
	}, { // Test 3: An unbounded ask is capped, and the cap is reported as the cut it is.
		Name: "capped ask", Stored: maxAuditPage + 1, Query: "?limit=100000",
		WantCount: maxAuditPage, WantHasMore: true,
	}, { // Test 4: No limit at all is the default page, cut the same way.
		Name: "default page", Stored: defaultAuditPage + 1, Query: "",
		WantCount: defaultAuditPage, WantHasMore: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			handler := New(run.NewMemStore(), &fakeSubmitter{}, zap.NewNop(),
				WithAudit(seedAuditTrail(t, test.Stored))).Handler()
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/audit"+test.Query, nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("GET /v1/audit%s = %d, want 200", test.Query, rec.Code)
			}
			var got auditResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode audit response: %v", err)
			}
			if got.HasMore != test.WantHasMore {
				t.Errorf("has_more = %v, want %v with %d entries stored and query %q",
					got.HasMore, test.WantHasMore, test.Stored, test.Query)
			}
			if got.Count != test.WantCount {
				t.Errorf("count = %d, want %d", got.Count, test.WantCount)
			}
			// The count has to describe the page it came with, or a reader reconciling the two
			// learns nothing from either.
			if len(got.Entries) != got.Count {
				t.Errorf("count = %d but the answer carries %d entries", got.Count, len(got.Entries))
			}
			// The page has to be the newest entries, newest first. Reading one past the page and
			// trimming the wrong end would silently hand back the oldest window instead.
			var wantPaths []string
			for i := range test.WantCount {
				wantPaths = append(wantPaths, fmt.Sprintf("/v1/runs/run_%d", test.Stored-1-i))
			}
			var gotPaths []string
			for _, e := range got.Entries {
				gotPaths = append(gotPaths, e.Path)
			}
			if diff := cmp.Diff(wantPaths, gotPaths, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("page mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
