package tfscan

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/run"
)

// standDownOrFail skips on a machine allowed to lack something, and fails where the suite was
// demanded whole.
func standDownOrFail(t *testing.T, format string, args ...any) {
	t.Helper()
	if os.Getenv("SWITCHTENDER_REQUIRE_FULL_SUITE") == "1" {
		t.Fatalf("SWITCHTENDER_REQUIRE_FULL_SUITE is set and "+format, args...)
	}
	t.Skipf(format, args...)
}

// runIn runs a command in dir and fails the test with its output when it fails.
func runIn(t *testing.T, dir, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	// The tool's version check would call the vendor's service, and this test reaches nothing remote.
	cmd.Env = append(os.Environ(), "CHECKPOINT_DISABLE=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
}

// writeAll writes files under dir, creating the directories they sit in.
func writeAll(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		full := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
}

// moduleRepo builds a git repository holding a module that declares an external data source and
// calls a nested local module that declares another, tagged v1.0.0, and returns its path.
func moduleRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeAll(t, dir, map[string]string{
		"main.tf": "data \"external\" \"remote\" {\n  program = [\"true\"]\n}\n" +
			"module \"inner\" {\n  source = \"./nested\"\n}\n",
		"nested/main.tf": "data \"external\" \"deep\" {\n  count   = 2\n  program = [\"true\"]\n}\n",
		"clean/main.tf":  "output \"o\" {\n  value = 1\n}\n",
	})
	runIn(t, dir, "git", "init", "-q", "-b", "main")
	runIn(t, dir, "git", "-c", "user.email=test@example.invalid", "-c", "user.name=test", "add", "-A")
	runIn(t, dir, "git", "-c", "user.email=test@example.invalid", "-c", "user.name=test", "commit",
		"-q", "-m", "module")
	runIn(t, dir, "git", "tag", "v1.0.0")
	return dir
}

// TestScanReadsWhatARealInitInstalled holds the scan to the module manifest the tools themselves
// write. A remote module is installed by the tool's own get or init into .terraform/modules, and
// the scan reads it there only through the manifest's record of it. Each tool installs a git module
// from a local repository here, with no network, so the record the scan reads is the one a real
// install produces: a module downloaded and current is read, nested modules included, by the
// addresses a plan gives them, and the same copy is no longer read once the configuration names
// another ref.
//
//nolint:funlen // Test function.
func TestScanReadsWhatARealInitInstalled(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		standDownOrFail(t, "git is not on PATH, so a git module cannot be installed")
	}
	tests := []struct {
		// Tool is the tool the scan reads for.
		Tool string
		// Binary is the executable that installs the modules.
		Binary string
	}{{ // Test 0: Terraform.
		Tool: run.ToolTerraform, Binary: "terraform",
	}, { // Test 1: OpenTofu.
		Tool: run.ToolOpenTofu, Binary: "tofu",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if _, err := exec.LookPath(test.Binary); err != nil {
				standDownOrFail(t, "%s is not on PATH, so no real install can be read", test.Binary)
			}
			repo := moduleRepo(t)
			work := t.TempDir()
			source := func(ref string) string {
				return fmt.Sprintf("module \"remote\" {\n  source = \"git::file://%s?ref=%s\"\n}\n"+
					"module \"tidy\" {\n  source = \"git::file://%s//clean?ref=%s\"\n}\n",
					filepath.ToSlash(repo), ref, filepath.ToSlash(repo), ref)
			}
			writeAll(t, work, map[string]string{"main.tf": source("v1.0.0")})
			runIn(t, work, test.Binary, "get", "-no-color")

			// A real get wrote this .terraform, which stands for the tree the gate downloads into, so
			// the scan reads its manifest.
			got := Scan(os.DirFS(work), ".", Options{Tool: test.Tool, Place: "the working directory",
				TrustModuleManifest: true})
			wantFindings := []string{
				"module.remote.data.external.remote runs a program during plan " +
					"(.terraform/modules/remote/main.tf line 1)",
				"module.remote.module.inner.data.external.deep runs a program during plan " +
					"(.terraform/modules/remote/nested/main.tf line 1, once for each instance its " +
					"count makes)",
			}
			if diff := cmp.Diff(wantFindings, got.Findings, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("findings mismatch (-want +got):\n%s", diff)
			}
			if len(got.Unread) != 0 {
				t.Errorf("unread = %q, want everything a real install put down read", got.Unread)
			}
			for _, want := range []string{".terraform/modules/modules.json", "main.tf",
				".terraform/modules/tidy/clean/main.tf"} {
				if !slices.Contains(got.Inputs, want) {
					t.Errorf("inputs = %q, want %q read", got.Inputs, want)
				}
			}

			// The configuration now names a ref nobody installed, so the next init replaces the
			// copy, and the scan must not classify the plan from the copy it replaces.
			writeAll(t, work, map[string]string{"main.tf": source("v2.0.0")})
			stale := Scan(os.DirFS(work), ".", Options{Tool: test.Tool, Place: "the working directory",
				TrustModuleManifest: true})
			if stale.Classification != run.DryRunIncomplete || len(stale.Findings) != 0 {
				t.Errorf("after the ref moved: classified %q with findings %q, want incomplete with "+
					"none", stale.Classification, stale.Findings)
			}
			if !strings.Contains(strings.Join(stale.Unread, "\n"), "the copy downloaded here is of") {
				t.Errorf("unread = %q, want the stale copy named", stale.Unread)
			}
		})
	}
}
