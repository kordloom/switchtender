package wallcheck

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

// buildTag is split so no line of this file is itself a paid build constraint.
const buildTag = "//go:" + "build paid"

// scoreFunc is a planted paid function well over the present floor.
const scoreFunc = `func scoreRules(rules []Rule, facts map[string]string) (int, error) {
	total := 0
	for i, r := range rules {
		v, ok := facts[r.Key]
		if !ok {
			return 0, fmt.Errorf("rule %d: missing fact %q", i, r.Key)
		}
		if strings.EqualFold(v, r.Want) {
			total += r.Weight
		} else if r.Required {
			return 0, fmt.Errorf("rule %d: required fact %q is %q", i, r.Key, v)
		}
	}
	return total, nil
}
`

// scoreReformatted is scoreFunc with different layout and comments, the same tokens.
const scoreReformatted = `func tally(rules []Rule, facts map[string]string) (int, error) {

	total := 0 // Running sum.
	for i, r := range rules {
		v, ok := facts[r.Key]
		if !ok { return 0, fmt.Errorf("rule %d: missing fact %q", i, r.Key) }
		if strings.EqualFold(v, r.Want) {
			total += r.Weight
		} else if r.Required {
			/* Required facts must agree. */
			return 0, fmt.Errorf("rule %d: required fact %q is %q", i, r.Key, v)
		}
	}
	return total, nil
}
`

// scoreRenamed is scoreFunc with every local, field, type, and package name changed.
const scoreRenamed = `func weigh(ps []Policy, seen map[string]string) (int, error) {
	sum := 0
	for n, p := range ps {
		got, found := seen[p.Name]
		if !found {
			return 0, xerr.Newf("rule %d: missing fact %q", n, p.Name)
		}
		if text.Same(got, p.Expect) {
			sum += p.Points
		} else if p.Must {
			return 0, xerr.Newf("rule %d: required fact %q is %q", n, p.Name, got)
		}
	}
	return sum, nil
}
`

// keysFunc is a planted paid helper between the exact and alpha floors.
const keysFunc = `func sortedKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
`

// keysRenamed is keysFunc with its names changed.
const keysRenamed = `func orderedNames(src map[string]int) []string {
	out := make([]string, 0, len(src))
	for name := range src {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
`

// ruleType is a planted paid struct type over the exact floor.
const ruleType = `type Rule struct {
	Key       string ` + "`json:\"key\"`" + `
	Want      string ` + "`json:\"want\"`" + `
	Weight    int    ` + "`json:\"weight\"`" + `
	Required  bool   ` + "`json:\"required\"`" + `
	Source    string ` + "`json:\"source\"`" + `
	Scope     string ` + "`json:\"scope\"`" + `
	Owner     string ` + "`json:\"owner\"`" + `
	Priority  int    ` + "`json:\"priority\"`" + `
	Expires   int64  ` + "`json:\"expires\"`" + `
	Inherit   bool   ` + "`json:\"inherit\"`" + `
	Reason    string ` + "`json:\"reason\"`" + `
	Ticket    string ` + "`json:\"ticket\"`" + `
	Approvers int    ` + "`json:\"approvers\"`" + `
	Audit     bool   ` + "`json:\"audit\"`" + `
}
`

// limitsBlock is a planted paid const block over the exact floor.
const limitsBlock = `const (
	maxRules     = 512
	maxFacts     = 4096
	maxWeight    = 1 << 20
	defaultScore = 17
	floorScore   = -3
	ceilingScore = 9001
	graceSeconds = 45
	burstWindow  = 90
	quietPeriod  = 300
	retryLimit   = 6
	retryBackoff = 250
	auditBatch   = 64
	auditFlush   = 5
	reviewQuorum = 2
)
`

// testFunc is a planted paid test body.
const testFunc = `func TestScoreRules(t *testing.T) {
	rules := []Rule{{Key: "os", Want: "linux", Weight: 3}}
	got, err := scoreRules(rules, map[string]string{"os": "linux"})
	if err != nil {
		t.Fatalf("score: %v", err)
	}
	if got != 3 {
		t.Fatalf("got %d, want 3", got)
	}
}
`

// pageLines returns n distinct HTML lines that read as a planted paid page.
func pageLines(prefix string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf(`<div class="%s-row" data-step="%d">Register step %d of the %s flow</div>`,
			prefix, i, i, prefix)
	}
	return out
}

