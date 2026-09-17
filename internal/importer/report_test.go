package importer

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/template"
)

// warnLiteral finds a warning format written directly at the call site.
var warnLiteral = regexp.MustCompile(`warn\(\s*"((?:[^"\\]|\\.)*)"`)

// warnConst finds a format held in a constant and then passed to warn, which the call-site pattern
// cannot see. One warning was written that way and went unchecked until a real export printed it.
//
// Only formats are scanned, never arguments. A fragment passed as a value composes into a format
// that carries its own classification, so reading it alone judges a sentence nobody ever shows.
var warnConst = regexp.MustCompile(`(?m)^\s*(?:const|var)\s+(\w+)\s*=\s*"((?:[^"\\]|\\.)*)"`)

// TestEveryWarningTheImportersRaiseIsClassified is the guard that keeps the migration report
// honest.
//
// The report groups warnings by reading their text, because tagging fifty-odd raise sites by hand
// is a change where missing one is invisible: the object stops being counted while the report still
// presents itself as complete. Reading the text moves that risk somewhere loud, and this is where.
//
// A new warning that matches neither category lands in Unclassified, and this fails naming it. The
// fix is to word the warning like the others or to widen a pattern deliberately, and either way
// somebody decides rather than the report quietly dropping an item.
func TestEveryWarningTheImportersRaiseIsClassified(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package: %v", err)
	}
	var unclassified []string
	var seen int
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		body, rerr := os.ReadFile(filepath.Join(".", name))
		if rerr != nil {
			t.Fatalf("read %s: %v", name, rerr)
		}
		formats := [][]string{}
		for _, m := range warnLiteral.FindAllStringSubmatch(string(body), -1) {
			formats = append(formats, []string{m[0], m[1]})
		}
		// A constant counts only when it actually reaches warn, so an unrelated string is not
		// judged as though somebody were shown it.
		for _, m := range warnConst.FindAllStringSubmatch(string(body), -1) {
			if strings.Contains(string(body), "warn("+m[1]) {
				formats = append(formats, []string{m[0], m[2]})
			}
		}
		for _, m := range formats {
			format := m[1]
			// The cap notice describes the report, not the export, and is excluded by Report.
			if strings.HasPrefix(format, "more than ") {
				continue
			}
			seen++
			// Verb placeholders are replaced with a word so the sentence reads as it will in a real
			// report; the classifier keys on the surrounding language, not on the values.
			text := regexp.MustCompile(`%[+#]?[a-zA-Z]`).ReplaceAllString(format, "x")
			// The failure worth catching: a warning that describes a drop in words the classifier
			// does not know. That item is counted as something to look at rather than something
			// missing, which undercounts the gap while the report still reads as complete.
			if dropVocabulary.MatchString(text) && !leftOutPhrases.MatchString(text) {
				unclassified = append(unclassified, name+": "+format)
			}
		}
	}
	if seen < 50 {
		t.Fatalf("only %d warnings found, so this guard is barely asserting anything. The raise "+
			"shape changed and this pattern has to change with it", seen)
	}
	if len(unclassified) > 0 {
		t.Errorf("%d of %d warnings describe a drop in words the report does not recognize, so "+
			"they would be counted as items to review rather than as things that did not come "+
			"across:\n  %s", len(unclassified), seen, strings.Join(unclassified, "\n  "))
	}
}

// TestTheReportSaysWhatComesAcrossAndWhatDoesNot covers the shape a migration decision needs.
func TestTheReportSaysWhatComesAcrossAndWhatDoesNot(t *testing.T) {
	t.Parallel()
	p := &Plan{}
	p.Templates = make([]*template.Template, 3)
	p.Credentials = make([]*credential.Credential, 2)
	p.warn("job %q has a remote trigger token, which was NOT imported", "deploy")
	p.warn("a job without a name was skipped")
	p.warn("credential %q arrives as a shell and needs its secret re-entered", "vault")

	r := p.Report()
	if r.CreatedTotal != 5 {
		t.Errorf("created total = %d, want 5", r.CreatedTotal)
	}
	if r.NeedsSecret != 2 {
		t.Errorf("needs secret = %d, want the 2 credential shells", r.NeedsSecret)
	}
	if len(r.LeftOut) != 2 {
		t.Errorf("left out = %v, want the trigger token and the unnamed job", r.LeftOut)
	}
	if len(r.NeedsReview) != 1 {
		t.Errorf("needs review = %v, want the credential re-entry", r.NeedsReview)
	}
	// Ordered by count, so the biggest thing an operator is moving leads.
	if len(r.Created) == 0 || r.Created[0].Kind != "templates" {
		t.Errorf("created = %+v, want templates first as the largest group", r.Created)
	}
}
