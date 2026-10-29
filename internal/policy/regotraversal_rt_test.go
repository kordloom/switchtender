package policy

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/kordloom/switchtender/internal/license"
)

// TestFileStoreRefusesARegoModuleOutsideThePolicyDir pins that a rego policy's files entry cannot
// escape the policy file's directory.
//
// loadRego resolves each files entry with filepath.Join(dir, rel) and no containment check, and uses
// an absolute path as-is, so "../secret.rego" or "/etc/anything" reads a file outside the policy
// tree. The documentation says the modules are read "from paths relative to the policy file, so the
// YAML and the Rego it loads move together"; nothing confines them to that directory. A path that
// climbs out should be refused the way the engine confines every other operator-supplied path
// (host-filter length, inventory host count), rather than reading an arbitrary file and attempting
// to compile it as Rego.
func TestFileStoreRefusesARegoModuleOutsideThePolicyDir(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	// A valid Rego module placed OUTSIDE the policy directory.
	outside := filepath.Join(base, "outside.rego")
	if err := os.WriteFile(outside, []byte("package switchtender\n\ndeny contains \"x\" if {\n\tfalse\n}\n"), 0o600); err != nil {
		t.Fatalf("write outside module: %v", err)
	}
	policyDir := filepath.Join(base, "policy")
	if err := os.MkdirAll(policyDir, 0o700); err != nil {
		t.Fatalf("mkdir policy: %v", err)
	}
	policyFile := filepath.Join(policyDir, "policies.yml")
	doc := "rego:\n  - name: escaper\n    package: switchtender\n    files: [\"../outside.rego\"]\n"
	if err := os.WriteFile(policyFile, []byte(doc), 0o600); err != nil {
		t.Fatalf("write policy file: %v", err)
	}
	if _, err := NewFileStore(policyFile); err == nil || errors.Is(err, license.ErrRequiresTier) {
		t.Fatalf("NewFileStore(%q outside %q) error = %v, want the path refused before anything "+
			"compiles; a files entry that climbs out with .. (or an absolute path) must be refused",
			outside, policyDir, err)
	}

	// An absolute path is refused the same way.
	absDoc := "rego:\n  - name: absolute\n    package: switchtender\n    files: [\"" + outside + "\"]\n"
	if err := os.WriteFile(policyFile, []byte(absDoc), 0o600); err != nil {
		t.Fatalf("write policy file: %v", err)
	}
	if _, err := NewFileStore(policyFile); err == nil || errors.Is(err, license.ErrRequiresTier) {
		t.Fatalf("NewFileStore(absolute %q) error = %v, want the path refused before anything "+
			"compiles", outside, err)
	}

	// Negative control: a files entry inside the policy directory passes the path check and reaches
	// the compiler, which a build without the paid module answers with the tier refusal.
	if err := os.WriteFile(filepath.Join(policyDir, "inside.rego"),
		[]byte("package switchtender\n\ndeny contains \"x\" if {\n\tfalse\n}\n"), 0o600); err != nil {
		t.Fatalf("write inside module: %v", err)
	}
	okDoc := "rego:\n  - name: inside\n    package: switchtender\n    files: [\"inside.rego\"]\n"
	if err := os.WriteFile(policyFile, []byte(okDoc), 0o600); err != nil {
		t.Fatalf("write policy file: %v", err)
	}
	if _, err := NewFileStore(policyFile); !errors.Is(err, license.ErrRequiresTier) {
		t.Fatalf("NewFileStore(inside) error = %v, want the tier refusal the compiler gives, which "+
			"only a module that passed the path check reaches", err)
	}
}
