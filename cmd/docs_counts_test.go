package cmd

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/secretsource"
)

// numberWords maps the spelled-out numbers the documentation writes to their values. The prose
// spells a count rather than writing a digit, so a count cannot be compared without this.
var numberWords = map[string]int{
	"one": 1, "two": 2, "three": 3, "four": 4, "five": 5, "six": 6, "seven": 7,
	"eight": 8, "nine": 9, "ten": 10, "eleven": 11, "twelve": 12, "thirteen": 13,
	"fourteen": 14, "fifteen": 15, "sixteen": 16, "seventeen": 17, "eighteen": 18,
	"nineteen": 19, "twenty": 20,
}

// docsCommandHeading matches a command section heading in the configuration reference.
var docsCommandHeading = regexp.MustCompile(`(?m)^## +([a-z][a-z -]*?)\s*$`)

// TestEveryCommandHasAConfigurationSection holds the configuration reference against the command
// tree the binary registers.
//
// The reference opens by claiming it lists every command, and listed ten of twenty. The nine missing
// sections included license, which is the command a paying customer runs to install what they
// bought, so the one path that turns a sale into a working install was the least documented thing in
// the product. A claim to be exhaustive is the kind a reader never verifies, so nothing surfaced it.
func TestEveryCommandHasAConfigurationSection(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("../docs/configuration.md")
	if err != nil {
		t.Fatalf("read the configuration reference: %v", err)
	}
	text := string(raw)

	// A heading may cover more than one command, as "## help and completion" does for the two Cobra
	// built-ins, so each heading contributes every word in it.
	documented := map[string]bool{}
	for _, m := range docsCommandHeading.FindAllStringSubmatch(text, -1) {
		for _, word := range strings.Fields(m[1]) {
			documented[word] = true
		}
	}
	if len(documented) < 10 {
		t.Fatalf("only %d command headings parsed out of the reference, so this test is not "+
			"checking it at all", len(documented))
	}

	for _, c := range rootCmd.Commands() {
		name := c.Name()
		if !documented[name] {
			t.Errorf("the configuration reference claims to list every command and has no section "+
				"for %q; either document it or stop claiming the list is complete", name)
		}
	}
}

// TestDocumentedCountsMatchTheCode holds a count the prose spells out against the list the binary
// actually carries.
//
// A count written into prose drifts the moment the list behind it grows, and it drifts silently: the
// sentence still reads correctly and no reader counts the rows to check. This repository has shipped
// "Six properties" over seven cards and "eleven capabilities" over a thirteen-row table, so the
// failure is not hypothetical.
func TestDocumentedCountsMatchTheCode(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name    string
		File    string
		Pattern string
		Want    int
	}{{ // Test 0: The credential kinds a user can create.
		Name: "credential kinds", File: "../docs/comparison.md",
		Pattern: `(?i)\b([a-z]+) credential kinds\b`, Want: len(credential.Kinds()),
	}, { // Test 1: The secret sources a credential can resolve through.
		Name: "secret sources", File: "../docs/comparison.md",
		Pattern: `(?i)\b([a-z]+) sources\b`, Want: len(secretsource.Kinds()),
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			raw, err := os.ReadFile(test.File)
			if err != nil {
				t.Fatalf("read %s: %v", test.File, err)
			}
			m := regexp.MustCompile(test.Pattern).FindStringSubmatch(string(raw))
			if m == nil {
				t.Fatalf("%s: the phrase this test pins was reworded out of %s, so the count is no "+
					"longer held to the code. Re-point the pattern rather than deleting the case",
					test.Name, test.File)
			}
			got, ok := numberWords[strings.ToLower(m[1])]
			if !ok {
				t.Fatalf("%s: %q is not a number word this test knows", test.Name, m[1])
			}
			if got != test.Want {
				t.Errorf("%s: the documentation says %s (%d) and the binary carries %d",
					test.Name, m[1], got, test.Want)
			}
		})
	}
}

// docsTableRow matches any row of a markdown table.
var docsTableRow = regexp.MustCompile(`^\s*\|`)

// docsTableRule matches the header rule that separates a table's headings from its rows.
var docsTableRule = regexp.MustCompile(`^\s*\|[\s|:-]+\|\s*$`)

// TestAStatedCountMatchesTheTableBeneathIt holds a count stated in prose against the table it
// introduces, so the two cannot drift apart.
//
// The comparison page said "Six controllers, eleven capabilities" above a table of six controllers
// and thirteen capabilities. Both halves of that sentence are the first thing an evaluator reads,
// and one of them was wrong by two.
func TestAStatedCountMatchesTheTableBeneathIt(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("../docs/comparison.md")
	if err != nil {
		t.Fatalf("read the comparison: %v", err)
	}
	lines := strings.Split(string(raw), "\n")

	claim := regexp.MustCompile(`(?i)\b([a-z]+) controllers, ([a-z]+) capabilities\b`)
	var controllers, capabilities int
	found := false
	for _, l := range lines {
		if m := claim.FindStringSubmatch(l); m != nil {
			controllers, capabilities = numberWords[strings.ToLower(m[1])], numberWords[strings.ToLower(m[2])]
			found = true
			break
		}
	}
	if !found {
		t.Fatal("the comparison no longer states its controller and capability counts in the form " +
			"this test pins, so nothing holds the sentence to the table beneath it")
	}

	// The first table on the page is the comparison itself: one heading column naming the
	// capability, then one column per controller, and one row per capability.
	gotControllers, gotCapabilities := 0, 0
	for i, l := range lines {
		if !docsTableRow.MatchString(l) || i+1 >= len(lines) || !docsTableRule.MatchString(lines[i+1]) {
			continue
		}
		gotControllers = len(strings.Split(strings.Trim(strings.TrimSpace(l), "|"), "|")) - 1
		for j := i + 2; j < len(lines) && docsTableRow.MatchString(lines[j]); j++ {
			gotCapabilities++
		}
		break
	}
	if gotCapabilities == 0 {
		t.Fatal("no comparison table was parsed, so this test is not checking anything")
	}
	if controllers != gotControllers {
		t.Errorf("the comparison says %d controllers and the table has %d columns beside the "+
			"capability name", controllers, gotControllers)
	}
	if capabilities != gotCapabilities {
		t.Errorf("the comparison says %d capabilities and the table has %d rows",
			capabilities, gotCapabilities)
	}
}
