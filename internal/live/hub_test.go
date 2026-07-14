package live

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/dcadolph/railwarden/internal/event"
)

// drain collects messages until end or a timeout.
func drain(t *testing.T, ch <-chan Message) []Message {
	t.Helper()
	var out []Message
	timeout := time.After(time.Second)
	for {
		select {
		case m, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, m)
			if m.Type == "end" {
				return out
			}
		case <-timeout:
			t.Fatal("timed out waiting for messages")
			return out
		}
	}
}

func TestHubPublishAndEnd(t *testing.T) {
	t.Parallel()
	h := NewHub()
	ch, cancel := h.Subscribe("run_1")
	defer cancel()

	h.PublishEvents("run_1", []event.Event{{Type: event.TypePlayStart, Play: "demo"}})
	h.PublishLog("run_1", []byte("hello"))
	h.CloseRun("run_1")

	got := drain(t, ch)
	if len(got) != 3 {
		t.Fatalf("got %d messages, want 3", len(got))
	}
	if got[0].Type != "event" || got[1].Type != "log" || got[2].Type != "end" {
		t.Fatalf("types = %q %q %q", got[0].Type, got[1].Type, got[2].Type)
	}

	var e event.Event
	if err := json.Unmarshal(got[0].Data, &e); err != nil {
		t.Fatalf("decode event: %v", err)
	}
	if e.Play != "demo" {
		t.Errorf("event play = %q, want demo", e.Play)
	}

	var line string
	if err := json.Unmarshal(got[1].Data, &line); err != nil {
		t.Fatalf("decode log: %v", err)
	}
	if line != "hello" {
		t.Errorf("log = %q, want hello", line)
	}
}

func TestHubUnsubscribe(t *testing.T) {
	t.Parallel()
	h := NewHub()
	ch, cancel := h.Subscribe("run_1")
	cancel()
	cancel() // second cancel must be safe

	if _, ok := <-ch; ok {
		t.Error("channel should be closed after cancel")
	}

	// Publishing to a run with no subscribers must not panic.
	h.PublishEvents("run_1", []event.Event{{Type: event.TypePlayStart}})
	h.CloseRun("run_1")
}
