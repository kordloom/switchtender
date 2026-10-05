package policy

import (
	"os"
	"path/filepath"
	"testing"
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
	if _, err := NewFileStore(policyFile); err == nil {
		t.Fatalf("NewFileStore loaded a rego module at %q, which is outside the policy directory %q; "+
			"a files entry that climbs out with .. (or an absolute path) must be refused", outside, policyDir)
	}

	// An absolute path is refused the same way.
	absDoc := "rego:\n  - name: absolute\n    package: switchtender\n    files: [\"" + outside + "\"]\n"
	if err := os.WriteFile(policyFile, []byte(absDoc), 0o600); err != nil {
		t.Fatalf("write policy file: %v", err)
	}
	if _, err := NewFileStore(policyFile); err == nil {
		t.Fatalf("NewFileStore loaded an absolute rego module path %q, which must be refused", outside)
	}

	// Negative control: a files entry inside the policy directory still loads.
	if err := os.WriteFile(filepath.Join(policyDir, "inside.rego"),
		[]byte("package switchtender\n\ndeny contains \"x\" if {\n\tfalse\n}\n"), 0o600); err != nil {
		t.Fatalf("write inside module: %v", err)
	}
	okDoc := "rego:\n  - name: inside\n    package: switchtender\n    files: [\"inside.rego\"]\n"
	if err := os.WriteFile(policyFile, []byte(okDoc), 0o600); err != nil {
		t.Fatalf("write policy file: %v", err)
	}
	if _, err := NewFileStore(policyFile); err != nil {
		t.Fatalf("NewFileStore refused a module inside the policy directory: %v", err)
	}
}
