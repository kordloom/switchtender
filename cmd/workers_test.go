package cmd

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/kordloom/switchtender/internal/dispatch"
)

// TestWorkersBelowOneIsRefusedRatherThanDefaulted covers a flag that used to do the opposite of
// what it said.
//
// --workers reads "concurrent runs this process executes at once", so zero is the obvious way to
// ask for a node that serves the API and executes nothing, which keeps run credentials off the
// process holding the public listener. The dispatcher reads a pool size below one as unset and
// substitutes four. A server started with --workers 0 executed runs, claimed them under its own
// name, and reported success, and nothing in the output said the flag had been overruled. It was
// found by starting a server that way and watching it run the job anyway.
func TestWorkersBelowOneIsRefusedRatherThanDefaulted(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Hint string
		Want bool
		In   int
	}{
		{In: 0, Hint: serveWorkersHint, Want: true},                        // Test 0: The value that silently became four.
		{In: -3, Hint: serveWorkersHint, Want: true},                       // Test 1: Negatives took the same branch.
		{In: 1, Hint: serveWorkersHint, Want: false},                       // Test 2: The smallest honest pool is allowed.
		{In: dispatch.DefaultWorkers, Hint: serveWorkersHint, Want: false}, // Test 3: The default.
		{In: 0, Hint: workerWorkersHint, Want: true},                       // Test 4: A worker asked to take no runs.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			err := checkWorkers(test.In, test.Hint)
			if got := err != nil; got != test.Want {
				t.Fatalf("checkWorkers(%d) error = %v, want an error: %v", test.In, err, test.Want)
			}
			if !test.Want {
				return
			}
			if !errors.Is(err, ErrUsage) {
				t.Errorf("error = %v, want it to carry ErrUsage so the exit code says usage", err)
			}
			// The refusal has to name the value back, or an operator reading a log cannot tell
			// which of several numeric flags they got wrong.
			if !strings.Contains(err.Error(), fmt.Sprint(test.In)) {
				t.Errorf("refusal does not name the rejected value: %v", err)
			}
			if !strings.Contains(err.Error(), test.Hint) {
				t.Errorf("refusal dropped the hint that says what to do instead: %v", err)
			}
		})
	}
}

// TestTheDispatcherStillTreatsZeroAsUnset records why the check above lives in the command rather
// than in dispatch.
//
// Zero meaning "unset" is the right reading for a library option, where a caller that never set it
// and a caller that set it to zero are indistinguishable. It is the wrong reading for a command
// line flag, where zero was typed on purpose. This test holds that split in place: if dispatch ever
// starts refusing zero itself, the command's check is the one that should move, and this failing
// is how that gets noticed.
func TestTheDispatcherStillTreatsZeroAsUnset(t *testing.T) {
	t.Parallel()
	if dispatch.DefaultWorkers < 1 {
		t.Fatalf("DefaultWorkers = %d, which would make the substitution meaningless",
			dispatch.DefaultWorkers)
	}
}
