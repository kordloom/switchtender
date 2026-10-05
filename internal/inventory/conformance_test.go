package inventory

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

// The conformance corpus is the guard the inventory engine decision rests on: wherever the native
// engine imitates Ansible, a test runs both and fails on any disagreement. Every document under
// testdata/conformance is resolved by the native engine and by the real ansible-inventory, and the
// two listings must match host for host, group for group, and variable for variable, value and
// type.
//
// A document named needs-ansible-* must be refused by the native engine as needing Ansible, and one
// named invalid-* must be refused as invalid by the native engine and fail to parse in Ansible too.
//
// CI runs this against every ansible-core release in TestedAnsibleCore, one job each, with
// SWITCHTENDER_ANSIBLE_CORE naming the release the job installed, so a job that silently tested
// some other version fails instead of passing.

const (
	// conformanceDir holds the corpus.
	conformanceDir = "testdata/conformance"
	// ansibleCoreEnv names the ansible-core release a CI job installed.
	ansibleCoreEnv = "SWITCHTENDER_ANSIBLE_CORE"
	// requireFullSuiteEnv turns a missing ansible-inventory from a skip into a failure.
	requireFullSuiteEnv = "SWITCHTENDER_REQUIRE_FULL_SUITE"
)

// requiredQuirks are the Ansible behaviors the decision names, each pinned by at least one corpus
// document whose name starts with the key. Deleting the document that pins one fails the test.
var requiredQuirks = map[string]string{
	"ini-host-var-typing":  "INI host variables typed with literal_eval",
	"ini-group-var-typing": "INI :vars values, typed the same way as host variables",
	"ini-ranges":           "host ranges, numeric, padded, alphabetic, and with strides",
	"ini-children":         "[group:children], including children named before they are declared",
	"precedence":           "ansible_group_priority and group depth deciding which variable wins",
	"many-groups":          "one host in many groups",
	"yaml-types":           "YAML 1.1 scalar typing as PyYAML reads it",
	"yaml-merge":           "YAML anchors, aliases, and merge keys",
	"json-static":          "the JSON static form a refreshed dynamic source is stored as",
}

// ansibleInventory returns the ansible-inventory binary and its ansible-core version, or skips when
// it is not installed, failing instead under the full suite.
func ansibleInventory(t *testing.T) (string, string) {
	t.Helper()
	bin, err := exec.LookPath("ansible-inventory")
	if err != nil {
		if os.Getenv(requireFullSuiteEnv) == "1" {
			t.Fatalf("%s is set and ansible-inventory is not on PATH", requireFullSuiteEnv)
		}
		t.Skip("ansible-inventory is not on PATH; the conformance corpus needs real Ansible")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "--version")
	cmd.Env = conformanceEnv(t.TempDir())
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("ansible-inventory --version: %v", err)
	}
	version := ParseAnsibleCoreVersion(string(out))
	if version == "" {
		t.Fatalf("could not read the ansible-core version from %q", out)
	}
	return bin, version
}

// conformanceEnv returns a clean environment for ansible-inventory: no user configuration, no
// inherited ANSIBLE_ settings, and a parse failure reported as one rather than as a warning beside
// an empty inventory.
func conformanceEnv(dir string) []string {
	cfg := filepath.Join(dir, "ansible.cfg")
	_ = os.WriteFile(cfg, nil, 0o600)
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + dir,
		"ANSIBLE_CONFIG=" + cfg,
		"ANSIBLE_INVENTORY_UNPARSED_FAILED=true",
		"ANSIBLE_NOCOLOR=1",
		"ANSIBLE_LOCAL_TEMP=" + filepath.Join(dir, "tmp"),
	}
	for _, k := range []string{"LANG", "LC_ALL", "LC_CTYPE", "SYSTEMROOT", "TMPDIR"} {
		if v := os.Getenv(k); v != "" {
			env = append(env, k+"="+v)
		}
	}
	if os.Getenv("LC_ALL") == "" && os.Getenv("LANG") == "" {
		env = append(env, "LC_ALL=C.UTF-8")
	}
	return env
}

// runAnsibleInventory lists content with the real ansible-inventory, from a file with no extension
// in a directory of its own, the way a stored inventory is materialized for a run.
func runAnsibleInventory(t *testing.T, bin, content string) ([]byte, error) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "inventory")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "-i", path, "--list")
	cmd.Dir = dir
	cmd.Env = conformanceEnv(dir)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%w: %s", err, stderr.String())
	}
	return out, nil
}

