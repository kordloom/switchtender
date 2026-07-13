package run

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

func TestStatusTerminal(t *testing.T) {
	t.Parallel()
	tests := []struct {
		In           Status
		WantTerminal bool
	}{
		{In: StatusPending, WantTerminal: false},   // Test 0: Pending is not terminal.
		{In: StatusRunning, WantTerminal: false},   // Test 1: Running is not terminal.
		{In: StatusSucceeded, WantTerminal: true},  // Test 2: Succeeded is terminal.
		{In: StatusFailed, WantTerminal: true},     // Test 3: Failed is terminal.
		{In: StatusCanceled, WantTerminal: true},   // Test 4: Canceled is terminal.
		{In: Status("other"), WantTerminal: false}, // Test 5: Unknown is not terminal.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := test.In.Terminal(); got != test.WantTerminal {
				t.Errorf("Terminal() = %v, want %v", got, test.WantTerminal)
			}
		})
	}
}

func TestRunClone(t *testing.T) {
	t.Parallel()
	code := 0
	start := time.Now()
	end := start.Add(time.Second)
	orig := &Run{
		ID:        "run_x",
		Playbook:  "play.yml",
		Inventory: "inventory.ini",
		Status:    StatusSucceeded,
		ExitCode:  &code,
		CreatedAt: start,
		StartedAt: &start,
		EndedAt:   &end,
	}

	clone := orig.Clone()
	if diff := cmp.Diff(orig, clone, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("clone mismatch (-want +got):\n%s", diff)
	}

	*clone.ExitCode = 99
	*clone.StartedAt = end
	if *orig.ExitCode == 99 {
		t.Error("ExitCode pointer is shared between original and clone")
	}
	if orig.StartedAt.Equal(end) {
		t.Error("StartedAt pointer is shared between original and clone")
	}

	if (*Run)(nil).Clone() != nil {
		t.Error("Clone of nil should be nil")
	}
}

func TestNewID(t *testing.T) {
	t.Parallel()
	a := NewID()
	b := NewID()
	if !strings.HasPrefix(a, "run_") {
		t.Errorf("NewID() = %q, want run_ prefix", a)
	}
	if a == b {
		t.Errorf("NewID() returned duplicate id %q", a)
	}
	if want := len("run_") + 16; len(a) != want {
		t.Errorf("len(NewID()) = %d, want %d", len(a), want)
	}
}

// TestRegisterTool registers an extension tool name and confirms ValidTool accepts it, then that a
// bad name panics. It does not call t.Parallel: it writes the package tool registry, so it runs in
// the sequential phase before the parallel tests that read it resume.
func TestRegisterTool(t *testing.T) {
	RegisterTool("plugintool")
	if !ValidTool("plugintool") {
		t.Error("ValidTool(plugintool) = false after RegisterTool, want true")
	}

	tests := []struct {
		Name string
		Tool string
	}{ // Test 0: Empty name is a programming error.
		{Name: "empty name", Tool: ""},
		// Test 1: A built-in name is a programming error.
		{Name: "built-in", Tool: ToolBash},
		// Test 2: A duplicate is a programming error.
		{Name: "duplicate", Tool: "plugintool"},
	}
	for testNum, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Errorf("test %d: RegisterTool(%q) did not panic", testNum, test.Tool)
				}
			}()
			RegisterTool(test.Tool)
		})
	}
}
