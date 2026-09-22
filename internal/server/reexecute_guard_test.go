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

// TestEveryReExecutionAuthorizesThroughOneFunction pins that what re-running a finished run requires
// is decided in one place.
//
// Four handlers fire an existing run's spec again: retry, relaunch, rerun, and the drift reconcile.
// Each needs use on every object the run names and on the worker queue it lands on, and the queue is
// deliberately not part of a run's readability, so it has to be added by whatever authorizes an
// execution. When two of the four composed that themselves rather than calling the one function,
// they agreed by coincidence, and the coincidence is the problem: the next object that scopes
// execution gets added to the owner and the copies keep the older, narrower check, with no compile
// error and no failing test. That has already happened once here, with the registry pull credential.
//
// The shape this looks for is a handler that authorizes a queue object by hand. Reaching for
// grant.QueueObject outside the owner means a second opinion about what executing requires.
func TestEveryReExecutionAuthorizesThroughOneFunction(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package: %v", err)
	}

	fset := token.NewFileSet()
	var offenders []string
	owners := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, perr := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if perr != nil {
			continue // A file that does not parse is the build's problem, not this test's.
		}
		for _, decl := range file.Decls {
			fn, isFunc := decl.(*ast.FuncDecl)
			if !isFunc {
				continue
			}
			ast.Inspect(fn, func(n ast.Node) bool {
				call, isCall := n.(*ast.CallExpr)
				if !isCall {
					return true
				}
				sel, isSel := call.Fun.(*ast.SelectorExpr)
				if !isSel || sel.Sel.Name != "QueueObject" {
					return true
				}
				pkg, isIdent := sel.X.(*ast.Ident)
				if !isIdent || pkg.Name != "grant" {
					return true
				}
				// The owner is allowed to name it, and so is the direct launch, which authorizes a
				// queue the request itself asked for rather than one inherited from a finished run.
				switch fn.Name.Name {
				case "authorizeReexecute", "queueObject":
					owners++
				default:
					offenders = append(offenders,
						fn.Name.Name+" at "+fset.Position(call.Pos()).String())
				}
				return true
			})
		}
	}

	if owners == 0 {
		t.Fatal("no function authorizes a queue object at all, so this guard is asserting nothing: " +
			"either the owner was renamed or re-execution stopped checking the queue")
	}
	for _, o := range offenders {
		t.Errorf("%s decides for itself what re-executing a run requires, instead of calling "+
			"authorizeReexecute. The four paths that re-run a spec have to ask one question, or the "+
			"next object that scopes execution is added to one of them and silently missing from "+
			"the others", o)
	}
}
