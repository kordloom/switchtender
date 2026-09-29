package dossier

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/run"
)

// The behavior of the register's controls is covered by the node suite in
// internal/ui/assets/jstest/register.test.mjs, which drives the real document. What is left here is
// the part no script can be asked about: that the document is self-contained, that it carries the
// stylesheet rules a report is required to have, and that nothing in it depends on a server or on a
// fragment link, neither of which a reader opening the file from a disk has.

// sampleRegister renders a register carrying one of everything the template can show, so a check
// reads the whole document rather than the half a quiet period produces.
func sampleRegister(t *testing.T) string {
	t.Helper()
	base := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	doc, err := RenderRegister(&RegisterInput{
		From: base, To: base.Add(7 * 24 * time.Hour), GeneratedAt: base,
		Runs: []*run.Run{{
			ID: "run_1", Playbook: "site.yml", Status: run.StatusSucceeded, Actor: "root",
			Source: "template", SourceID: "tpl_web", CreatedAt: base,
		}},
		Decisions:      map[string]Decision{"run_1": {Verdict: "Approved", Actor: "root", At: base, Seq: 4}},
		ChainOK:        true,
		ChainCount:     9,
		Anchored:       1,
		AnchorProblems: []string{"Anchor 4 no longer holds against the chain."},
		Head:           &audit.Entry{Seq: 9, Hash: "abc123"},
	})
	if err != nil {
		t.Fatalf("RenderRegister() error = %v", err)
	}
	return string(doc)
}

// styleBlock returns the register's stylesheet, without the surrounding markup.
func styleBlock(t *testing.T, doc string) string {
	t.Helper()
	open := strings.Index(doc, "<style>")
	end := strings.Index(doc, "</style>")
	if open < 0 || end < open {
		t.Fatal("the register carries no stylesheet")
	}
	return doc[open+len("<style>") : end]
}

// ruleFontSize returns the font size a selector's rule declares, in pixels, and reports whether it
// declares one at all.
func ruleFontSize(css, selector string) (float64, bool) {
	rule := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(selector) + `\s*\{([^}]*)\}`)
	body := rule.FindStringSubmatch(css)
	if body == nil {
		return 0, false
	}
	size := regexp.MustCompile(`font-size:\s*([0-9.]+)px`).FindStringSubmatch(body[1])
	if size == nil {
		return 0, false
	}
	px, err := strconv.ParseFloat(size[1], 64)
	if err != nil {
		return 0, false
	}
	return px, true
}

func TestRegisterIsSelfContained(t *testing.T) {
	t.Parallel()
	doc := sampleRegister(t)
	// A register is mailed to an auditor and opened from a disk. Anything it fetches is a section
	// of the evidence that silently does not render for the person reading it.
	external := regexp.MustCompile(`(?i)(<link\b|<img\b|<iframe\b|<script[^>]+\bsrc=|@import\b|url\(\s*['"]?https?:)`)
	if found := external.FindString(doc); found != "" {
		t.Errorf("the register reaches outside the file: %q", found)
	}
	if strings.Contains(doc, "https://") || strings.Contains(doc, "http://") {
		t.Error("the register carries an absolute URL, so part of it needs a network to render")
	}
}

func TestRegisterNavigatesWithoutFragmentLinksOrInlineHandlers(t *testing.T) {
	t.Parallel()
	doc := sampleRegister(t)
	if strings.Contains(doc, `href="#`) {
		t.Error("the register navigates with a fragment link, which a file URL will not follow")
	}
	if regexp.MustCompile(`\son[a-z]+\s*=\s*"`).MatchString(doc) {
		t.Error("the register carries an inline event handler rather than addEventListener")
	}
	// Every section the sidebar offers has to exist, or the navigation is a dead control.
	jumps := regexp.MustCompile(`data-jump="([a-z]+)"`).FindAllStringSubmatch(doc, -1)
	if len(jumps) < 5 {
		t.Fatalf("the register offers %d jump targets, want the four sections and the cards", len(jumps))
	}
	for _, jump := range jumps {
		if !strings.Contains(doc, `<section id="`+jump[1]+`">`) {
			t.Errorf("the register offers a jump to %q, which is not a section in it", jump[1])
		}
	}
}

func TestRegisterStyleSheetMeetsTheReportStandard(t *testing.T) {
	t.Parallel()
	css := styleBlock(t, sampleRegister(t))
	tests := []struct {
		Name string
		Want string
	}{
		// Test 0: The report follows the reader's system theme.
		{Name: "dark mode", Want: "@media (prefers-color-scheme: dark)"},
		// Test 1: A printed copy drops the controls and the navigation.
		{Name: "print rules", Want: "@media print"},
		// Test 2: The sidebar is the standard width.
		{Name: "sidebar width", Want: "grid-template-columns: 300px"},
		// Test 3: And it stays put while the table scrolls.
		{Name: "sticky sidebar", Want: ".sidebar { position: sticky;"},
		// Test 4: Printing hides the navigation and the controls.
		{Name: "print hides chrome", Want: ".sidebar, .toolbar, .table-foot, button, canvas { display: none; }"},
		// Test 5: A wide table scrolls inside its own box rather than the page.
		{Name: "table scroller", Want: ".scroll { overflow-x: auto; }"},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			if !strings.Contains(css, test.Want) {
				t.Errorf("the register stylesheet is missing %q", test.Want)
			}
		})
	}
}

func TestRegisterTextIsReadable(t *testing.T) {
	t.Parallel()
	css := styleBlock(t, sampleRegister(t))
	// Section descriptions and the numbers beside them are read, not skimmed past, so the standard
	// puts a floor under them.
	tests := []struct {
		Selector    string
		WantAtLeast float64
	}{
		// Test 0: The description under every heading.
		{Selector: ".sub", WantAtLeast: 14},
		// Test 1: The label on a summary card.
		{Selector: ".card .k", WantAtLeast: 14},
		// Test 2: The period the sidebar names.
		{Selector: ".nav-range", WantAtLeast: 14},
		// Test 3: The change table itself.
		{Selector: "table", WantAtLeast: 14},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Selector), func(t *testing.T) {
			t.Parallel()
			px, ok := ruleFontSize(css, test.Selector)
			if !ok {
				t.Fatalf("%s declares no font size, so its size is whatever it inherits", test.Selector)
			}
			if px < test.WantAtLeast {
				t.Errorf("%s is set at %gpx, want at least %gpx", test.Selector, px, test.WantAtLeast)
			}
		})
	}
}

func TestRegisterCollapsesItsLargeSections(t *testing.T) {
	t.Parallel()
	doc := sampleRegister(t)
	// The change table and the anchor list are the two that grow without bound.
	if got := strings.Count(doc, "<details open"); got != 2 {
		t.Errorf("collapsible sections = %d, want the change table and the anchor problems", got)
	}
	if !strings.Contains(doc, `<summary><span class="toggle-label">Change table</span>`) {
		t.Error("the change table has no toggle label on its summary")
	}
}
