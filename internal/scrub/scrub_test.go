package scrub_test

import (
	"errors"
	"testing"

	"github.com/kordloom/switchtender/internal/scrub"
)

// upper is a scrubber that masks one known secret, standing in for any real one.
var upper = scrub.ScrubberFunc[string](func(v string) string {
	if v == "" {
		return v
	}
	return replaceSecret(v)
})

// replaceSecret masks the literal secret this test uses.
func replaceSecret(v string) string {
	const secret = "hunter2"
	out := ""
	for i := 0; i < len(v); {
		if i+len(secret) <= len(v) && v[i:i+len(secret)] == secret {
			out += scrub.Marker
			i += len(secret)
			continue
		}
		out += string(v[i])
		i++
	}
	return out
}

// TestRestoreDecidesTheSameWayForEveryField walks the three cases the rule exists to separate.
//
// The middle one is the whole reason the package exists: a caller is shown a mask, changes something
// else, and saves. The submission carries the mask, and storing it verbatim replaces the credential
// with the text of a mask, which fails every later run with nothing on screen to explain it.
func TestRestoreDecidesTheSameWayForEveryField(t *testing.T) {
	t.Parallel()
	const stored = "deploy --pass hunter2 --limit canary"
	shown := upper.Scrub(stored)

	tests := []struct {
		Name     string
		Incoming string
		Want     string
		WantErr  error
	}{{ // Test 0: A real edit, carrying no mask, is taken as given.
		Name: "a real edit", Incoming: "deploy --pass swordfish --limit all",
		Want: "deploy --pass swordfish --limit all",
	}, { // Test 1: The view they were shown, echoed back. The secret is kept.
		Name: "the masked view echoed back", Incoming: shown, Want: stored,
	}, { // Test 2: A mask that is not that view. Which secret it stands for cannot be told.
		Name: "a mask that does not match", Incoming: "deploy --pass " + scrub.Marker + " --limit all",
		WantErr: scrub.ErrEcho,
	}, { // Test 3: An empty submission carries no mask and is a real edit.
		Name: "cleared", Incoming: "", Want: "",
	}}

	for testNum, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			got, err := scrub.Restore(upper, test.Incoming, stored, "command")
			if !errors.Is(err, test.WantErr) {
				t.Fatalf("test %d: Restore() error = %v, want %v", testNum, err, test.WantErr)
			}
			if test.WantErr != nil {
				return
			}
			if got != test.Want {
				t.Errorf("test %d: Restore() = %q, want %q", testNum, got, test.Want)
			}
		})
	}
}

// TestRestoreNeverStoresAMarker is the invariant the whole package is for, stated once: whatever the
// inputs, Restore either refuses or returns something that is not a mask.
func TestRestoreNeverStoresAMarker(t *testing.T) {
	t.Parallel()
	const stored = "deploy --pass hunter2"
	for _, incoming := range []string{
		upper.Scrub(stored),
		"deploy --pass " + scrub.Marker,
		scrub.Marker,
		"a " + scrub.Marker + " b",
		"clean edit",
	} {
		got, err := scrub.Restore(upper, incoming, stored, "command")
		if err != nil {
			continue // refused, which is the safe answer
		}
		if got != "clean edit" && got != stored {
			t.Errorf("Restore(%q) = %q: it stored neither the real value nor a refusal", incoming, got)
		}
	}
}
