package cmd

import (
	"fmt"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/importer"
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

// migrationSummaryCount matches a count line of the import's migration summary, such as
// "  Comes across:       12 objects", capturing the label and the count it states.
var migrationSummaryCount = regexp.MustCompile(`^  ([A-Z][a-z ]*[a-z]):\s+(\d+)\b`)

// migrationSummaryKind matches one kind listed beneath Comes across, such as
// "      templates            1", capturing the kind and how many of it come across.
var migrationSummaryKind = regexp.MustCompile(`^ {6}([a-z][a-z ]*[a-z]) +(\d+)$`)

// TestTheMigrationGuideShowsARealSummary holds the sample summary in the migration guide to the
// lines beneath each count it states, and to what the importer prints.
//
// The guide said 1218 objects come across over three kinds that add up to 1153, that 17
// credentials need a secret with no credentials among those kinds, and that 4 things do not come
// across above a list of one, on the page that says that list is itemized and never summarized. No
// run of the importer had produced any of it. The sample is now a preview of the AWX export the
// importer's own tests read, so every number in it traces to a run, and a change to what the
// importer prints fails here until the sample is pasted from a run again.
func TestTheMigrationGuideShowsARealSummary(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("../docs/migration.md")
	if err != nil {
		t.Fatalf("read the migration guide: %v", err)
	}
	lines := strings.Split(string(raw), "\n")
	start := slices.Index(lines, "    Migration summary:")
	if start < 0 {
		t.Fatal("the migration guide no longer shows a sample summary in the form this test " +
			"pins, so nothing holds its numbers to a run")
	}
	var sample []string
	for _, l := range lines[start:] {
		if !strings.HasPrefix(l, "    ") {
			break
		}
		sample = append(sample, strings.TrimPrefix(l, "    "))
	}

	// Each count the sample states must match what is listed beneath it.
	stated, beneath, heading := map[string]int{}, map[string][]string{}, ""
	for _, l := range sample[1:] {
		if m := migrationSummaryCount.FindStringSubmatch(l); m != nil {
			heading = m[1]
			stated[heading], _ = strconv.Atoi(m[2])
			continue
		}
		beneath[heading] = append(beneath[heading], l)
	}
	if _, ok := stated["Comes across"]; !ok {
		t.Fatalf("no count was parsed out of the sample, so this test is not checking it:\n%s",
			strings.Join(sample, "\n"))
	}
	total, kinds := 0, map[string]int{}
	for _, l := range beneath["Comes across"] {
		m := migrationSummaryKind.FindStringSubmatch(l)
		if m == nil {
			t.Errorf("%q beneath Comes across is not a kind and its count", l)
			continue
		}
		n, _ := strconv.Atoi(m[2])
		kinds[m[1]] = n
		total += n
	}
	if total != stated["Comes across"] {
		t.Errorf("the sample says %d objects come across and the kinds beneath it add up to %d",
			stated["Comes across"], total)
	}
	if stated["Needs a secret"] != kinds["credentials"] {
		t.Errorf("the sample says %d credentials need a secret and lists %d credentials "+
			"coming across", stated["Needs a secret"], kinds["credentials"])
	}
	for _, label := range []string{"Does not come across", "Worth reviewing"} {
		if len(beneath[label]) != stated[label] {
			t.Errorf("the sample says %s: %d and itemizes %d", label, stated[label],
				len(beneath[label]))
		}
	}

	// And the sample must be what a preview of the export prints, so each number traces to a run.
	const export = "../internal/importer/testdata/awx-export.json"
	data, err := os.ReadFile(export)
	if err != nil {
		t.Fatalf("read the export the sample previews: %v", err)
	}
	plan, err := importer.FromAWX(data, time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("FromAWX() error = %v", err)
	}
	var printed strings.Builder
	reportSummary(&printed, plan)
	got := strings.Join(sample, "\n")
	if diff := cmp.Diff(strings.TrimRight(printed.String(), "\n"), got); diff != "" {
		t.Errorf("the sample in the migration guide is not what a preview of %s prints, so paste "+
			"the summary from a real run (-printed +guide):\n%s", export, diff)
	}
}
