package audit_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

// loomsealRepoEnv points the cross-check at a loomseal checkout other than the sibling directory.
const loomsealRepoEnv = "SWITCHTENDER_LOOMSEAL_REPO"

// loomsealRepo locates the loomseal checkout beside this one, skipping the test when it is absent so
// the suite still runs for someone who cloned only this repository.
//
// A sibling checkout is the ordinary case but not the only one. The checkout has to sit at the exact
// tag go.mod names for the cross-check to prove anything, so a developer whose loomseal work is ahead
// of the released tag could not run this suite at all: the cross-verification stopped being exercised
// on the machine most likely to be changing the format, which is the one place it matters most.
// Pointing this at a worktree parked on the pinned tag keeps the check runnable while the sibling
// moves ahead.
func loomsealRepo(t *testing.T) string {
	t.Helper()
	repo := os.Getenv(loomsealRepoEnv)
	if repo == "" {
		wd, err := os.Getwd()
		if err != nil {
			t.Fatalf("getwd: %v", err)
		}
		repo = filepath.Join(wd, "..", "..", "..", "loomseal")
	}
	if _, err := os.Stat(filepath.Join(repo, "go.mod")); err != nil {
		t.Skip("loomseal checkout not found beside this repository; set " + loomsealRepoEnv +
			" to point at one")
	}
	requireCheckoutMatchesModule(t, repo)
	return repo
}

// requireCheckoutMatchesModule fails when the loomseal checkout is not the version this module
// depends on.
//
// The cross-verification runs the verifier out of the checkout while the product compiles against
// the published module. Those were the same thing while a replace directive pointed one at the
// other. They are no longer, so a commit in the checkout that is not yet released would have this
// suite checking the product against a verifier nobody has, quietly, and reporting agreement with
// something that is not what ships. Saying so is the whole value of the cross-check.
func requireCheckoutMatchesModule(t *testing.T, repo string) {
	t.Helper()
	want, err := exec.Command("go", "list", "-m", "-f", "{{.Version}}",
		"github.com/kordloom/loomseal").Output()
	if err != nil {
		t.Fatalf("read the required loomseal version: %v", err)
	}
	version := strings.TrimSpace(string(want))

	tagged, err := exec.Command("git", "-C", repo, "rev-list", "-n1", version).Output()
	if err != nil {
		t.Fatalf("the loomseal checkout has no %s tag, so it cannot be the version this module "+
			"depends on: %v", version, err)
	}
	head, err := exec.Command("git", "-C", repo, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("read the loomseal checkout HEAD: %v", err)
	}
	if strings.TrimSpace(string(tagged)) != strings.TrimSpace(string(head)) {
		t.Fatalf("the loomseal checkout is at %s but this module depends on %s (%s), so the "+
			"cross-check would compare this product against a verifier it does not ship with",
			strings.TrimSpace(string(head))[:12], version, strings.TrimSpace(string(tagged))[:12])
	}
}
