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
//
// The second shape is a run handed over with no masking at all, which is how the reconcile proposal
// went out: submitter.Submit into a variable, straight into respondJSON. That one is caught by
// following what a submit assigns, since a run is the only thing those calls return.
//
// Neither shape is every possible one. Proving a respondJSON argument is a run needs type
// information this test does not have, and thirty-odd handlers legitimately respond with a bare
// identifier that is a credential or a schedule. What is pinned here are the two shapes that have
// actually gone wrong.
func TestEveryRunResponseGoesThroughRespondRun(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package: %v", err)
	}

	fset := token.NewFileSet()
	var offenders []string
	seen := 0
	fromSubmit := map[string]bool{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, perr := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if perr != nil {
			continue // A file that does not parse is the build's problem, not this test's.
		}
		// Walked one function at a time, so a variable holding a submitted run is only matched
		// against the respondJSON calls that can actually see it.
		for _, decl := range file.Decls {
			ast.Inspect(decl, func(n ast.Node) bool {
				switch node := n.(type) {
				case *ast.AssignStmt:
					if !producesARun(node.Rhs) {
						return true
					}
					for _, lhs := range node.Lhs {
						if id, isIdent := lhs.(*ast.Ident); isIdent && id.Name != "_" {
							fromSubmit[id.Name] = true
						}
					}
				case *ast.CallExpr:
					fn, isIdent := node.Fun.(*ast.Ident)
					if !isIdent {
						return true
					}
					switch fn.Name {
					case "respondRun":
						seen++
					case "respondJSON":
						for _, arg := range node.Args {
							if reason := unscrubbedRun(arg, fromSubmit); reason != "" {
								offenders = append(offenders,
									fset.Position(node.Pos()).String()+" ("+reason+")")
							}
						}
					}
				}
				return true
			})
			for k := range fromSubmit {
				delete(fromSubmit, k)
			}
		}
	}

	if seen == 0 {
		t.Fatal("no respondRun calls found, so this guard is asserting nothing: either the helper " +
			"was renamed or every run response has gone back to writing itself")
	}
	for _, o := range offenders {
		t.Errorf("%s: use respondRun. This run reaches the caller without the scrub the read "+
			"handlers apply, so somebody below admin reads the secret-shaped assignments in a "+
			"command they did not write", o)
	}
}

// runProducers are the calls that return a run and nothing else, so a variable they assign holds a
// run without any need to know its type.
var runProducers = map[string]bool{
	"Submit": true, "SubmitSplit": true, "SubmitPipeline": true,
	"RetryFailedShards": true, "RelaunchFailedHosts": true,
}

// producesARun reports whether any expression on the right of an assignment is a call to one of the
// run producers.
func producesARun(rhs []ast.Expr) bool {
	for _, expr := range rhs {
		call, ok := expr.(*ast.CallExpr)
		if !ok {
			continue
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if ok && runProducers[sel.Sel.Name] {
			return true
		}
	}
	return false
}

// unscrubbedRun reports why an argument handed to respondJSON is a run going out without the scrub,
// or the empty string when it is not one of the two shapes this guard knows.
func unscrubbedRun(arg ast.Expr, fromSubmit map[string]bool) string {
	switch a := arg.(type) {
	case *ast.CallExpr:
		if id, ok := a.Fun.(*ast.Ident); ok && id.Name == "maskRun" {
			return "maskRun result: the notification targets are masked but the command is not"
		}
	case *ast.Ident:
		if fromSubmit[a.Name] {
			return a.Name + " holds a submitted run and is going out with no masking at all"
		}
	}
	return ""
}
