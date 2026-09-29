package server

import (
	"strings"
	"testing"

	"github.com/kordloom/switchtender/internal/audit"
)

// TestABundleExportIsBounded covers what one click on an audit page could cost a long-lived install.
//
// A bundle is one signed document over every claim it carries, so unlike the streaming verify beside it,
// it has to hold them all at once. An audit chain grows for the life of the install, a row per mutating
// request, per webhook fire, and per span beat, so an unwindowed export assembles the entire history in
// memory, several times its stored size, every time somebody asks. Nothing bounded it and nothing told
// the caller they could ask for less, though the command has offered a window all along.
//
// The window narrows to the newest entries, which is the end an auditor reads, and an export too large to
// assemble is refused with the parameter that would make it work rather than by taking the process down.
func TestABundleExportIsBounded(t *testing.T) {
	t.Parallel()

	entries := make([]*audit.Entry, 10)
	for i := range entries {
		entries[i] = &audit.Entry{ID: audit.NewID(), Seq: int64(i + 1)}
	}

	tests := []struct {
		// Name says what the caller asked for.
		Name string
		// Limit is the query parameter as sent.
		Limit string
		// WantCount is how many entries the bundle should carry, when it is served.
		WantCount int
		// WantMsg is a distinctive part of the refusal, empty when the request is served.
		WantMsg string
	}{
		{"no window", "", 10, ""},
		{"a window narrower than the chain", "4", 4, ""},
		{"a window wider than the chain", "500", 10, ""},
		{"exactly the chain", "10", 10, ""},
		{"not a number", "soon", 0, "limit must be a count"},
		{"zero", "0", 0, "limit must be a count"},
		{"negative", "-5", 0, "limit must be a count"},
		{"past the ceiling", "999999999", 0, "at most"},
	}

	for testNum, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			got, msg := bundleWindow(entries, test.Limit)
			if test.WantMsg != "" {
				if msg == "" {
					t.Fatalf("test %d: limit=%q was accepted, want a refusal", testNum, test.Limit)
				}
				if !strings.Contains(msg, test.WantMsg) {
					t.Errorf("test %d: the refusal %q does not explain the limit", testNum, msg)
				}
				return
			}
			if msg != "" {
				t.Fatalf("test %d: limit=%q was refused: %s", testNum, test.Limit, msg)
			}
			if len(got) != test.WantCount {
				t.Errorf("test %d: the bundle carries %d entries, want %d",
					testNum, len(got), test.WantCount)
			}
			// A window keeps the newest entries, which is the end of the trail an auditor reads, and
			// keeps them in chain order, which a bundle's claims must be in.
			if len(got) > 0 && got[len(got)-1].Seq != entries[len(entries)-1].Seq {
				t.Errorf("test %d: the window ends at seq %d, want the newest entry %d",
					testNum, got[len(got)-1].Seq, entries[len(entries)-1].Seq)
			}
			for i := 1; i < len(got); i++ {
				if got[i].Seq <= got[i-1].Seq {
					t.Fatalf("test %d: the window is not in chain order at %d", testNum, i)
				}
			}
		})
	}

	// A chain past the ceiling with no window says how to ask for one, rather than assembling it.
	t.Run("a chain past the ceiling", func(t *testing.T) {
		t.Parallel()
		huge := make([]*audit.Entry, maxBundleEntries+1)
		for i := range huge {
			huge[i] = &audit.Entry{ID: "e", Seq: int64(i + 1)}
		}
		got, msg := bundleWindow(huge, "")
		if msg == "" {
			t.Fatalf("a chain of %d entries was assembled whole, want a refusal naming the window",
				len(huge))
		}
		if got != nil {
			t.Error("a refused export still returned entries")
		}
		if !strings.Contains(msg, "limit=") {
			t.Errorf("the refusal %q does not name the parameter that would serve the request", msg)
		}
	})
}
