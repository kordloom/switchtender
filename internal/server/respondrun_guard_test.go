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

// TestEveryRunResponseGoesThroughRespondRun pins the rule that a handler cannot quietly opt out of.
//
// Scrubbing a run before it goes back to a caller is a rule spread over more than a dozen handlers,
// and a rule like that holds only as long as everybody writing the next handler knows about it.
// They did not: the read handlers scrubbed and the eleven handlers that create or change a run did
// not, so a retry, a relaunch, a rerun, a template launch, a trigger fire and an approval each
// handed back a run somebody else composed with its secret-shaped assignments in the clear.
//
// The fix was one helper, and this is what keeps it one helper. A respondJSON given a maskRun result
// is a run going out without passing the scrub, which is the exact shape of the bug: masking the
// notification targets looks like masking, so it reads as done.
func TestEveryRunResponseGoesThroughRespondRun(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package: %v", err)
	}

	fset := token.NewFileSet()
	var offenders []string
	seen := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, perr := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if perr != nil {
			continue // A file that does not parse is the build's problem, not this test's.
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			fn, ok := call.Fun.(*ast.Ident)
			if !ok {
				return true
			}
			switch fn.Name {
			case "respondRun":
				seen++
			case "respondJSON":
				for _, arg := range call.Args {
					inner, isCall := arg.(*ast.CallExpr)
					if !isCall {
						continue
					}
					id, isIdent := inner.Fun.(*ast.Ident)
					if isIdent && id.Name == "maskRun" {
						offenders = append(offenders,
							name+":"+fset.Position(call.Pos()).String())
					}
				}
			}
			return true
		})
	}

	if seen == 0 {
		t.Fatal("no respondRun calls found, so this guard is asserting nothing: either the helper " +
			"was renamed or every run response has gone back to writing itself")
	}
	for _, o := range offenders {
		t.Errorf("%s responds with maskRun directly instead of respondRun, so this run goes to "+
			"the caller without the scrub the read handlers apply: a caller below admin sees the "+
			"secret-shaped assignments in a command they did not write", o)
	}
}
