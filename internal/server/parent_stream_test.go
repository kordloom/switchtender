package server

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/event"
	"github.com/kordloom/switchtender/internal/live"
	"github.com/kordloom/switchtender/internal/run"
)

// TestParentStreamForwardsShardEventsLive pins that a split or pipeline parent's live stream
// delivers its children's events as they run, before the run turns terminal.
//
// A coordinator runs no tool of its own, so its own event log is always empty. Its children publish
// their events under the parent topic as they execute, the same echo the dispatcher performs. A
// parent stream that answered the wake by re-reading its own empty log emitted nothing, so the
// matrix stayed blank until the run ended. Forwarding the payload the wake carries is what makes the
// parent page fill in live.
func TestParentStreamForwardsShardEventsLive(t *testing.T) {
	t.Parallel()
	testParentStreamForwardsLive(t, run.KindSplit)
}

// TestPipelineStreamForwardsStepEventsLive pins the same behavior for a pipeline parent, which fans
// its steps out the same way a split fans its shards.
func TestPipelineStreamForwardsStepEventsLive(t *testing.T) {
	t.Parallel()
	testParentStreamForwardsLive(t, run.KindPipeline)
}

// testParentStreamForwardsLive drives a coordinator parent stream, has two children report through
// the parent topic while the parent is still running, and asserts both events arrive as SSE event
// frames before any terminal signal.
func testParentStreamForwardsLive(t *testing.T, kind string) {
	t.Helper()
	ctx := context.Background()
	store := run.NewMemStore()
	parentID := "run_parent"
	// The parent is a coordinator, left running so the stream does not end before its children
	// report. A coordinator never runs a tool, so nothing is ever appended to its own event log.
	if err := store.Save(ctx, &run.Run{
		ID: parentID, Kind: kind, Status: run.StatusRunning, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("Save(parent) error = %v", err)
	}
	count := 2
	for i := 0; i < count; i++ {
		idx := i
		if err := store.Save(ctx, &run.Run{
			ID: fmt.Sprintf("run_child_%d", i), Status: run.StatusRunning, CreatedAt: time.Now(),
			ParentID: &parentID, ShardIndex: &idx, ShardCount: &count,
		}); err != nil {
			t.Fatalf("Save(child) error = %v", err)
		}
	}

	hub := live.NewHub()
	srv := httptest.NewServer(New(store, &fakeSubmitter{}, zap.NewNop(), WithStreamer(hub)).Handler())
	t.Cleanup(srv.Close)

	res, err := http.Get(srv.URL + "/v1/runs/" + parentID + "/stream")
	if err != nil {
		t.Fatalf("GET parent stream: %v", err)
	}
	defer func() { _ = res.Body.Close() }()

	// The handler subscribes before it flushes its response headers, so by the time the request
	// returns the subscription is live and a publish will not be dropped. The brief pause is a
	// safety margin, not a correctness requirement.
	time.Sleep(100 * time.Millisecond)

	// Each child reports one host result under the parent topic, exactly as the dispatcher echoes a
	// child's events to its parent. The events are never stored under the parent, so re-reading the
	// parent's log yields nothing and only forwarding the wake payload can deliver them.
	at := time.Date(2026, 7, 30, 0, 0, 0, 0, time.UTC)
	hub.PublishEvents(parentID, []event.Event{
		{Type: event.TypeRunnerOK, Time: at, Play: "p", Task: "install", Host: "web01"},
	})
	hub.PublishEvents(parentID, []event.Event{
		{Type: event.TypeRunnerOK, Time: at, Play: "p", Task: "install", Host: "web02"},
	})

	data := streamEventData(bufio.NewReader(res.Body))
	got := map[string]bool{}
	deadline := time.After(3 * time.Second)
	for len(got) < 2 {
		select {
		case d, ok := <-data:
			if !ok {
				t.Fatalf("parent stream ended before both live events arrived; got %v", got)
			}
			for _, host := range []string{"web01", "web02"} {
				if strings.Contains(d, host) {
					got[host] = true
				}
			}
		case <-deadline:
			t.Fatalf("parent stream did not deliver both live events before turning terminal; got %v", got)
		}
	}
}

// streamEventData reads an SSE body and emits the JSON data of each event frame on the returned
// channel, closing it when the stream ends. It ignores log and end frames, so a caller sees only the
// structured events a page would fold into its matrix.
func streamEventData(reader *bufio.Reader) <-chan string {
	out := make(chan string, 64)
	go func() {
		defer close(out)
		inEvent := false
		for {
			s, err := reader.ReadString('\n')
			if s != "" {
				line := strings.TrimRight(s, "\r\n")
				switch {
				case line == "event: event":
					inEvent = true
				case strings.HasPrefix(line, "event: "):
					inEvent = false
				case inEvent && strings.HasPrefix(line, "data: "):
					out <- strings.TrimPrefix(line, "data: ")
					inEvent = false
				}
			}
			if err != nil {
				return
			}
		}
	}()
	return out
}
