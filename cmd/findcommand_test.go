package cmd

import (
	"sync"
	"testing"

	"github.com/spf13/cobra"
)

// rootFind serializes command lookups on the shared root. Cobra records the name a command was
// called by while Find walks the tree, so parallel tests that look commands up race on that write,
// and the race detector turns the run red about one time in seven.
var rootFind sync.Mutex //nolint:gochecknoglobals // Guards the one root every test shares.

// findCommand returns the subcommand of the root named name, failing the test when there is none.
func findCommand(t *testing.T, name string) *cobra.Command {
	t.Helper()
	rootFind.Lock()
	defer rootFind.Unlock()
	sub, _, err := rootCmd.Find([]string{name})
	if err != nil {
		t.Fatalf("find the %s command: %v", name, err)
	}
	return sub
}
