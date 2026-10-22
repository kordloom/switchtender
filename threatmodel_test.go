package main

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// threatModelWriterHeading is the heading of the threat model section on a principal that can write
// the database.
const threatModelWriterHeading = "Whoever can write the database"

// TestThreatModelNamesTheDatabaseWriter checks that the threat model names the adversary who can
// write the database, in the Markdown source and in the page generated from it, and that the two
// claims the homepage and the agents page make about the approval binding are still worded as the
// threat model defends them.
func TestThreatModelNamesTheDatabaseWriter(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Path string
		Want string
	}{{ // Test 0: The Markdown source holds the section heading.
		Path: "docs/threat-model.md",
		Want: "### " + threatModelWriterHeading,
	}, { // Test 1: The generated page holds the section heading with its anchor.
		Path: "site/docs/threat-model.html",
		Want: `<h3 id="whoever-can-write-the-database">` + threatModelWriterHeading + `</h3>`,
	}, { // Test 2: The agents page still states that the executor refuses a drifted change.
		Path: "site/agents.html",
		Want: "The executor refuses a change that drifted after the decision, so what a person " +
			"approved is provably what ran.",
	}, { // Test 3: The homepage still states what an approval commits and the executor recomputes.
		Path: "site/index.html",
		Want: "An approval here commits a digest of the inventory snapshot, the resolved container " +
			"image, the pinned commit, and the saved plan, and the executor recomputes that digest " +
			"before it runs anything.",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := readFlat(t, test.Path); !strings.Contains(got, test.Want) {
				t.Errorf("%s does not contain %q", test.Path, test.Want)
			}
		})
	}
}

// TestThreatModelDatabaseWriterSection checks that the section on a principal that can write the
// database says what the principal lacks and what the chain's public hashes allow.
func TestThreatModelDatabaseWriterSection(t *testing.T) {
	t.Parallel()
	const path = "docs/threat-model.md"
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	_, rest, found := strings.Cut(string(raw), "### "+threatModelWriterHeading+"\n")
	if !found {
		t.Fatalf("%s does not contain the heading %q", path, "### "+threatModelWriterHeading)
	}
	section := rest
	for _, next := range []string{"\n### ", "\n## "} {
		if before, _, ok := strings.Cut(section, next); ok {
			section = before
		}
	}
	section = whitespaceRun.ReplaceAllString(section, " ")
	tests := []struct {
		Want string
	}{{ // Test 0: The section names the identity the writer does not hold.
		Want: "identity",
	}, { // Test 1: The section names the re-link the public hashes allow.
		Want: "re-link",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if !strings.Contains(section, test.Want) {
				t.Errorf("%s section %q does not contain %q", path, threatModelWriterHeading, test.Want)
			}
		})
	}
}
