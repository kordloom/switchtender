package review

import (
	"fmt"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// TestRedTeamCommandInACodeBlockDoesNotAct proves a command a comment shows as code is not acted
// on. Leading spaces and tabs are trimmed before the first line is read, so a first line indented
// four spaces or a tab, which Markdown on both forges renders as a code block, still approves.
func TestRedTeamCommandInACodeBlockDoesNotAct(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Body        string
		WantCommand string
	}{{ // Test 0: A first line indented four spaces is an indented code block.
		Body: "    /switchtender apply\n\nThis is what an approver types.", WantCommand: "",
	}, { // Test 1: A first line indented by a tab is an indented code block too.
		Body: "\t/switchtender apply", WantCommand: "",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ev := &CommentEvent{Body: test.Body}
			if diff := cmp.Diff(test.WantCommand, ev.Command()); diff != "" {
				t.Errorf("command mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
