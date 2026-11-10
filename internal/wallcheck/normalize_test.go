package wallcheck

import (
	"fmt"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

func TestBodies(t *testing.T) {
	t.Parallel()
	tests := []struct {
		WantExactSame bool
		WantAlphaSame bool
		A             string
		B             string
	}{{ // Test 0: Comments, layout, and semicolons do not change either body.
		A:             "x := f(a) // Call.\nreturn x",
		B:             "x := f(a); /* inline */ return x",
		WantExactSame: true, WantAlphaSame: true,
	}, { // Test 1: Renaming identifiers changes the exact body only.
		A:             "total := add(rules, facts)\nreturn total",
		B:             "sum := plus(ps, seen)\nreturn sum",
		WantExactSame: false, WantAlphaSame: true,
	}, { // Test 2: Predeclared names are kept, so swapping one for another identifier differs.
		A:             "n := len(xs)",
		B:             "n := size(xs)",
		WantExactSame: false, WantAlphaSame: false,
	}, { // Test 3: Literals are kept.
		A:             `f("alpha", 1)`,
		B:             `f("beta", 1)`,
		WantExactSame: false, WantAlphaSame: false,
	}, { // Test 4: Renaming that merges two names into one differs.
		A:             "a = b + c",
		B:             "a = b + b",
		WantExactSame: false, WantAlphaSame: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			a, b := lex([]byte(test.A)), lex([]byte(test.B))
			exactSame := string(exactBody(a)) == string(exactBody(b))
			if diff := cmp.Diff(test.WantExactSame, exactSame); diff != "" {
				t.Errorf("exact sameness mismatch (-want +got):\n%s", diff)
			}
			alphaSame := string(alphaBody(a)) == string(alphaBody(b))
			if diff := cmp.Diff(test.WantAlphaSame, alphaSame); diff != "" {
				t.Errorf("alpha sameness mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestSignificant(t *testing.T) {
	t.Parallel()
	tests := []struct {
		WantLines []line
		In        string
	}{{ // Test 0: Blank and comment lines go, whitespace collapses, numbers stay true.
		In:        "// header\n\n  a   :=  1\n# shell\n\t<!-- html -->\n * doc\nb\n",
		WantLines: []line{{text: "a := 1", num: 3}, {text: "b", num: 7}},
	}, { // Test 1: An empty file has no lines.
		In: "",
	}, { // Test 2: A build constraint is a comment.
		In:        buildTag + "\n\npackage rules\n",
		WantLines: []line{{text: "package rules", num: 3}},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got := significant([]byte(test.In))
			opts := []cmp.Option{cmp.AllowUnexported(line{}), cmpopts.EquateEmpty()}
			if diff := cmp.Diff(test.WantLines, got, opts...); diff != "" {
				t.Errorf("lines mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestScanSkips(t *testing.T) {
	t.Parallel()
	root := writeTree(t, map[string]string{
		"go.sum":                  "example.com/a v1.0.0 h1:abc=\n",
		"node_modules/x/index.js": paidScript,
		".git/config":             paidScript,
		"internal/paid/impl/a.js": paidScript,
		"keep/a.js":               paidScript,
		"keep/bin.dat":            "\x00\x01binary",
	})
	res, err := scan(root, DefaultConfig().Exclude, DefaultConfig())
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	paths := map[string]bool{}
	for _, fp := range res.fps {
		paths[fp.Path] = true
	}
	want := map[string]bool{"keep/a.js": true, "keep/bin.dat": true}
	if diff := cmp.Diff(want, paths); diff != "" {
		t.Errorf("scanned paths mismatch (-want +got):\n%s", diff)
	}
}

func TestScanMissingRoot(t *testing.T) {
	t.Parallel()
	_, err := scan(t.TempDir()+"/absent", nil, DefaultConfig())
	if err == nil {
		t.Fatal("scan of a missing root succeeded")
	}
}
