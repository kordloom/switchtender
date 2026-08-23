package server

import (
	"strings"
	"testing"
)

// TestABundleExportIsBounded covers what one click on an audit page could cost a long-lived install.
//
// A bundle is one signed document over every claim it carries, so unlike the streaming verify beside it,
// it has to hold them all at once. An audit chain grows for the life of the install, a row per mutating
// request, per webhook fire, and per span beat, so an unwindowed export assembles the entire history in
// memory, several times its stored size, every time somebody asks. Nothing bounded it and nothing told
// the caller they could ask for less, though the command has offered a window all along.
//
// The window is decided from the chain's size, never from a materialized slice, and an export too large
// to assemble is refused with the parameter that would make it work rather than by taking the process
// down. The handler collects only the decided window on a second streaming pass.
func TestABundleExportIsBounded(t *testing.T) {
	t.Parallel()

	tests := []struct {
		// Name says what the caller asked for.
		Name string
		// Count is how many entries the chain holds.
		Count int
		// Limit is the query parameter as sent.
		Limit string
		// WantWindow is how many entries the bundle should carry, when it is served.
		WantWindow int
		// WantMsg is a distinctive part of the refusal, empty when the request is served.
		WantMsg string
	}{
		{"no window", 10, "", 10, ""},
		{"a window narrower than the chain", 10, "4", 4, ""},
		{"a window wider than the chain", 10, "500", 10, ""},
		{"exactly the chain", 10, "10", 10, ""},
		{"not a number", 10, "soon", 0, "limit must be a count"},
		{"zero", 10, "0", 0, "limit must be a count"},
		{"negative", 10, "-5", 0, "limit must be a count"},
		{"past the ceiling", 10, "999999999", 0, "at most"},
		// A chain past the ceiling with no window says how to ask for one, rather than assembling it.
		{"a chain past the ceiling", maxBundleEntries + 1, "", 0, "limit="},
		// A windowed request on the same chain is served: the whole point of deciding from the count
		// is that a bounded ask never pays for the unbounded history.
		{"a window into a chain past the ceiling", maxBundleEntries + 1, "1000", 1000, ""},
	}

	for testNum, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			got, msg := bundleWindow(test.Count, test.Limit)
			if test.WantMsg != "" {
				if msg == "" {
					t.Fatalf("test %d: limit=%q was accepted, want a refusal", testNum, test.Limit)
				}
				if !strings.Contains(msg, test.WantMsg) {
					t.Errorf("test %d: the refusal %q does not explain itself", testNum, msg)
				}
				return
			}
			if msg != "" {
				t.Fatalf("test %d: limit=%q was refused: %s", testNum, test.Limit, msg)
			}
			if got != test.WantWindow {
				t.Errorf("test %d: the window is %d entries, want %d", testNum, got, test.WantWindow)
			}
		})
	}
}
