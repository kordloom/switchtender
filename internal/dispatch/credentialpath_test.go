package dispatch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEveryCredentialIsOpenedThroughOnePath pins the step that is invisible when it is missing.
//
// A stored credential is not its own value. Unsealing it yields whatever was sealed, and for a
// credential backed by an external engine that is the source configuration, not the secret; the
// secret exists only after the source resolves it, and a dynamic one comes with a lease that has to
// be handed back. openCredential does all of that, and materialization has always used it.
//
// Three other places fetched and unsealed a credential by hand instead, and each failed silently in
// its own way: a registry login read a JSON config as a username, an inventory source ran with no
// credential and reported the empty result as the truth, and a passphrase-protected project key
// reached the SSH parser as the JSON wrapper it is stored in. None of them errored in a way that
// named the omission, because the value they used was a real string, just the wrong one.
//
// Sealer.Open outside that one function is the shape of the mistake, so it is what this looks for.
func TestEveryCredentialIsOpenedThroughOnePath(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package: %v", err)
	}

	fset := token.NewFileSet()
	var offenders []string
	opens := 0
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
				// The shape is an assignment whose right side unseals a credential's sealed secret
				// and whose left side keeps the plaintext. Opening one to check that it decrypts
				// and discarding the value is a different act and is left alone, as is unsealing
				// something that is not a credential, such as an inventory's content config.
				assign, isAssign := n.(*ast.AssignStmt)
				if !isAssign {
					return true
				}
				for _, rhs := range assign.Rhs {
					if !unsealsACredentialSecret(rhs) {
						continue
					}
					opens++
					if keepsPlaintext(assign.Lhs) && fn.Name.Name != "openCredential" {
						offenders = append(offenders,
							fn.Name.Name+" at "+fset.Position(assign.Pos()).String())
					}
				}
				return true
			})
		}
	}

	if opens == 0 {
		t.Fatal("no sealer.Open calls found at all, so this guard is asserting nothing: either the " +
			"sealer was renamed or credentials are opened somewhere this cannot see")
	}
	for _, o := range offenders {
		t.Errorf("%s unseals a credential itself instead of calling openCredential, so it skips "+
			"resolving the credential through its source and never receives a lease to revoke. A "+
			"credential backed by an external engine then yields its configuration rather than its "+
			"secret, and the caller uses that string as though it were the credential", o)
	}
}

// unsealsACredentialSecret reports whether an expression is sealer.Open of a credential's sealed
// secret, which is the one call that yields a value still needing its source resolved.
func unsealsACredentialSecret(expr ast.Expr) bool {
	call, isCall := expr.(*ast.CallExpr)
	if !isCall || len(call.Args) != 1 {
		return false
	}
	open, isSel := call.Fun.(*ast.SelectorExpr)
	if !isSel || open.Sel.Name != "Open" {
		return false
	}
	sealer, isSel := open.X.(*ast.SelectorExpr)
	if !isSel || sealer.Sel.Name != "sealer" {
		return false
	}
	arg, isSel := call.Args[0].(*ast.SelectorExpr)
	return isSel && arg.Sel.Name == "Secret"
}

// keepsPlaintext reports whether the first value of an unseal is bound to a name rather than
// discarded. A discarded plaintext is a decryptability check, not a use.
func keepsPlaintext(lhs []ast.Expr) bool {
	if len(lhs) == 0 {
		return false
	}
	id, isIdent := lhs[0].(*ast.Ident)
	return !isIdent || id.Name != "_"
}
