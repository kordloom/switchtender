package server

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestOnlyOneDeciderTurnsAnAuthorizationErrorIntoAResponse holds the refusal wording to one owner.
//
// The authorizer returns two sentinels, and what a caller is told depends on which: a grant they do
// not hold, or an organization they do not belong to. denyOnAuthzError is the one place that turns
// either into a response, so the wording, the status, and the distinction between them are decided
// once.
//
// A handler that tests the sentinel itself and writes its own refusal is how that drifts, and it
// did. Template launch matched errForbiddenGrant and answered the bare one-word body while every
// other grant refusal in the product named the grant, so the same denial read two different ways
// depending on which endpoint the operator reached, and the interface shows that body verbatim.
//
// This is a syntactic rule because the failure is syntactic: the sentinel is in scope everywhere in
// the package, and nothing but this stops the next handler doing the same thing.
func TestOnlyOneDeciderTurnsAnAuthorizationErrorIntoAResponse(t *testing.T) {
	t.Parallel()
	const decider = "authorize.go"
	sentinels := map[string]bool{"errForbiddenGrant": true, "errForbiddenOrg": true}

	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	fset := token.NewFileSet()
	found := 0
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") || filepath.Base(name) == decider {
			continue
		}
		src, rerr := os.ReadFile(name)
		if rerr != nil {
			t.Fatalf("read %s: %v", name, rerr)
		}
		file, perr := parser.ParseFile(fset, name, src, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", name, perr)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			id, ok := n.(*ast.Ident)
			if !ok || !sentinels[id.Name] {
				return true
			}
			found++
			t.Errorf("%s names %s. Only %s may read an authorization sentinel and decide what the "+
				"caller is told, or the same denial reads one way through one endpoint and another "+
				"way through the next. Call denyOnAuthzError instead.",
				fset.Position(id.Pos()), id.Name, decider)
			return true
		})
	}
	// The guard must be looking at something. A rename that made the sentinels invisible to it
	// would leave it passing over a package it no longer checks.
	if src, rerr := os.ReadFile(decider); rerr != nil {
		t.Fatalf("read %s: %v", decider, rerr)
	} else {
		for sentinel := range sentinels {
			if !strings.Contains(string(src), sentinel) {
				t.Errorf("%s no longer defines %s, so this guard is watching for a name that does "+
					"not exist", decider, sentinel)
			}
		}
	}
}
