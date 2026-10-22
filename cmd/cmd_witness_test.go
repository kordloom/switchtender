package cmd

import (
	"fmt"
	"regexp"
	"testing"

	"github.com/spf13/cobra"
)

// TestWitnessHelpNeverCallsItIndependent holds the help text of the witness commands to the wording
// the product copy uses. The witness is outside the watched install, and the hosted one is operated
// by KordLoom, so it is never independent of KordLoom and the help never says so. The four commands
// a reader meets first are named, so a command dropped from the tree fails rather than passing
// unread, and every other witness subcommand is walked as well.
func TestWitnessHelpNeverCallsItIndependent(t *testing.T) {
	t.Parallel()
	forbidden := regexp.MustCompile(`(?i)independen`)
	tests := []struct {
		Cmd *cobra.Command
	}{{ // Test 0: The witness command itself.
		Cmd: witnessCmd,
	}, { // Test 1: The witness server.
		Cmd: witnessServeCmd,
	}, { // Test 2: Enrollment with a witness.
		Cmd: witnessEnrollCmd,
	}, { // Test 3: Rotating the witness key.
		Cmd: witnessRotateCmd,
	}}
	seen := map[*cobra.Command]bool{}
	for _, test := range tests {
		seen[test.Cmd] = true
	}
	for _, sub := range witnessCmd.Commands() {
		if !seen[sub] {
			seen[sub] = true
			tests = append(tests, struct{ Cmd *cobra.Command }{Cmd: sub})
		}
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if test.Cmd.Parent() != witnessCmd && test.Cmd != witnessCmd {
				t.Fatalf("%q is not a witness subcommand", test.Cmd.Name())
			}
			for field, text := range map[string]string{
				"Short":   test.Cmd.Short,
				"Long":    test.Cmd.Long,
				"Example": test.Cmd.Example,
			} {
				if found := forbidden.FindString(text); found != "" {
					t.Errorf("%s %s says %q; the witness is outside, never independent",
						test.Cmd.CommandPath(), field, found)
				}
			}
		})
	}
}
