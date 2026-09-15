package server

import (
	"strings"
	"testing"
)

// TestLogTailKeepsOnlyTheEnd covers the run detail pane pulling an entire log across the network to
// display the last quarter of a megabyte of it.
//
// The pane caps itself at 256 KB and got there by downloading the whole log into the browser and
// slicing it, so a 213 MB log meant 213 MB over the wire and through the tab. The tail is
// accumulated here in a bounded buffer, which keeps the control plane's memory bounded too: that is
// the property the chunked read was written for, and materializing the log to slice it would have
// thrown it away.
func TestLogTailKeepsOnlyTheEnd(t *testing.T) {
	t.Parallel()

	t.Run("keeps the last n bytes across many writes", func(t *testing.T) {
		t.Parallel()
		ring := newTailBuffer(10)
		for i := range 100 {
			ring.write([]byte(string(rune('a' + i%26))))
		}
		if got := len(ring.bytes()); got != 10 {
			t.Errorf("kept %d bytes, want the cap of 10", got)
		}
		if ring.omitted() != 90 {
			t.Errorf("omitted = %d, want 90, or the reader is told the wrong size", ring.omitted())
		}
	})

	t.Run("a single write larger than the cap keeps its end", func(t *testing.T) {
		t.Parallel()
		ring := newTailBuffer(8)
		ring.write([]byte("0123456789abcdef"))
		if got := string(ring.bytes()); got != "89abcdef" {
			t.Errorf("kept %q, want the last 8 bytes", got)
		}
		if ring.omitted() != 8 {
			t.Errorf("omitted = %d, want 8", ring.omitted())
		}
	})

	t.Run("a log under the cap is whole and reports nothing omitted", func(t *testing.T) {
		t.Parallel()
		ring := newTailBuffer(1024)
		ring.write([]byte("line one\nline two\n"))
		if got := string(ring.bytes()); got != "line one\nline two\n" {
			t.Errorf("kept %q, want the whole log", got)
		}
		if ring.omitted() != 0 {
			t.Errorf("omitted = %d, want 0: a whole log must not be labeled a tail", ring.omitted())
		}
	})

	t.Run("the request cannot ask for unbounded memory", func(t *testing.T) {
		t.Parallel()
		if got := tailBytes("999999999"); got != maxLogTail {
			t.Errorf("tailBytes(huge) = %d, want it capped at %d", got, maxLogTail)
		}
		// No parameter means the whole log, streamed, which is what a download wants.
		for _, raw := range []string{"", "0", "-5", "abc"} {
			if got := tailBytes(raw); got != 0 {
				t.Errorf("tailBytes(%q) = %d, want 0 so the full log still streams", raw, got)
			}
		}
	})

	t.Run("the kept tail never exceeds the cap however it is filled", func(t *testing.T) {
		t.Parallel()
		ring := newTailBuffer(16)
		ring.write([]byte(strings.Repeat("x", 10)))
		ring.write([]byte(strings.Repeat("y", 10)))
		ring.write([]byte(strings.Repeat("z", 3)))
		if got := len(ring.bytes()); got > 16 {
			t.Errorf("kept %d bytes past a cap of 16", got)
		}
		if got := string(ring.bytes()); !strings.HasSuffix(got, "zzz") {
			t.Errorf("kept %q, want it to end at the end of the log", got)
		}
	})
}
