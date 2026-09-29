package server

import (
	"strconv"
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
		// The window the refusal recommends has to be one the ceiling actually accepts, or the
		// advice walks the caller into the same refusal from the other side.
		{"the window the refusal suggests", maxBundleEntries + 1,
			strconv.Itoa(suggestedBundleWindow), suggestedBundleWindow, ""},
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

// TestTheBundleCeilingFitsTheAdvertisedBox pins the ceiling against the machine the product is sold
// as running on, rather than against what the format tolerates.
//
// The homepage teaches an anonymous curl of this endpoint as the thing to run against any install.
// At the old ceiling one such request assembled every entry, marshaled the document, and signed it
// in memory, peaking near 1.7 GB: an out-of-memory kill on a 1 or 2 GB instance, triggered by a
// stranger following the instructions on the marketing page.
func TestTheBundleCeilingFitsTheAdvertisedBox(t *testing.T) {
	t.Parallel()
	// Measured peak is roughly 7 KB of resident memory per entry assembled, so a ceiling above this
	// cannot be served on the smallest instance the docs describe.
	const bytesPerEntry = 7 << 10
	const smallestBoxBudget = 512 << 20
	if peak := maxBundleEntries * bytesPerEntry; peak > smallestBoxBudget {
		t.Errorf("an unwindowed bundle at the ceiling peaks near %d MB, past the %d MB one request "+
			"may take on the smallest documented install", peak>>20, smallestBoxBudget>>20)
	}
	if suggestedBundleWindow > maxBundleEntries {
		t.Errorf("the refusal suggests limit=%d, which the ceiling of %d refuses",
			suggestedBundleWindow, maxBundleEntries)
	}
}
