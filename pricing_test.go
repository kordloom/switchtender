package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// whitespaceRun matches the line breaks and indentation a wrapped HTML or Markdown sentence carries,
// so a phrase matches the same whether or not an editor wrapped it.
var whitespaceRun = regexp.MustCompile(`\s+`)

// pricingSurfaces lists every hand-written file a buyer reads prices or commitments from. The
// rendered docs under site/docs are left out because sitegen builds them from docs/*.md, which are
// read here instead.
func pricingSurfaces(t *testing.T) []string {
	t.Helper()
	var files []string
	for _, pattern := range []string{"site/*.html", "docs/*.md"} {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			t.Fatalf("glob %s: %v", pattern, err)
		}
		files = append(files, matches...)
	}
	return append(files, "LICENSING.md", "README.md", "SECURITY.md")
}

// readFlat returns a file's text with every whitespace run collapsed to one space.
func readFlat(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return whitespaceRun.ReplaceAllString(string(raw), " ")
}

// tierCard returns one tier's card from the flattened pricing page, from its name to the end of
// its article, so a phrase is checked against the tier that has to say it.
func tierCard(t *testing.T, page, tier string) string {
	t.Helper()
	start := strings.Index(page, `<div class="tier-name">`+tier+`</div>`)
	if start < 0 {
		t.Fatalf("the pricing page has no %s card", tier)
	}
	end := strings.Index(page[start:], "</article>")
	if end < 0 {
		t.Fatalf("the %s card on the pricing page never closes", tier)
	}
	return page[start : start+end]
}

// findOne returns the first capture group of pattern in text, or fails the test naming what.
func findOne(t *testing.T, pattern, text, what string) string {
	t.Helper()
	m := regexp.MustCompile(pattern).FindStringSubmatch(text)
	if m == nil {
		t.Fatalf("could not find %s (pattern %s)", what, pattern)
	}
	return m[1]
}

// dollars parses a published amount such as "9,900".
func dollars(t *testing.T, s string) int {
	t.Helper()
	n, err := strconv.Atoi(strings.ReplaceAll(s, ",", ""))
	if err != nil {
		t.Fatalf("parse amount %q: %v", s, err)
	}
	return n
}

