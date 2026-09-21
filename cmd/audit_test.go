package cmd

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// TestEveryCLIMutationIsAudited fails when a command-line mutation reaches a store without first
// recording an audit entry.
//
// Every mutation over HTTP is recorded by the auth middleware, so the API cannot forget. The command
// line reaches the same stores directly and had no such chokepoint: creating an admin account and
// minting a token, the two most security-relevant operations in the product, left no trace at all
// while the chain verified perfectly. This walks the command sources so a mutation added later is
// caught here rather than by an auditor noticing a silence.
func TestEveryCLIMutationIsAudited(t *testing.T) {
	t.Parallel()
	// Methods that change stored state. A read is not listed and needs no entry.
	mutators := map[string]bool{"Save": true, "Delete": true}
	// Files whose mutations are exempt, with the reason each is not an audited operator action.
	exempt := map[string]string{
		// The demo seeds a throwaway database and serves it read-only.
		"cmd_demo.go": "seeds a disposable demo database",
		// The witness runs on a machine outside the watched install and writes only its own signed
		// checkpoint file. There is no chain of the watched server here to record into, and its
		// whole purpose is memory the server cannot reach.
		"cmd_witness.go": "writes the witness's own signed checkpoint, not a watched store",
	}

	files, err := filepath.Glob("cmd_*.go")
	if err != nil {
		t.Fatalf("Glob() error = %v", err)
	}
	// A guard whose work list is empty is a guard that stopped guarding. If the command files move
	// or the naming changes, this must fail loudly here rather than pass over nothing.
	if len(files) == 0 {
		t.Fatal("Glob(cmd_*.go) matched no files: the command sources moved and this guard is " +
			"scanning nothing")
	}
	fset := token.NewFileSet()
	audited := 0
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		if why, ok := exempt[filepath.Base(path)]; ok {
			t.Logf("%s exempt: %s", filepath.Base(path), why)
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("ParseFile(%s) error = %v", path, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			fn, ok := n.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				return true
			}
			var mutates, records bool
			ast.Inspect(fn.Body, func(inner ast.Node) bool {
				call, ok := inner.(*ast.CallExpr)
				if !ok {
					return true
				}
				switch f := call.Fun.(type) {
				case *ast.SelectorExpr:
					if mutators[f.Sel.Name] {
						mutates = true
					}
				case *ast.Ident:
					if f.Name == "recordCLI" {
						records = true
					}
				}
				return true
			})
			if mutates && records {
				audited++
			}
			if mutates && !records {
				t.Errorf("%s: %s changes stored state without calling recordCLI, so the change "+
					"happens with no entry in the audit chain", filepath.Base(path), fn.Name.Name)
			}
			return true
		})
	}
	// The scan must have seen the thing it exists to check. Zero audited mutations means the
	// mutator vocabulary or the recordCLI convention changed out from under this test, and a green
	// run over a vocabulary that matches nothing is the silent pass this guard forbids elsewhere.
	if audited == 0 {
		t.Fatal("the scan found no audited mutation at all: either the mutator list or the " +
			"recordCLI convention drifted, and this guard is matching nothing")
	}
}
