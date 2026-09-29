package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/google/go-cmp/cmp"
	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/run"
)

// windowSpy records the window each fleet view asks the store for, so a test can assert what the
// handler actually requested rather than what it reported.
type windowSpy struct {
	// Store answers every method the spy does not record.
	run.Store
	// window holds the last window FleetHealth or TaskTrends was called with.
	window atomic.Int64
	// limit holds the last limit HostHistory was called with.
	limit atomic.Int64
}

// FleetHealth records the window and answers empty.
func (s *windowSpy) FleetHealth(_ context.Context, window int) ([]run.HostHealth, error) {
	s.window.Store(int64(window))
	return nil, nil
}

// TaskTrends records the window and answers empty.
func (s *windowSpy) TaskTrends(_ context.Context, window int) ([]run.TaskTrend, error) {
	s.window.Store(int64(window))
	return nil, nil
}

// HostHistory records the limit and answers empty.
func (s *windowSpy) HostHistory(_ context.Context, _ string, limit int) ([]run.HostSummary, error) {
	s.limit.Store(int64(limit))
	return nil, nil
}

// TestSummaryWindowIsCapped pins the bound on the caller-supplied window of the fleet views.
//
// The per host and per task summary tables are the ones retention keeps rather than deletes, so on
// a long lived fleet they hold a row for every host of every run. Each row a window admits becomes
// an element of the answer, concatenated and serialized, so an uncapped window turns one query
// string into a request to render the whole history of every host. The assertion is on the window
// that reached the store and on the window the response reports, not on the status code: an
// uncapped handler answers 200 just as happily.
func TestSummaryWindowIsCapped(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Path is the request target, query string included.
		Path string
		// WantStore is the window or limit the store must be asked for.
		WantStore int
		// WantReported is the window the response body must echo. Zero means the response carries
		// no window field, which is the host history view.
		WantReported int
	}{
		{ // Test 0: An absurd fleet window is cut to the cap instead of reaching the store.
			Path: "/v1/fleet?window=100000000", WantStore: run.MaxSummaryWindow,
			WantReported: run.MaxSummaryWindow,
		},
		{ // Test 1: A window one past the cap is still cut.
			Path: "/v1/fleet?window=101", WantStore: 100, WantReported: 100,
		},
		{ // Test 2: A window under the cap is honored exactly.
			Path: "/v1/fleet?window=50", WantStore: 50, WantReported: 50,
		},
		{ // Test 3: No window at all gets the modest default, not the cap.
			Path: "/v1/fleet", WantStore: defaultFleetWindow, WantReported: defaultFleetWindow,
		},
		{ // Test 4: A window that does not parse falls back to the default.
			Path: "/v1/fleet?window=all", WantStore: defaultFleetWindow,
			WantReported: defaultFleetWindow,
		},
		{ // Test 5: A zero window falls back to the default rather than asking for nothing.
			Path: "/v1/fleet?window=0", WantStore: defaultFleetWindow,
			WantReported: defaultFleetWindow,
		},
		{ // Test 6: A negative window falls back to the default.
			Path: "/v1/fleet?window=-9", WantStore: defaultFleetWindow,
			WantReported: defaultFleetWindow,
		},
		{ // Test 7: The task trend view carries the same cap.
			Path: "/v1/tasks?window=999999", WantStore: run.MaxSummaryWindow,
			WantReported: run.MaxSummaryWindow,
		},
		{ // Test 8: A task window under the cap is honored exactly.
			Path: "/v1/tasks?window=7", WantStore: 7, WantReported: 7,
		},
		{ // Test 9: One host's history has its own, deeper cap.
			Path: "/v1/hosts/db01/runs?limit=100000000", WantStore: run.MaxHostHistory,
		},
		{ // Test 10: A host history limit under that cap is honored exactly.
			Path: "/v1/hosts/db01/runs?limit=250", WantStore: 250,
		},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			spy := &windowSpy{Store: run.NewMemStore()}
			handler := New(spy, &fakeSubmitter{}, zap.NewNop()).Handler()
			rec := httptest.NewRecorder()

			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, test.Path, nil))

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body)
			}
			got := int(spy.window.Load())
			if test.WantReported == 0 {
				got = int(spy.limit.Load())
			}
			if diff := cmp.Diff(test.WantStore, got); diff != "" {
				t.Errorf("window reaching the store mismatch (-want +got):\n%s", diff)
			}
			if test.WantReported == 0 {
				return
			}
			var body struct {
				// Window is the window the view says it used.
				Window int `json:"window"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode body %s: %v", rec.Body, err)
			}
			if diff := cmp.Diff(test.WantReported, body.Window); diff != "" {
				t.Errorf("reported window mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