// TestConformance resolves every corpus document with both engines and requires them to agree.
func TestConformance(t *testing.T) {
	t.Parallel()
	bin, version := ansibleInventory(t)
	if want := os.Getenv(ansibleCoreEnv); want != "" && version != want {
		t.Fatalf("%s says this job installed ansible-core %s, and ansible-inventory reports %s",
			ansibleCoreEnv, want, version)
	}
	t.Logf("ansible-core %s", version)
	entries, err := os.ReadDir(conformanceDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			raw, err := os.ReadFile(filepath.Join(conformanceDir, name))
			if err != nil {
				t.Fatal(err)
			}
			content := string(raw)
			native, nerr := ResolveNative(content)
			switch {
			case strings.HasPrefix(name, "needs-ansible-"):
				if !errors.Is(nerr, ErrNeedsAnsible) {
					t.Fatalf("ResolveNative() error = %v, want ErrNeedsAnsible", nerr)
				}
				return
			case strings.HasPrefix(name, "invalid-"):
				if !errors.Is(nerr, ErrInvalidInventory) {
					t.Fatalf("ResolveNative() error = %v, want ErrInvalidInventory", nerr)
				}
				if out, err := runAnsibleInventory(t, bin, content); err == nil {
					t.Fatalf("the native engine refuses this as invalid and ansible-core %s "+
						"read it:\n%s", version, out)
				}
				return
			}
			if nerr != nil {
				t.Fatalf("ResolveNative() error = %v", nerr)
			}
			out, err := runAnsibleInventory(t, bin, content)
			if err != nil {
				t.Fatalf("ansible-inventory: %v", err)
			}
			ansible, err := ParseListing(out)
			if err != nil {
				t.Fatalf("ParseListing(ansible-inventory output): %v", err)
			}
			if diff := DiffListings(native, ansible); len(diff) > 0 {
				t.Errorf("ansible-core %s and the native engine disagree:\n  %s\nansible-inventory:\n%s",
					version, strings.Join(diff, "\n  "), out)
			}
			nd, err := native.Digest()
			if err != nil {
				t.Fatal(err)
			}
			ad, err := ansible.Digest()
			if err != nil {
				t.Fatal(err)
			}
			if nd != ad {
				t.Errorf("Digest() native %s, ansible %s", nd, ad)
			}
		})
	}
}

// TestConformanceCoversTheNamedQuirks fails when a quirk the decision names has no corpus document.
func TestConformanceCoversTheNamedQuirks(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir(conformanceDir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	for prefix, quirk := range requiredQuirks {
		if !slices.ContainsFunc(names, func(n string) bool { return strings.HasPrefix(n, prefix) }) {
			t.Errorf("no corpus document starts with %q, so nothing pins %s", prefix, quirk)
		}
	}
}

// minorOf returns the major.minor of a version such as 2.18.1.
func minorOf(version string) string {
	parts := strings.SplitN(version, ".", 3)
	if len(parts) < 2 {
		return version
	}
	return parts[0] + "." + parts[1]
}

// ciMatrix finds the ansible-core pins in the conformance job's matrix.
var ciMatrix = regexp.MustCompile(`ansible-core:\s*\[([^\]]*)\]`)

// TestConformanceMatrixMatchesTheTestedReleases reads the CI workflow and requires its conformance
// matrix to pin exactly the releases in TestedAnsibleCoreReleases, one for each minor version in
// TestedAnsibleCore, so the range doctor reports as tested is the range CI tests, and a pin is an
// exact release rather than whatever patch of a minor is newest the day the job runs.
func TestConformanceMatrixMatchesTheTestedReleases(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	m := ciMatrix.FindSubmatch(raw)
	if m == nil {
		t.Fatal("ci.yml has no ansible-core matrix for the inventory conformance job")
	}
	var pins []string
	for _, pin := range strings.Split(string(m[1]), ",") {
		pin = strings.Trim(strings.TrimSpace(pin), `"'`)
		if pin == "" {
			continue
		}
		if strings.Count(pin, ".") != 2 || strings.ContainsAny(pin, "*x") {
			t.Errorf("ci.yml pins ansible-core %q, want an exact release such as 2.18.19", pin)
		}
		pins = append(pins, pin)
	}
	if diff := cmp.Diff(TestedAnsibleCoreReleases, pins); diff != "" {
		t.Errorf("ci.yml's ansible-core pins differ from TestedAnsibleCoreReleases (-want +got):\n%s",
			diff)
	}
	var minors []string
	for _, release := range TestedAnsibleCoreReleases {
		minors = append(minors, minorOf(release))
	}
	if diff := cmp.Diff(TestedAnsibleCore, minors); diff != "" {
		t.Errorf("TestedAnsibleCoreReleases covers minors that differ from TestedAnsibleCore "+
			"(-want +got):\n%s", diff)
	}
}