// paidPage is a planted paid HTML page of thirty significant lines.
//
//nolint:gochecknoglobals // Not modified, simplifies testing.
var paidPage = strings.Join(pageLines("enroll", 30), "\n") + "\n"

// paidScript is a planted paid script under the shingle size, so only its whole-file hash exists.
const paidScript = `import { strict as assert } from "node:assert";
const steps = ["claim", "verify", "bind", "seal"];
const tokens = new Map(steps.map((s, i) => [s, i * 7]));
assert.equal(tokens.get("bind"), 14);
assert.equal(steps.length, 4);
export default tokens;
console.log("register ready", tokens.size);
`

// goFile wraps declarations in a package clause and imports.
func goFile(pkg string, decls ...string) string {
	return "package " + pkg + "\n\nimport (\n\t\"fmt\"\n\t\"sort\"\n\t\"strings\"\n)\n\n" +
		strings.Join(decls, "\n")
}

// testFile wraps testFunc in a package clause and the testing import.
func testFile(pkg string) string {
	return "package " + pkg + "\n\nimport \"testing\"\n\n" + testFunc
}

// paidTree returns the planted paid module.
func paidTree() map[string]string {
	score := goFile("rules", ruleType, limitsBlock, scoreFunc, keysFunc)
	return map[string]string{
		"rules/score.go":            buildTag + "\n\n// Proprietary notice.\n\n" + score,
		"rules/score_test.go":       buildTag + "\n\n" + testFile("rules"),
		"register/register.html":    paidPage,
		"register/jstest/ready.mjs": paidScript,
		"build/go.mod":              "module example.com/paidbuild\n\ngo 1.26\n",
	}
}

// publicTree returns the planted public tree, including a mounted copy of the paid module that the
// default exclusions must skip.
func publicTree() map[string]string {
	tree := map[string]string{
		"main.go":               goFile("main", "func main() {\n\tfmt.Println(\"hi\")\n}\n"),
		"internal/core/core.go": goFile("core", "func Add(a, b int) int {\n\treturn a + b\n}\n"),
		"site/index.html":       strings.Join(pageLines("welcome", 20), "\n") + "\n",
		"go.mod":                "module example.com/public\n\ngo 1.26\n",
		"internal/paid/paid.go": "package paid\n",
	}
	for path, body := range paidTree() {
		tree["internal/paid/impl/"+path] = body
	}
	return tree
}

// writeTree writes files under a fresh temporary directory and returns its root.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for path, body := range files {
		full := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// with returns a copy of base with the extra files added or replaced.
