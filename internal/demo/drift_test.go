package demo

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"gopkg.in/yaml.v3"
)

// driftPlay is the shape of assets/drift.yml a test needs: the play's name and the check mode it is
// pinned to.
type driftPlay struct {
	// Name is the play name the run output shows.
	Name string `yaml:"name"`
	// CheckMode is the play's check_mode keyword, the pin that keeps the check from writing.
	CheckMode bool `yaml:"check_mode"`
	// Tasks are the play's tasks, read only to confirm the check still compares files.
	Tasks []map[string]any `yaml:"tasks"`
}

// readDriftPlay returns the single play assets/drift.yml declares.
func readDriftPlay(t *testing.T) driftPlay {
	t.Helper()
	body, err := assets.ReadFile("assets/drift.yml")
	if err != nil {
		t.Fatalf("read assets/drift.yml: %v", err)
	}
	var plays []driftPlay
	if err := yaml.Unmarshal(body, &plays); err != nil {
		t.Fatalf("parse assets/drift.yml: %v", err)
	}
	if len(plays) != 1 {
		t.Fatalf("assets/drift.yml holds %d plays, want one", len(plays))
	}
	return plays[0]
}

// TestDriftCheckIsPinnedToCheckMode pins the keyword that keeps the drift check from erasing the
// drift it reports.
//
// The check places the desired file on each host, so outside check mode it overwrites every state
// file with the desired one. The seeder submits it as a dry run, which covers the demo's own run,
// and covers nothing else: started by hand the play reported the fleet's drift and destroyed it in
// the same pass, and reported a fleet in perfect sync on the next. The play-level pin holds however
// the play is started, including from a copy somebody lifts out of this repository.
func TestDriftCheckIsPinnedToCheckMode(t *testing.T) {
	t.Parallel()
	play := readDriftPlay(t)
	if play.Name == "" {
		t.Error("the drift play has no name, so its runs list under a blank heading")
	}
	if !play.CheckMode {
		t.Error("assets/drift.yml does not pin check_mode, so a run without --check erases the " +
			"demo's drift")
	}
	if len(play.Tasks) == 0 {
		t.Fatal("assets/drift.yml declares no tasks, so the drift page would report nothing at all")
	}
	for i, task := range play.Tasks {
		if _, ok := task["copy"]; !ok {
			t.Errorf("task %d does not compare a file, so its result is not drift", i)
		}
		if mode, ok := task["check_mode"]; ok && mode != true {
			t.Errorf("task %d turns check mode back off, which lets the check write", i)
		}
	}
}

// writeDemoAssetTree writes the embedded assets and the per-host configuration state into dir, the
// tree the drift check runs against. It skips the sample repositories, which the check never reads
// and which materialize turns into git working copies.
func writeDemoAssetTree(t *testing.T, dir string) {
	t.Helper()
	err := fs.WalkDir(assets, "assets", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel("assets", path)
		if err != nil {
			return err
		}
		if rel == "repos" {
			return fs.SkipDir
		}
		target := filepath.Join(dir, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o750)
		}
		body, err := assets.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, body, 0o600)
	})
	if err != nil {
		t.Fatalf("write demo assets: %v", err)
	}
	if err := writeFleetState(dir); err != nil {
		t.Fatalf("writeFleetState() error = %v", err)
	}
}

// digestTree returns a sha256 per file under root, keyed by path relative to root, so two states of
// the same tree can be compared as a whole.
func digestTree(t *testing.T, root string) map[string]string {
	t.Helper()
	digests := make(map[string]string)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		body, err := os.ReadFile(path) //nolint:gosec // A path this test just wrote.
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(body)
		digests[rel] = hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		t.Fatalf("digest %s: %v", root, err)
	}
	return digests
}

