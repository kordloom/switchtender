package importer

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// unfuzzedParsers names a From* entry point that deliberately has no fuzz target, with the reason.
// It is empty on purpose: every one of these reads a file somebody else wrote, which is the whole
// definition of the input a fuzzer exists for, so an entry here needs an argument rather than a
// shrug.
var unfuzzedParsers = map[string]string{}

// TestEveryImporterEntryPointIsFuzzed pins the one thing the fuzz-matrix guard structurally cannot
// see.
//
// The matrix guard in the repository root holds the workflow's list to the fuzz functions that
// exist, so a target written and never run fails there. It is blind in the other direction: a
// parser with no fuzz function at all is invisible to it, because there is nothing for the matrix
// to be missing. That blindness was real, not theoretical. FromChef, FromPuppet, and FromCron each
// parsed an uploaded file with no fuzz target, and two of them shipped that way, while the four
// importers written earlier all had one.
//
// Every From* here takes bytes a stranger produced and hands them to a decoder: three of them pick
// between several shapes by what happens to parse, and several walk fields by index. That is the
// exact surface a fuzzer covers and review does not.
func TestEveryImporterEntryPointIsFuzzed(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package: %v", err)
	}

	parsers := map[string]string{} // From name -> file
	fuzzers := map[string]bool{}
	fset := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") {
			continue
		}
		file, perr := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if perr != nil {
			continue // A file that does not parse is the build's problem, not this test's.
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || !fn.Name.IsExported() {
				continue
			}
			switch {
			case strings.HasPrefix(fn.Name.Name, "Fuzz"):
				fuzzers[fn.Name.Name] = true
			case strings.HasPrefix(fn.Name.Name, "From"):
				parsers[fn.Name.Name] = name
			}
		}
	}

	if len(parsers) < 5 {
		t.Fatalf("only %d From* entry points found; the scan is matching too little and this "+
			"guard would pass over an unfuzzed parser", len(parsers))
	}
	if len(fuzzers) == 0 {
		t.Fatal("no Fuzz functions found in this package, so the guard is asserting nothing")
	}

	for name, file := range parsers {
		if reason, deliberate := unfuzzedParsers[name]; deliberate {
			t.Logf("%s is deliberately unfuzzed: %s", name, reason)
			continue
		}
		if !fuzzers["Fuzz"+name] {
			t.Errorf("%s in %s parses a file somebody else wrote and has no Fuzz%s: write one, or "+
				"name it in unfuzzedParsers with the reason it is safe to skip. The matrix guard "+
				"cannot catch this, because a parser with no target is invisible to a list of "+
				"targets.", name, file, name)
		}
	}
}