func with(base, extra map[string]string) map[string]string {
	out := make(map[string]string, len(base)+len(extra))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

// lineOf returns the one-based line in body where needle starts.
func lineOf(t *testing.T, body, needle string) int {
	t.Helper()
	i := strings.Index(body, needle)
	if i < 0 {
		t.Fatalf("needle %q not in body", needle)
	}
	return strings.Count(body[:i], "\n") + 1
}

func TestFixtureTokenCounts(t *testing.T) {
	t.Parallel()
	cfg := DefaultConfig()
	src := goFile("rules", ruleType, limitsBlock, scoreFunc, keysFunc)
	fps, ok := goUnits("x.go", []byte(src), false, cfg)
	if !ok {
		t.Fatal("fixture does not parse")
	}
	tokens := map[int]int{}
	for _, fp := range fps {
		tokens[fp.Line] = fp.Tokens
	}
	tests := []struct {
		Name    string
		Needle  string
		WantMin int
		WantMax int
	}{
		{Name: "ruleType", Needle: "Rule struct", WantMin: cfg.ExactFloor, WantMax: cfg.AlphaFloor - 1},
		{Name: "limitsBlock", Needle: "const (", WantMin: cfg.ExactFloor, WantMax: cfg.AlphaFloor - 1},
		{Name: "scoreFunc", Needle: "func scoreRules", WantMin: cfg.PresentFloor, WantMax: 1 << 30},
		{Name: "keysFunc", Needle: "func sortedKeys", WantMin: cfg.ExactFloor,
			WantMax: cfg.AlphaFloor - 1},
	}
	for _, test := range tests {
		got := tokens[lineOf(t, src, test.Needle)]
		if got < test.WantMin || got > test.WantMax {
			t.Errorf("%s has %d tokens, want %d to %d", test.Name, got, test.WantMin, test.WantMax)
		}
	}
}

func TestCheck(t *testing.T) { //nolint:funlen // Test function.
	t.Parallel()
	page := pageLines("enroll", 30)
	stolen := goFile("core", scoreFunc)
	reformatted := goFile("core", scoreReformatted)
	renamed := goFile("core", scoreRenamed)
	split1 := strings.Join(page[:15], "\n") + "\n"
	split2 := strings.Join(page[15:], "\n") + "\n"
	pasted := strings.Join(pageLines("welcome", 20), "\n") + "\n<script>\n" +
		strings.Join(page[5:20], "\n") + "\n</script>\n"
	tests := []struct {
		WantFindings []Finding
		Extra        map[string]string
		Cfg          *Config
	}{{  // Test 0: The untouched public tree is clean.
	}, { // Test 1: A paid function copied with its build tag and comments gone.
		Extra: map[string]string{"internal/core/stolen.go": stolen},
		WantFindings: []Finding{
			// The import block's closing parenthesis joins the first shingle, because the paid
			// const block ends with one right before the function.
			{Path: "internal/core/stolen.go", Line: 7, Label: "shingle", Shingles: 5},
			{Path: "internal/core/stolen.go", Line: lineOf(t, stolen, "func"), Label: "exact func"},
		},
	}, { // Test 2: The same function reformatted and renamed at the top level.
		Extra: map[string]string{"internal/core/tally.go": reformatted},
		WantFindings: []Finding{
			{Path: "internal/core/tally.go", Line: lineOf(t, reformatted, "func"), Label: "exact func"},
		},
	}, { // Test 3: The same function with every identifier renamed.
		Extra: map[string]string{"internal/core/weigh.go": renamed},
		WantFindings: []Finding{
			{Path: "internal/core/weigh.go", Line: lineOf(t, renamed, "func"), Label: "alpha func"},
		},
	}, { // Test 4: A paid struct type pasted into another package.
		Extra: map[string]string{"internal/server/rule.go": "package server\n\n" + ruleType},
		WantFindings: []Finding{
			{Path: "internal/server/rule.go", Line: 3, Label: "exact type"},
			{Path: "internal/server/rule.go", Line: 3, Label: "shingle", Shingles: 5},
		},
	}, { // Test 5: A paid const block pasted.
		Extra: map[string]string{"internal/core/limits.go": "package core\n\n" + limitsBlock},
		WantFindings: []Finding{
			{Path: "internal/core/limits.go", Line: 3, Label: "exact value"},
			{Path: "internal/core/limits.go", Line: 3, Label: "shingle", Shingles: 5},
		},
	}, { // Test 6: A short paid helper copied exactly is still on the list.
		Extra:        map[string]string{"internal/core/keys.go": goFile("core", keysFunc)},
		WantFindings: []Finding{{Path: "internal/core/keys.go", Line: 9, Label: "exact func"}},
	}, { // Test 7: A short paid helper renamed falls under the alpha floor and passes.
		Extra: map[string]string{"internal/core/keys.go": goFile("core", keysRenamed)},
	}, { // Test 8: A paid test body copied into a public test file.
		Extra:        map[string]string{"internal/core/core_test.go": testFile("core")},
		WantFindings: []Finding{{Path: "internal/core/core_test.go", Line: 5, Label: "exact func"}},
	}, { // Test 9: The paid page copied whole into the site.
		Extra: map[string]string{"site/register.html": paidPage},
		WantFindings: []Finding{
			{Path: "site/register.html", Line: 1, Label: "file"},
			{Path: "site/register.html", Line: 1, Label: "shingle", Shingles: 19},
		},
	}, { // Test 10: The paid page split across two public files.
		Extra: map[string]string{"site/a.html": split1, "site/b.html": split2},
		WantFindings: []Finding{
			{Path: "site/a.html", Line: 1, Label: "shingle", Shingles: 4},
			{Path: "site/b.html", Line: 1, Label: "shingle", Shingles: 4},
		},
	}, { // Test 11: Part of the paid page pasted into an existing site page.
		Extra:        map[string]string{"site/index.html": pasted},
		WantFindings: []Finding{{Path: "site/index.html", Line: 22, Label: "shingle", Shingles: 4}},
	}, { // Test 12: Thirteen paid lines make two shingles, under the hit threshold.
		Extra: map[string]string{"site/short.html": strings.Join(page[:13], "\n") + "\n"},
	}, { // Test 13: The paid script copied under a new name, caught by its whole-file hash.
		Extra:        map[string]string{"internal/ui/ready.mjs": "// Copied.\n" + paidScript},
		WantFindings: []Finding{{Path: "internal/ui/ready.mjs", Line: 2, Label: "file"}},
	}, { // Test 14: Raising the floors over the planted function lets the copy pass.
		Extra: map[string]string{"internal/core/stolen.go": stolen},
		Cfg: &Config{
			ExactFloor: 1000, AlphaFloor: 1000, PresentFloor: 1000, FileFloor: 1000, ShingleLines: 12,
			ShingleHits: 1000,
		},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			cfg := DefaultConfig()
			if test.Cfg != nil {
				cfg = *test.Cfg
				cfg.Exclude = DefaultConfig().Exclude
			}
			paid := writeTree(t, paidTree())
			list, _, err := Gen(paid, writeTree(t, publicTree()), cfg)
			if err != nil {
				t.Fatalf("gen: %v", err)
			}
			res, err := Check(writeTree(t, with(publicTree(), test.Extra)), list, cfg)
			if err != nil {
				t.Fatalf("check: %v", err)
			}
			if diff := cmp.Diff(test.WantFindings, res.Findings, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("findings mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestGen(t *testing.T) {
	t.Parallel()
	shared := pageLines("shared", 14)
	tests := []struct {
		WantDropped int
		WantPruned  int
		WantText    string
		Want        error
		Paid        map[string]string
		Public      map[string]string
	}{{  // Test 0: A clean pair of trees.
	}, { // Test 1: A paid test body already public is dropped.
		Public:      map[string]string{"internal/core/core_test.go": testFile("core")},
		WantDropped: 1,
	}, { // Test 2: A short paid helper already public is dropped.
		Public:      map[string]string{"internal/core/keys.go": goFile("core", keysFunc)},
		WantDropped: 1,
	}, { // Test 3: A distinctive paid function already public fails.
		Public:   map[string]string{"internal/core/stolen.go": goFile("core", scoreFunc)},
		Want:     ErrPresent,
		WantText: "rules/score.go:",
	}, { // Test 4: A paid page already public fails on its whole-file hash.
		Public:   map[string]string{"site/register.html": paidPage},
		Want:     ErrPresent,
		WantText: "register/register.html:1 file matches site/register.html:1",
	}, { // Test 5: Shingles shared with public boilerplate are pruned.
		Paid: map[string]string{
			"register/footer.html": strings.Join(shared, "\n") + "\n<p>Paid only footer</p>\n",
		},
		Public: map[string]string{
			"site/footer.html": strings.Join(shared, "\n") + "\n<p>Community footer</p>\n",
		},
		WantPruned: 3,
	}, { // Test 6: A paid Go file that does not parse fails, so no unit goes unhashed.
		Paid:     map[string]string{"rules/broken.go": "package rules\n\nfunc {\n"},
		Want:     ErrParse,
		WantText: "rules/broken.go",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			paid := writeTree(t, with(paidTree(), test.Paid))
			public := writeTree(t, with(publicTree(), test.Public))
			list, rep, err := Gen(paid, public, DefaultConfig())
			if !errors.Is(err, test.Want) {
				t.Fatalf("got error %v, want %v", err, test.Want)
			}
			if err != nil {
				if !strings.Contains(err.Error(), test.WantText) {
					t.Errorf("error %q does not name %q", err, test.WantText)
				}
				return
			}
			if diff := cmp.Diff(test.WantDropped, rep.Dropped); diff != "" {
				t.Errorf("dropped mismatch (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(test.WantPruned, rep.Pruned); diff != "" {
				t.Errorf("pruned mismatch (-want +got):\n%s", diff)
			}
			res, err := Check(public, list, DefaultConfig())
			if err != nil {
				t.Fatalf("check: %v", err)
			}
			if diff := cmp.Diff([]Finding(nil), res.Findings, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("the tree gen ran against must check clean (-want +got):\n%s", diff)
			}
		})
	}
}

func TestGenDeterministic(t *testing.T) {
	t.Parallel()
	paid, public := writeTree(t, paidTree()), writeTree(t, publicTree())
	var out [2]bytes.Buffer
	for i := range out {
		list, _, err := Gen(paid, public, DefaultConfig())
		if err != nil {
			t.Fatalf("gen: %v", err)
		}
		if _, err := list.WriteTo(&out[i]); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	if diff := cmp.Diff(out[0].String(), out[1].String()); diff != "" {
		t.Errorf("gen output differs between runs (-first +second):\n%s", diff)
	}
	text := out[0].String()
	if strings.Contains(text, "scoreRules") || strings.Contains(text, "register") {
		t.Error("list leaks content")
	}
}