// thousands formats an amount the way the pages print it, with comma separators.
func thousands(n int) string {
	s := strconv.Itoa(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// TestRetiredPricingClaimsStayGone fails on any public page that still makes a commitment KordLoom
// withdrew before launch. Each one was removed from several pages at once, and a single page left
// behind would keep the promise alive in writing.
func TestRetiredPricingClaimsStayGone(t *testing.T) {
	t.Parallel()
	files := pricingSurfaces(t)
	tests := []struct {
		Pattern string
		Why     string
	}{{ // Test 0: Founding 10.
		Pattern: `(?i)\bfounding (10|ten|customers?)\b`,
		Why:     "Founding 10 was withdrawn before launch, so nothing may offer it or point at it.",
	}, { // Test 1: The 12-hour Team clock.
		Pattern: `(?i)within 12 hours`,
		Why:     "Team's first-response clock is one business day.",
	}, { // Test 2: The every-day clock.
		Pattern: `(?i)every day of the year`,
		Why:     "No support clock runs on weekends or holidays.",
	}, { // Test 3: Round-the-clock coverage.
		Pattern: `(?i)\b24x7\b`,
		Why:     "Enterprise coverage is whatever its order form states.",
	}, { // Test 4: Long-term support lines.
		Pattern: `\bLTS\b`,
		Why:     "SECURITY.md patches only the latest release, so no page sells LTS lines.",
	}, { // Test 5: The no-release continuity trigger.
		Pattern: `(?i)120 consecutive|120-day`,
		Why:     "Continuity converts to Apache 2.0 only if KordLoom ceases business.",
	}, { // Test 6: The Migration Program's band.
		Pattern: `(?i)\bat your band\b`,
		Why:     "The Migration Program includes Team at the 250-host band.",
	}, { // Test 7: An immediate migration start.
		Pattern: `(?i)starts this week|date given here holds`,
		Why:     "A migration date holds once its statement of work confirms it.",
	}, { // Test 8: Enforcement sold as a paid feature.
		Pattern: `(?i)enforced policy`,
		Why:     "Community enforces one policy, so the paid tiers sell more than one.",
	}, { // Test 9: An independent witness.
		Pattern: `(?i)independent (party|witness)`,
		Why:     "The hosted witness is KordLoom-operated: outside the customer's install, not independent of KordLoom.",
	}, { // Test 10: External approver coordination.
		Pattern: `(?i)external approver coordination`,
		Why:     "No code backs it.",
	}, { // Test 11: Compliance sold as an export.
		Pattern: `(?i)is your SOC 2`,
		Why:     "The register produces evidence relevant to SOC 2, not compliance.",
	}, { // Test 12: A receipt lifetime in years.
		Pattern: `(?i)verifies in ten`,
		Why:     "Verification does not depend on KordLoom, and no page promises a span of years.",
	}, { // Test 13: The Migration Program's Team year without its band.
		Pattern: `(?i)first year of Team included[^ a-z]`,
		Why:     "The Migration Program includes Team at the 250-host band, so every mention names it.",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			re := regexp.MustCompile(test.Pattern)
			for _, path := range files {
				if found := re.FindString(readFlat(t, path)); found != "" {
					t.Errorf("%s says %q. %s", path, found, test.Why)
				}
			}
		})
	}
}

// TestProReadsTheSameEverywhere holds every page that states Pro's price or host limit to the Pro
// card on the pricing page, and the license mint to the same host band. A price changed in one
// place and not the others is how the pages came to disagree before.
func TestProReadsTheSameEverywhere(t *testing.T) {
	t.Parallel()
	card := tierCard(t, readFlat(t, "site/pricing.html"), "Pro")
	price := findOne(t, `<div class="tier-price"><b>\$([\d,]+)</b>`, card, "the Pro price")
	hosts := findOne(t, `To ([\d,]+) hosts, single-node`, card, "the Pro host limit")
	mint := findOne(t, `const proBand = "(\d+)"`, readFlat(t, "cmd/cmd_license.go"), "proBand")
	if diff := cmp.Diff(strings.ReplaceAll(hosts, ",", ""), mint); diff != "" {
		t.Errorf("the mint signs Pro at a band the pricing page does not sell (-page +mint):\n%s", diff)
	}
	tests := []struct {
		File    string
		Pattern string
	}{{ // Test 0: The license text.
		File: "LICENSING.md", Pattern: `Pro, at \$(?P<price>[\d,]+) a year flat per organization to (?P<hosts>[\d,]+) hosts`,
	}, { // Test 1: The README.
		File: "README.md", Pattern: `approval policies at \$(?P<price>[\d,]+) a year`,
	}, { // Test 2: The comparison doc's sign-in row.
		File: "docs/comparison.md", Pattern: `SwitchTender prices SSO at \$(?P<price>[\d,]+) a year`,
	}, { // Test 3: The comparison doc's tier row.
		File: "docs/comparison.md", Pattern: `Pro \$(?P<price>[\d,]+) a year to (?P<hosts>[\d,]+) hosts`,
	}, { // Test 4: The AAP comparison table.
		File: "site/aap-alternative.html", Pattern: `Pro, \$(?P<price>[\d,]+)/yr`,
	}, { // Test 5: The Ascender comparison table.
		File: "site/ascender-alternative.html", Pattern: `Pro, \$(?P<price>[\d,]+)/yr`,
	}, { // Test 6: The AWX comparison table.
		File: "site/awx-alternative.html", Pattern: `Pro, \$(?P<price>[\d,]+)/yr`,
	}, { // Test 7: The AWX page's sign-in paragraph.
		File: "site/awx-alternative.html", Pattern: `unlocked by Pro at \$(?P<price>[\d,]+) a year`,
	}, { // Test 8: The Semaphore comparison.
		File: "site/semaphore-alternative.html", Pattern: `SwitchTender at \$(?P<price>[\d,]+) a year for up to (?P<hosts>[\d,]+) hosts`,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			re := regexp.MustCompile(test.Pattern)
			matches := re.FindAllStringSubmatch(readFlat(t, test.File), -1)
			if len(matches) == 0 {
				t.Fatalf("%s no longer states Pro's price where this test reads it (%s)", test.File, test.Pattern)
			}
			for _, m := range matches {
				if got := m[re.SubexpIndex("price")]; got != price {
					t.Errorf("%s prices Pro at $%s, and the pricing page at $%s", test.File, got, price)
				}
				if i := re.SubexpIndex("hosts"); i >= 0 && m[i] != hosts {
					t.Errorf("%s gives Pro %s hosts, and the pricing page %s", test.File, m[i], hosts)
				}
			}
		})
	}
}

// TestNoPageQuotesTheOldProPrice catches a page that still prices Pro at $490 somewhere the table
// above does not read. $490 is Semaphore's Pro price, so it may appear only in a sentence about
// Semaphore.
func TestNoPageQuotesTheOldProPrice(t *testing.T) {
	t.Parallel()
	re := regexp.MustCompile(`\$490`)
	for _, path := range pricingSurfaces(t) {
		text := readFlat(t, path)
		for _, loc := range re.FindAllStringIndex(text, -1) {
			if !strings.Contains(text[max(0, loc[0]-160):loc[0]], "Semaphore") {
				t.Errorf("%s quotes $490 outside a sentence about Semaphore: %q", path,
					text[max(0, loc[0]-80):min(len(text), loc[1]+40)])
			}
		}
	}
}