// recapChanged reads an ansible-playbook recap and returns the changed task count per host, which
// is the number the drift page shows for that host.
func recapChanged(t *testing.T, output string) map[string]int {
	t.Helper()
	line := regexp.MustCompile(`^(\S+)\s+:\s+ok=\d+\s+changed=(\d+)`)
	changed := make(map[string]int)
	inRecap := false
	for _, text := range strings.Split(output, "\n") {
		if strings.HasPrefix(text, "PLAY RECAP") {
			inRecap = true
			continue
		}
		if !inRecap {
			continue
		}
		match := line.FindStringSubmatch(strings.TrimSpace(text))
		if match == nil {
			continue
		}
		count, err := strconv.Atoi(match[2])
		if err != nil {
			t.Fatalf("parse changed count from %q: %v", text, err)
		}
		changed[match[1]] = count
	}
	return changed
}

// TestDriftCheckReportsDriftWithoutErasingIt runs the real playbook the demo runs, without asking
// for check mode, and pins both halves of what has to be true afterward: the drift is reported, and
// the files that carry it are untouched.
//
// This is the regression that pinning check_mode exists to prevent. Reporting alone is not enough,
// because the version that erased the drift reported it correctly on the pass that erased it.
func TestDriftCheckReportsDriftWithoutErasingIt(t *testing.T) {
	t.Parallel()
	bin, err := exec.LookPath("ansible-playbook")
	if err != nil {
		t.Skip("no ansible-playbook to run the drift check")
	}
	dir := t.TempDir()
	writeDemoAssetTree(t, dir)
	before := digestTree(t, filepath.Join(dir, "state"))

	// No --check, which is how a visitor holding these assets would start it.
	cmd := exec.Command(bin, "-i", "inv.ini", "drift.yml")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("ansible-playbook error = %v, output:\n%s", err, out)
	}

	want := make(map[string]int, len(fleetHosts))
	for _, h := range fleetHosts {
		want[h.Name] = len(h.Stale)
	}
	if diff := cmp.Diff(want, recapChanged(t, string(out)), cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("reported drift mismatch (-want +got):\n%s\noutput:\n%s", diff, out)
	}
	if diff := cmp.Diff(before, digestTree(t, filepath.Join(dir, "state")),
		cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("the check rewrote the fleet state it was reading (-before +after):\n%s", diff)
	}

	// A second pass has to find the same drift. The failure this guards against reported nothing
	// the second time, which reads on the drift page as a fleet that quietly repaired itself.
	second := exec.Command(bin, "-i", "inv.ini", "drift.yml")
	second.Dir = dir
	out, err = second.CombinedOutput()
	if err != nil {
		t.Fatalf("second ansible-playbook error = %v, output:\n%s", err, out)
	}
	if diff := cmp.Diff(want, recapChanged(t, string(out)), cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("the second pass found different drift (-want +got):\n%s", diff)
	}
}

// TestDriftCheckAgreesUnderCheckMode pins that the demo's own invocation, which does pass check
// mode, leaves the same files alone and reports the same counts. The two invocations agreeing is
// what lets the page say the number came from the check rather than from how it was started.
func TestDriftCheckAgreesUnderCheckMode(t *testing.T) {
	t.Parallel()
	bin, err := exec.LookPath("ansible-playbook")
	if err != nil {
		t.Skip("no ansible-playbook to run the drift check")
	}
	dir := t.TempDir()
	writeDemoAssetTree(t, dir)
	before := digestTree(t, filepath.Join(dir, "state"))

	cmd := exec.Command(bin, "-i", "inv.ini", "drift.yml", "--check")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("ansible-playbook --check error = %v, output:\n%s", err, out)
	}

	want := make(map[string]int, len(fleetHosts))
	for _, h := range fleetHosts {
		want[h.Name] = len(h.Stale)
	}
	got := recapChanged(t, string(out))
	if diff := cmp.Diff(want, got, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("reported drift under --check mismatch (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(before, digestTree(t, filepath.Join(dir, "state")),
		cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("the check rewrote the fleet state under --check (-before +after):\n%s", diff)
	}
	total := 0
	for _, count := range got {
		total += count
	}
	if total == 0 {
		t.Error("the check reported no drift on any host, so the drift page has nothing to show")
	}
}
