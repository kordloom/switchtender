package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

// queryRecorder is an upstream that records the raw query string of every request it serves, so a
// test can assert the parameter names that actually reached the wire rather than only that a call
// happened.
type queryRecorder struct {
	// mu guards raw, since the client and the test read and write it from different goroutines.
	mu sync.Mutex
	// raw is the RawQuery of the most recent request.
	raw string
	// path is the URL path of the most recent request.
	path string
}

// ServeHTTP records the request's path and query and answers with an empty run listing.
func (q *queryRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	q.mu.Lock()
	q.raw = r.URL.RawQuery
	q.path = r.URL.Path
	q.mu.Unlock()
	_, _ = w.Write([]byte(`{"runs":[]}`))
}

// values returns the recorded query parsed into its parameters.
func (q *queryRecorder) values(t *testing.T) url.Values {
	t.Helper()
	q.mu.Lock()
	defer q.mu.Unlock()
	parsed, err := url.ParseQuery(q.raw)
	if err != nil {
		t.Fatalf("ParseQuery(%q) error = %v", q.raw, err)
	}
	return parsed
}

// TestListRunsSendsTheParameterNamesTheServerReads pins the wire contract of the list_runs tool.
//
// The parameter names are the whole point of this test. A misnamed page parameter is not a failure
// an agent can see: the runs endpoint ignores what it does not recognize, serves its own default
// page, and answers 200, so a tool that asked for ten runs returns two hundred and reports success.
// Asserting only that a request was made, or that it did not error, passes with the wrong name.
func TestListRunsSendsTheParameterNamesTheServerReads(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Query is the fielded search the agent supplies.
		Query string
		// Limit is how many runs the agent asks for.
		Limit int
		// WantValues is the exact set of query parameters that must reach the upstream.
		WantValues url.Values
	}{
		{ // Test 0: A limit reaches the wire as limit, the name the runs endpoint reads.
			Limit:      10,
			WantValues: url.Values{"limit": {"10"}},
		},
		{ // Test 1: A search and a limit travel together under both server-side names.
			Query:      "status:failed host:web01",
			Limit:      5,
			WantValues: url.Values{"q": {"status:failed host:web01"}, "limit": {"5"}},
		},
		{ // Test 2: No limit sends no page parameter, leaving the server's default in force.
			Query:      "status:failed",
			WantValues: url.Values{"q": {"status:failed"}},
		},
		{ // Test 3: A non-positive limit is dropped rather than sent as a zero page.
			Limit:      -1,
			WantValues: url.Values{},
		},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			rec := &queryRecorder{}
			ts := httptest.NewServer(rec)
			defer ts.Close()

			tool := findTool(t, Tools(testClient(t, ts), Options{}), "list_runs")
			args, err := json.Marshal(map[string]any{"query": test.Query, "limit": test.Limit})
			if err != nil {
				t.Fatalf("Marshal() error = %v", err)
			}
			if _, err := tool.Run(context.Background(), args); err != nil {
				t.Fatalf("list_runs error = %v", err)
			}

			if rec.path != "/v1/runs" {
				t.Errorf("path = %q, want /v1/runs", rec.path)
			}
			got := rec.values(t)
			if diff := cmp.Diff(test.WantValues, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("query parameters mismatch (-want +got):\n%s", diff)
			}
			// The old name is called out by itself, so a regression names the defect rather than
			// only showing a diff.
			if got.Has("page_size") {
				t.Errorf("query carries page_size, which the runs endpoint does not read: %q", rec.raw)
			}
		})
	}
}

// findTool returns the named tool or fails the test.
func findTool(t *testing.T, tools []Tool, name string) Tool {
	t.Helper()
	for _, tool := range tools {
		if tool.Name == name {
			return tool
		}
	}
	t.Fatalf("tool %q is not exposed", name)
	return Tool{}
}