// TestTheMigrationDifferencesFollowTheTeamBands derives what a larger band adds to the Migration
// Program from the published Team bands, and holds both pages that state it to that arithmetic.
func TestTheMigrationDifferencesFollowTheTeamBands(t *testing.T) {
	t.Parallel()
	page := readFlat(t, "site/pricing.html")
	m := regexp.MustCompile(`\$([\d,]+)</b> to 250 hosts &middot; <b>\$([\d,]+)</b> to 500 &middot; ` +
		`<b>\$([\d,]+)</b> to 1,000 &middot; <b>\$([\d,]+)</b> unlimited`).FindStringSubmatch(page)
	if m == nil {
		t.Fatal("could not find the Team bands on the pricing page")
	}
	b250, b500, b1000, bUnl := dollars(t, m[1]), dollars(t, m[2]), dollars(t, m[3]), dollars(t, m[4])
	team := findOne(t, `<div class="tier-price"><b>\$([\d,]+)</b>`, tierCard(t, page, "Team"), "the Team price")
	if diff := cmp.Diff(b250, dollars(t, team)); diff != "" {
		t.Errorf("the Team headline price is not its 250 band (-band +headline):\n%s", diff)
	}
	mint := findOne(t, `var mintBands = \[\]string\{([^}]*)\}`, readFlat(t, "cmd/cmd_license.go"), "mintBands")
	if diff := cmp.Diff(`"250", "500", "1000", "unlimited"`, mint); diff != "" {
		t.Errorf("the mint's bands are not the published bands (-published +mint):\n%s", diff)
	}
	wantDiffs := fmt.Sprintf("$%s at 500 hosts, $%s at 1,000, and $%s unlimited",
		thousands(b500-b250), thousands(b1000-b250), thousands(bUnl-b250))
	tests := []struct {
		File string
		Want string
	}{{ // Test 0: The pricing page names the band.
		File: "site/pricing.html", Want: "first year of Team at the 250-host band included",
	}, { // Test 1: The pricing page states the differences.
		File: "site/pricing.html", Want: wantDiffs,
	}, { // Test 2: The migration page names the band.
		File: "site/migration.html", Want: "first year of Team at the 250-host band included",
	}, { // Test 3: The migration page states the differences.
		File: "site/migration.html", Want: wantDiffs,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if !strings.Contains(readFlat(t, test.File), test.Want) {
				t.Errorf("%s does not say %q", test.File, test.Want)
			}
		})
	}
}

// importerList parses a published importer list such as "AWX, AAP, Chef, and crontabs" into
// sorted names.
func importerList(list string) []string {
	var names []string
	for _, part := range strings.Split(list, ",") {
		name := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(part), "and "))
		if name != "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// TestCommunityImportersAreThePromise holds the importer list the pricing page shows as Community
// to the lists the never-shrinks promise is measured against, and checks the code gates none of
// them. Chef and Puppet were free in the binary and on the pricing page for weeks while the
// contractual list left them out.
func TestCommunityImportersAreThePromise(t *testing.T) {
	t.Parallel()
	card := tierCard(t, readFlat(t, "site/pricing.html"), "Community")
	want := importerList(findOne(t, `One-command importers: ([^<]+)</li>`, card, "the Community importers"))
	tests := []struct {
		File    string
		Pattern string
	}{{ // Test 0: The license text the terms freeze.
		File: "LICENSING.md", Pattern: `One-command importers: ([^.]+)\.`,
	}, { // Test 1: The baseline doc that restates it.
		File: "docs/community-baseline.md", Pattern: `One-command importers: ([^-]+?) -`,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got := importerList(findOne(t, test.Pattern, readFlat(t, test.File), test.File+" importers"))
			if diff := cmp.Diff(want, got); diff != "" {
				t.Errorf("%s lists different importers than the Community card (-card +file):\n%s", test.File, diff)
			}
		})
	}
	if !strings.Contains(readFlat(t, "site/terms.html"), "The list also includes the Chef and Puppet importers") {
		t.Error("the terms no longer extend the frozen Community list to the Chef and Puppet importers")
	}
	files, err := filepath.Glob("internal/importer/*.go")
	if err != nil {
		t.Fatalf("glob importers: %v", err)
	}
	for _, path := range append(files, "cmd/cmd_import.go") {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		if strings.Contains(readFlat(t, path), "license.") {
			t.Errorf("%s calls the license package, and every importer is a Community feature", path)
		}
	}
}
