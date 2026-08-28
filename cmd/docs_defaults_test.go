package cmd

import (
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// docsHeading matches a command section heading in the configuration reference.
var docsHeading = regexp.MustCompile(`^## +([a-z][a-z-]*)\s*$`)

// docsDefaultTable matches the header of a table whose second column holds flag defaults. Other
// tables in the reference put something else there, so their rows are not defaults to check.
var docsDefaultTable = regexp.MustCompile(`^\| *Flag *\| *Default *\|`)

// docsLiteralRow matches a flag row whose default is written as a literal, in backticks. The
// reference writes a default it can state exactly in backticks and describes the rest in prose, so
// a bare cell like "provider default" or "host and pid" is a description of behavior rather than a
// value the binary could be holding.
var docsLiteralRow = regexp.MustCompile("^\\| +`(--[a-z0-9-]+)` +\\| +`([^`]*)` +\\|")

// TestDocumentedFlagDefaultsMatchTheBinary holds the configuration reference against the flags the
// binary actually registers.
//
// The reference gave the serve, init, and demo listen address as ":8080" while all three default to
// loopback. Those are not the same address: one is reachable from the network and the other is not,
// so a reader planning a firewall around the documented value was planning around a server that is
// not there. A default is the part of a flag table a reader trusts without checking, and nothing
// held the two together, so the drift stayed silent through a change to the default itself.
func TestDocumentedFlagDefaultsMatchTheBinary(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("../docs/configuration.md")
	if err != nil {
		t.Fatalf("read the configuration reference: %v", err)
	}

	byName := map[string]*cobra.Command{}
	for _, c := range rootCmd.Commands() {
		byName[c.Name()] = c
	}

	var cmd *cobra.Command
	var section string
	defaults := false
	checked := 0
	for _, line := range strings.Split(string(raw), "\n") {
		switch {
		case docsHeading.MatchString(line):
			section = docsHeading.FindStringSubmatch(line)[1]
			cmd, defaults = byName[section], false
			continue
		case docsDefaultTable.MatchString(line):
			defaults = true
			continue
		case strings.TrimSpace(line) == "":
			defaults = false
			continue
		}
		m := docsLiteralRow.FindStringSubmatch(line)
		if m == nil || cmd == nil || !defaults {
			continue
		}
		name, documented := m[1], m[2]
		flag := cmd.Flags().Lookup(strings.TrimPrefix(name, "--"))
		if flag == nil {
			t.Errorf("%s: the reference documents %s, which the command does not register",
				section, name)
			continue
		}
		checked++
		if !sameDefault(flag, documented) {
			t.Errorf("%s %s: documented default %q, binary default %q. A reader trusts a default "+
				"without checking it, so a wrong one is worse than an absent one",
				section, name, documented, flag.DefValue)
		}
	}
	// A parser that quietly matches nothing turns this into a pass that proves nothing, which is the
	// same silence the drift lived in.
	if checked < 20 {
		t.Fatalf("only %d literal defaults matched the table parser, so the reference is not being "+
			"checked at all", checked)
	}
	t.Logf("checked %d documented literal defaults against the binary", checked)
}

// sameDefault reports whether a documented literal states the flag's real default. A duration is
// compared as a duration, so the reference may write the "1h" an operator would type rather than
// the "1h0m0s" the flag prints back.
func sameDefault(f *pflag.Flag, documented string) bool {
	if documented == f.DefValue {
		return true
	}
	if strings.Contains(f.Value.Type(), "duration") {
		want, errW := time.ParseDuration(f.DefValue)
		got, errG := time.ParseDuration(documented)
		return errW == nil && errG == nil && want == got
	}
	return false
}
