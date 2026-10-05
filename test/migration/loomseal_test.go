package migration

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// loomsealRepoEnv points the suite at the LoomSeal checkout whose verifier checks every exported
// chain, the variable the audit package's cross-verification reads.
const loomsealRepoEnv = "SWITCHTENDER_LOOMSEAL_REPO"

// loomsealBuild is the verifier binary built once per test process, or why it could not be.
var loomsealBuild struct {
	// once guards the build.
	once sync.Once
	// path is the built binary.
	path string
	// skip is why the verifier is unavailable on a machine allowed to lack it.
	skip string
	// fail is why the verifier could not be built where it is present.
	fail string
}

// loomsealBinary returns the LoomSeal verifier built from the checkout the module depends on.
//
// The check shells out to the reference verifier rather than importing one, because the claim is
// that a relying party's own tool accepts what this install exports. A verifier compiled into this
// suite could drift toward whatever the product emits.
func loomsealBinary(t *testing.T) string {
	t.Helper()
	loomsealBuild.once.Do(func() {
		repo := os.Getenv(loomsealRepoEnv)
		if repo == "" {
			repo = filepath.Join("..", "..", "..", "loomseal")
		}
		if _, err := os.Stat(filepath.Join(repo, "go.mod")); err != nil {
			loomsealBuild.skip = "no LoomSeal checkout was found; set " + loomsealRepoEnv
			return
		}
		want, err := exec.Command("go", "list", "-m", "-f", "{{.Version}}",
			"github.com/kordloom/loomseal").Output()
		if err != nil {
			loomsealBuild.fail = "read the required LoomSeal version: " + err.Error()
			return
		}
		version := strings.TrimSpace(string(want))
		tagged, terr := exec.Command("git", "-C", repo, "rev-list", "-n1", version).Output()
		head, herr := exec.Command("git", "-C", repo, "rev-parse", "HEAD").Output()
		if terr != nil || herr != nil ||
			strings.TrimSpace(string(tagged)) != strings.TrimSpace(string(head)) {
			loomsealBuild.fail = "the LoomSeal checkout is not at " + version + ", the version this " +
				"module depends on, so it would check the product against a verifier it does not ship with"
			return
		}
		dir, err := os.MkdirTemp("", "stmig-loomseal-")
		if err != nil {
			loomsealBuild.fail = "create the verifier directory: " + err.Error()
			return
		}
		bin := filepath.Join(dir, "loomseal")
		build := exec.Command("go", "build", "-o", bin, ".")
		build.Dir = repo
		if out, err := build.CombinedOutput(); err != nil {
			loomsealBuild.fail = "build the LoomSeal verifier: " + err.Error() + "\n" + string(out)
			return
		}
		loomsealBuild.path = bin
	})
	switch {
	case loomsealBuild.fail != "":
		t.Fatal(loomsealBuild.fail)
	case loomsealBuild.skip != "":
		standDownOrFail(t, "%s", loomsealBuild.skip)
	}
	return loomsealBuild.path
}

// loomsealReport is the part of the verifier's JSON report the scenarios read.
type loomsealReport struct {
	// OK reports whether every check passed.
	OK bool `json:"ok"`
	// Problems lists why it did not.
	Problems []string `json:"problems"`
}

// verifyOffline runs the LoomSeal verifier over a signed bundle on disk and returns its verdict. A
// refusal is a verdict rather than a failure to run, so the report is read either way.
func verifyOffline(t *testing.T, path string) loomsealReport {
	t.Helper()
	out, err := exec.Command(loomsealBinary(t), "verify", "--json", path).Output()
	if err != nil && len(out) == 0 {
		var stderr string
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = string(ee.Stderr)
		}
		t.Fatalf("run the LoomSeal verifier: %v\n%s", err, stderr)
	}
	var rep loomsealReport
	if err := json.Unmarshal(out, &rep); err != nil {
		t.Fatalf("the LoomSeal report is not JSON: %v\n%s", err, out)
	}
	return rep
}
