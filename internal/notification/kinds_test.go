package notification

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// TestKindsIsEveryKindConstant pins Kinds to the Kind constants it enumerates, by reading the
// source, so a kind declared and left out of Kinds cannot escape the store contract that proves
// every attachable kind loses its attachments with its object.
func TestKindsIsEveryKindConstant(t *testing.T) {
	t.Parallel()
	file, err := parser.ParseFile(token.NewFileSet(), "notification.go", nil, 0)
	if err != nil {
		t.Fatalf("parse notification.go: %v", err)
	}
	var declared []string
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range vs.Names {
				if !strings.HasPrefix(name.Name, "Kind") || i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok {
					continue
				}
				value, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatalf("unquote %s: %v", name.Name, err)
				}
				declared = append(declared, value)
			}
		}
	}
	if len(declared) == 0 {
		t.Fatal("no Kind constants were found, so this guard is asserting nothing")
	}
	if diff := cmp.Diff(declared, Kinds()); diff != "" {
		t.Errorf("Kinds() does not enumerate the Kind constants in order (-declared +Kinds):\n%s",
			diff)
	}
	for _, kind := range Kinds() {
		if got, err := NormalizeObjectKind(kind); err != nil || got != kind {
			t.Errorf("NormalizeObjectKind(%q) = %q, %v, want the kind itself", kind, got, err)
		}
	}
}
