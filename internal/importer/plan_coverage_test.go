package importer

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// planEnumerationSites are the functions that must handle every object kind a Plan carries, each
// with the file it lives in and the harm its omission causes. They are the hand-lists this guard
// exists to hold together: a kind added to Plan but missed in one of them fails here by name
// instead of failing an operator later.
var planEnumerationSites = []struct {
	// File is the source file, relative to this package or the module root.
	File string
	// Func is the function that enumerates the Plan's kinds.
	Func string
	// Harm is what silently goes wrong when the new kind is missing there.
	Harm string
}{
	{"importer.go", "objects", "the kind does not count toward import success, so a document of only that kind refuses as nothing recognized"},
	{"report.go", "Report", "the migration summary undercounts what comes across while still looking complete"},
	{"apply.go", "Apply", "the kind is counted as created but never persisted"},
	{"../../cmd/cmd_import.go", "reportPlan", "the CLI report omits the kind an operator is reviewing"},
	{"../../cmd/cmd_import.go", "applyPlan", "the CLI wires no store for the kind, so applying drops it"},
	{"../../internal/server/import_handlers.go", "importHandler", "the API response hides the kind from the UI"},
}

// TestEveryPlanKindIsEnumeratedEverywhere walks each enumeration site's source and requires every
// exported slice field of Plan except Warnings to be named inside the enumerating function.
//
// The importer seam for a new SOURCE is uniform, but a new object KIND fans out across count,
// report, apply, CLI, and API by hand. Each site is individually reasonable and the omission is
// only visible in aggregate, exactly the shape that let policies restore invisibly in backup.
func TestEveryPlanKindIsEnumeratedEverywhere(t *testing.T) {
	t.Parallel()
	var kinds []string
	typ := reflect.TypeOf(Plan{})
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if !f.IsExported() || f.Type.Kind() != reflect.Slice || f.Name == "Warnings" {
			continue
		}
		kinds = append(kinds, f.Name)
	}
	if len(kinds) < 6 {
		t.Fatalf("only %d object kinds found on Plan; the reflection walk is broken", len(kinds))
	}

	for _, site := range planEnumerationSites {
		body := funcSource(t, site.File, site.Func)
		for _, kind := range kinds {
			if !strings.Contains(body, kind) {
				t.Errorf("%s in %s never names Plan.%s: %s", site.Func, site.File, kind, site.Harm)
			}
		}
	}
}

// funcSource returns the source text of one function in one file, failing loudly when either is
// missing so a renamed site cannot turn this guard into a scan over nothing.
func funcSource(t *testing.T, file, name string) string {
	t.Helper()
	src, err := os.ReadFile(filepath.Clean(file))
	if err != nil {
		t.Fatalf("read %s: %v (if the file moved, update planEnumerationSites)", file, err)
	}
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, file, src, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	for _, decl := range parsed.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != name || fn.Body == nil {
			continue
		}
		return string(src[fset.Position(fn.Body.Lbrace).Offset:fset.Position(fn.Body.Rbrace).Offset])
	}
	t.Fatalf("%s has no function %s (if it was renamed, update planEnumerationSites)", file, name)
	return ""
}
