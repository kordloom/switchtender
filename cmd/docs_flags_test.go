package cmd

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// docsFlagMention matches a flag named anywhere in the configuration reference, in a table cell or
// in prose, so documenting one in either form counts.
var docsFlagMention = regexp.MustCompile("--([a-z0-9-]+)")

// TestEveryFlagIsInTheConfigurationReference holds the configuration reference against the flags the
// binary actually registers.
//
// The page opens by claiming it lists every command, flag, and environment variable. A sibling test
// already holds the command half. The flag half was unheld and untrue: eighteen flags on visible
// commands were missing, and they were not the obscure ones. --policy-file is how an operator runs
// approval policy from a file, --trusted-proxy and --client-ip-header decide which client address
// the server believes behind a proxy, and --allow-admin-token governs what an AI agent's token may
// be. A security reviewer reading this page would conclude those controls do not exist.
//
// A claim to be exhaustive is the kind a reader never verifies and an author never revisits, which
// is why it needs a test rather than care.
//
// A hidden command is excluded, because it is not part of the product's surface: license mint signs
// licenses and is useless without the issuer's private key. That is a property of the command rather
// than a list kept here, so a command that becomes hidden or visible moves itself.
func TestEveryFlagIsInTheConfigurationReference(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("../docs/configuration.md")
	if err != nil {
		t.Fatalf("read the configuration reference: %v", err)
	}
	documented := map[string]bool{}
	for _, m := range docsFlagMention.FindAllStringSubmatch(string(raw), -1) {
		documented[m[1]] = true
	}

	var missing []string
	seen := map[string]bool{}
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		if c.Hidden {
			return
		}
		c.Flags().VisitAll(func(f *pflag.Flag) {
			if f.Hidden || documented[f.Name] || seen[f.Name] {
				return
			}
			seen[f.Name] = true
			missing = append(missing, c.Name()+" --"+f.Name)
		})
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(rootCmd)

	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("the configuration reference says it lists every flag and is missing %d:\n  %s\n"+
			"Document each one, or hide the command if it is not part of the product's surface.",
			len(missing), strings.Join(missing, "\n  "))
	}
}
