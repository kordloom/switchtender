package audit_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// runLoomSealVerify runs the loomseal verifier over a bundle and returns its JSON report.
//
// It shells out to the format's own command rather than importing a verifier, because the point of
// the check is that a relying party's tool accepts this product's output. A verifier vendored into
// this repository could drift toward whatever this repository happens to emit.
func runLoomSealVerify(t *testing.T, signed []byte) []byte {
	t.Helper()
	repo := loomsealRepo(t)
	path := filepath.Join(t.TempDir(), "bundle.loomseal.json")
	if err := os.WriteFile(path, signed, 0o600); err != nil {
		t.Fatalf("write bundle: %v", err)
	}
	cmd := exec.Command("go", "run", ".", "verify", "--json", path)
	cmd.Dir = repo
	out, err := cmd.Output()
	if err != nil {
		var stderr string
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = string(ee.Stderr)
		}
		// A refusal is a verdict, not a failure to run: the report still comes back on stdout.
		if len(out) == 0 {
			t.Fatalf("run loomseal verify: %v\n%s", err, stderr)
		}
	}
	return out
}

// loomsealRepo locates the loomseal checkout beside this one, skipping the test when it is absent so
// the suite still runs for someone who cloned only this repository.
func loomsealRepo(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	repo := filepath.Join(wd, "..", "..", "..", "loomseal")
	if _, err := os.Stat(filepath.Join(repo, "go.mod")); err != nil {
		t.Skip("loomseal checkout not found beside this repository")
	}
	return repo
}
