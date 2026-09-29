package audit_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoEntryIsStampedWithAFreshWallClock keeps closed the gap five handlers had opened.
//
// An audit.Entry with a zero At is stamped when the store assigns its sequence, under the same
// lock, so the recorded time and the position agree. A handler that instead wrote At: time.Now()
// read the wall clock a beat before the store took that lock, so under ordinary concurrency two
// requests were stamped in one order and sequenced in the other, and the trail held entries whose
// time preceded the one before them on an install where no clock moved. The chain still recomputed,
// so it never read as broken; it read as edited.
//
// The rule this enforces is exactly that: never stamp an entry with a freshly-read wall clock in a
// live path. It is not "these packages may set At." An entry that carries a clock chosen elsewhere,
// the dispatcher's clock threaded through as a parameter or the demo's deliberate backdating, is
// fine and stays fine, because that time is a decision rather than a race. So this flags the one
// shape that is the race, At: time.Now(), and needs no list of blessed packages to do it.
func TestNoEntryIsStampedWithAFreshWallClock(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	fset := token.NewFileSet()

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch filepath.Base(path) {
			case "vendor", "node_modules", ".git":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return nil
		}
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok || !isAuditEntry(lit.Type) {
				return true
			}
			for _, el := range lit.Elts {
				kv, ok := el.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				id, ok := kv.Key.(*ast.Ident)
				if !ok || id.Name != "At" || !isTimeNowCall(kv.Value) {
					continue
				}
				rel, _ := filepath.Rel(root, path)
				t.Errorf("%s:%d stamps an audit.Entry with time.Now(): a live wall-clock read "+
					"before the store takes its sequence lock is how the trail's times ran "+
					"backwards under concurrency. Leave At zero to let the store stamp it at "+
					"append time, or pass a chosen time through a clock the caller controls.",
					rel, fset.Position(kv.Pos()).Line)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
}

// isAuditEntry reports whether a composite-literal type names audit.Entry.
func isAuditEntry(expr ast.Expr) bool {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "audit" && sel.Sel.Name == "Entry"
}

// isTimeNowCall reports whether an expression is a direct call to time.Now, the freshly-read wall
// clock this rule forbids. An injected clock (now()) or a chosen time (ago(3)) is a different
// expression and passes, because it is a decision rather than a race.
func isTimeNowCall(expr ast.Expr) bool {
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "time" && sel.Sel.Name == "Now"
}

// repoRoot returns the module root by walking up to go.mod, so the scan covers the whole module.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	for {
		if matches, _ := filepath.Glob(filepath.Join(dir, "go.mod")); len(matches) > 0 {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above the test directory")
		}
		dir = parent
	}
}
