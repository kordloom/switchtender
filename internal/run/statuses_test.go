package run

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"testing"
)

// TestAllStatusesMatchesTheDeclaredConstants pins the enumerator to the const block itself, by
// reading the source, so the one hand-written list everything else derives from cannot drift.
//
// Every other status list in the module is derived: the lifecycle table checks it covers this set,
// and the store guards walk it against their SQL predicates. That chain is only as good as this
// root, and a ninth status declared in the const block but absent here would silently exempt
// itself from every derived guard, which is the exact quiet divergence the chain exists to stop.
func TestAllStatusesMatchesTheDeclaredConstants(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "run.go", nil, 0)
	if err != nil {
		t.Fatalf("parse run.go: %v", err)
	}
	declared := map[string]bool{}
	for _, decl := range file.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok || vs.Type == nil {
				continue
			}
			if id, ok := vs.Type.(*ast.Ident); !ok || id.Name != "Status" {
				continue
			}
			for _, v := range vs.Values {
				lit, ok := v.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				value, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatalf("unquote %s: %v", lit.Value, err)
				}
				declared[value] = true
			}
		}
	}
	if len(declared) == 0 {
		t.Fatal("no Status constants found in run.go: the scan is matching nothing")
	}

	enumerated := map[string]bool{}
	for _, st := range AllStatuses() {
		enumerated[string(st)] = true
	}
	for value := range declared {
		if !enumerated[value] {
			t.Errorf("Status %q is declared but missing from AllStatuses(): every guard derived "+
				"from the enumerator silently exempts it", value)
		}
	}
	for value := range enumerated {
		if !declared[value] {
			t.Errorf("AllStatuses() lists %q but no such Status constant is declared", value)
		}
	}
}
