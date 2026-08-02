package live

import (
	"testing"
	"time"
)

// TestCloseRunEndsEverySubscriber pins that finishing a run both signals and closes its subscribers.
//
// Dropping the topic without closing left every subscriber holding a channel that would never be
// closed and never written to again: cancel finds no topic to remove it from and returns without
// closing, so a reader ranging over the channel waits forever for a message that cannot arrive.
func TestCloseRunEndsEverySubscriber(t *testing.T) {
	t.Parallel()
	h := NewHub()
	a, cancelA := h.Subscribe("run_1")
	b, cancelB := h.Subscribe("run_1")
	defer cancelA()
	defer cancelB()

	h.CloseRun("run_1")

	for i, ch := range []<-chan Message{a, b} {
		// The end signal arrives first.
		select {
		case msg, ok := <-ch:
			if !ok {
				t.Errorf("subscriber %d was closed without being told the run ended", i)
				continue
			}
			if msg.Type != "end" {
				t.Errorf("subscriber %d got %q, want end", i, msg.Type)
			}
		case <-time.After(time.Second):
			t.Fatalf("subscriber %d got no end signal", i)
		}
		// Then the channel is closed, so a reader ranging over it finishes.
		select {
		case _, ok := <-ch:
			if ok {
				t.Errorf("subscriber %d received a message after the run ended", i)
			}
		case <-time.After(time.Second):
			t.Errorf("subscriber %d was left open after the run ended, so a reader ranging over "+
				"it waits for a message that cannot arrive", i)
		}
	}
	// Canceling afterwards is safe and must not double close.
	cancelA()
	cancelB()
}
