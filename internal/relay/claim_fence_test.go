package relay

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/run"
)

// rlyRequeueAfterReadStore is a run store that lets the janitor requeue a run and another worker
// claim it right after the relay first reads it, which is the window a report's checks and its
// write sit across.
type rlyRequeueAfterReadStore struct {
	// Store serves every call.
	run.Store
	// SummaryAppender serves the continuation writes, which no test here makes.
	run.SummaryAppender
	// once fires after exactly once.
	once sync.Once
	// after requeues the run and claims it again, after the first read returns.
	after func()
}

// Get reads the run and then lets the requeue and the second claim land.
func (s *rlyRequeueAfterReadStore) Get(ctx context.Context, id string) (*run.Run, error) {
	r, err := s.Store.Get(ctx, id)
	s.once.Do(s.after)
	return r, err
}

// TestAStaleReportCannotLandOnTheNextClaim pins the report path against the requeue that lands
// between a report's checks and its write. relay-a claimed the run and stalled. Its report is
// checked against the row relay-a still held, and before the write the janitor requeues the run and
// relay-b claims it.
//
// The step that moves a run from pending to running for a report compared only the status, and the
// progress write was fenced on whoever held the re-read row. So relay-a's terminal report moved
// relay-b's claimed run to running and finished it, and relay-a's progress report moved it to
// running and wrote onto it, before relay-b had started anything. relay-b's own fenced start then
// lost to the stale worker's report.
func TestAStaleReportCannotLandOnTheNextClaim(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name string
		Body string
	}{{ // Test 0: A progress report.
		Name: "progress", Body: `{"status":"running","claimed_by":"relay-a"}`,
	}, { // Test 1: A terminal report from a worker that sent no progress first.
		Name: "terminal", Body: `{"status":"succeeded","claimed_by":"relay-a"}`,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			mem := run.NewMemStore()
			appender, ok := mem.(run.SummaryAppender)
			if !ok {
				t.Fatal("the memory store does not append summaries")
			}
			if err := mem.Save(ctx, &run.Run{ID: "run_race", Playbook: "site.yml",
				Status: run.StatusPending, CreatedAt: time.Now()}); err != nil {
				t.Fatalf("Save() error = %v", err)
			}
			stale, err := mem.Claim(ctx, "relay-a", []string{""})
			if err != nil {
				t.Fatalf("Claim() error = %v", err)
			}
			var next *run.Run
			store := &rlyRequeueAfterReadStore{Store: mem, SummaryAppender: appender}
			store.after = func() {
				if _, err := mem.ReclaimStale(ctx, -time.Minute); err != nil {
					t.Errorf("ReclaimStale() error = %v", err)
				}
				var cerr error
				if next, cerr = mem.Claim(ctx, "relay-b", []string{""}); cerr != nil {
					t.Errorf("second Claim() error = %v", cerr)
				}
			}
			ts := httptest.NewServer(NewHandler(store, SinglePool("tok"), nil, nil, nil))
			t.Cleanup(ts.Close)

			req, err := http.NewRequestWithContext(ctx, http.MethodPost,
				ts.URL+"/relay/v1/runs/run_race/save", strings.NewReader(test.Body))
			if err != nil {
				t.Fatalf("NewRequest() error = %v", err)
			}
			req.Header.Set("Authorization", "Bearer tok")
			req.Header.Set(leaseHeader, stale.ClaimSecret)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("Do() error = %v", err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusNoContent {
				t.Errorf("relay-a's %s report was applied after its claim ended", test.Name)
			}
			if next == nil {
				t.Fatal("the second claim never landed, so the race was not set up")
			}
			got, err := mem.Get(ctx, "run_race")
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			if got.Status != run.StatusPending || got.ClaimedBy != "relay-b" ||
				got.ClaimSecret != next.ClaimSecret {
				t.Errorf("relay-b's claim is %s held by %q after relay-a's stale report, want it "+
					"pending and still relay-b's to start", got.Status, got.ClaimedBy)
			}
		})
	}
}
