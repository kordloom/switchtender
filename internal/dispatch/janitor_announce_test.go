package dispatch

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/run"
)

// TestJanitorAnnouncesWhatItSettles pins that a run the janitor ends is announced on the direct
// channels as well as to its named targets: a server-wide webhook, the channel a pager or a chat
// room hangs off, hears that a run whose worker died was interrupted and that a run past its
// timeout was ended. Those channels have no ledger behind them, so the janitor's own announcement
// is the only way they hear it.
func TestJanitorAnnouncesWhatItSettles(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name       string
		Build      func(now time.Time) *run.Run
		WantStatus run.Status
	}{{ // Test 0: The worker died and the sweep interrupted the run.
		Name: "worker lost",
		Build: func(now time.Time) *run.Run {
			stale := now.Add(-10 * time.Minute)
			return &run.Run{ID: "run_jan_lost", Status: run.StatusRunning, CreatedAt: stale,
				Tool: run.ToolBash, Command: "deploy", ClaimedBy: "worker-that-died",
				ClaimedAt: &stale, StartedAt: &stale}
		},
		WantStatus: run.StatusInterrupted,
	}, { // Test 1: The run outlived its timeout and the control node ended it.
		Name: "overrun",
		Build: func(now time.Time) *run.Run {
			started := now.Add(-10 * time.Minute)
			return &run.Run{ID: "run_jan_overrun", Status: run.StatusRunning, CreatedAt: started,
				Tool: run.ToolBash, Command: "deploy", ClaimedBy: "worker-still-beating",
				ClaimedAt: &now, StartedAt: &started, Timeout: 1}
		},
		WantStatus: run.StatusFailed,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			var mu sync.Mutex
			var heard []string
			hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,
				r *http.Request) {
				var body notification
				if json.NewDecoder(r.Body).Decode(&body) == nil && body.Run != nil {
					mu.Lock()
					heard = append(heard, body.Event+" "+body.Run.ID+" "+string(body.Run.Status))
					mu.Unlock()
				}
				w.WriteHeader(http.StatusOK)
			}))
			t.Cleanup(hook.Close)
			runs := run.NewMemStore()
			orphan := test.Build(time.Now())
			if err := runs.Save(context.Background(), orphan); err != nil {
				t.Fatalf("Save() error = %v", err)
			}
			d := New(runs, okRunner(), nil, WithWebhooks([]string{hook.URL}),
				WithNotifyClient(http.DefaultClient))
			t.Cleanup(d.Close)
			want := "run.finished " + orphan.ID + " " + string(test.WantStatus)
			deadline := time.Now().Add(5 * time.Second)
			for {
				mu.Lock()
				got := append([]string(nil), heard...)
				mu.Unlock()
				for _, h := range got {
					if h == want {
						return
					}
				}
				if time.Now().After(deadline) {
					t.Fatalf("the server-wide webhook heard %v, want %q", got, want)
				}
				time.Sleep(20 * time.Millisecond)
			}
		})
	}
}
