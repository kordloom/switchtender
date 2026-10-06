//go:build !windows

package ansibleruntime

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// realInstallEnv opts into the tests that install a real runtime, from the package index or from
// the wheel directory pip is configured with. The full-suite switch opts in too.
const realInstallEnv = "SWITCHTENDER_ANSIBLE_RUNTIME_TEST"

// realWheels returns the wheel directory a real install reads instead of PyPI: the one the CI
// mirror's image points pip at, since an install no longer reads pip's environment, and empty
// elsewhere.
func realWheels() string {
	if os.Getenv("PIP_NO_INDEX") == "" {
		return ""
	}
	return os.Getenv("PIP_FIND_LINKS")
}

// requireRealInstall skips a real install unless it was opted into, so an offline go test does
// not reach for a package index.
func requireRealInstall(t *testing.T) {
	t.Helper()
	if os.Getenv(realInstallEnv) != "1" && os.Getenv("SWITCHTENDER_REQUIRE_FULL_SUITE") != "1" {
		t.Skip("a real install reads a package index or a wheel directory: set " + realInstallEnv +
			"=1 to run it")
	}
}

// TestInstallsARealRuntime installs the newest supported ansible-core for real into a temporary
// directory, with pip in hash-checking mode, and shows it is what a locator with nothing configured
// runs: its ansible-playbook runs a play and its ansible-inventory reads an inventory, each as a
// separate program. A second install only verifies it.
func TestInstallsARealRuntime(t *testing.T) {
	t.Parallel()
	requireRealInstall(t)
	ctx := context.Background()
	lock, err := LockFor("")
	if err != nil {
		t.Fatalf("LockFor() error = %v", err)
	}
	root := filepath.Join(t.TempDir(), "ansible")
	var out bytes.Buffer
	res, err := Install(ctx, InstallOptions{Root: root, Out: &out, Wheels: realWheels()})
	if err != nil {
		t.Fatalf("Install() error = %v\n%s", err, out.String())
	}
	if res.AlreadyInstalled || res.Runtime.Release != lock.Release || !res.Runtime.Current {
		t.Fatalf("Install() = %+v, want a fresh, current %s", res.Runtime, lock.Release)
	}
	cmds := NewLocator("", root).Locate()
	if cmds.Source != SourceManaged || cmds.Release != lock.Release {
		t.Fatalf("Locate() = %+v, want the managed runtime", cmds)
	}
	home := t.TempDir()
	env := append(os.Environ(), "HOME="+home, "ANSIBLE_LOCAL_TEMP="+filepath.Join(home, "tmp"),
		"ANSIBLE_NOCOLOR=1", "ANSIBLE_CONFIG="+filepath.Join(home, "none.cfg"))
	playbook := filepath.Join(home, "play.yml")
	if err := os.WriteFile(playbook, []byte("- hosts: localhost\n  gather_facts: false\n"+
		"  tasks:\n    - ansible.builtin.debug:\n        msg: the managed runtime ran this\n"),
		0o600); err != nil {
		t.Fatalf("write playbook: %v", err)
	}
	pb, err := cmds.Path("ansible-playbook")
	if err != nil {
		t.Fatalf("Path(ansible-playbook) error = %v", err)
	}
	play := exec.CommandContext(ctx, pb, "-i", "localhost,", "-c", "local", playbook)
	play.Env, play.Dir = env, home
	got, err := play.CombinedOutput()
	if err != nil || !strings.Contains(string(got), "the managed runtime ran this") {
		t.Fatalf("ansible-playbook from the managed runtime: %v\n%s", err, got)
	}
	inv := filepath.Join(home, "hosts.ini")
	if err := os.WriteFile(inv, []byte("[web]\nweb1 port=8080\n"), 0o600); err != nil {
		t.Fatalf("write inventory: %v", err)
	}
	ai, err := cmds.Path("ansible-inventory")
	if err != nil {
		t.Fatalf("Path(ansible-inventory) error = %v", err)
	}
	list := exec.CommandContext(ctx, ai, "-i", inv, "--list")
	list.Env, list.Dir = env, home
	got, err = list.Output()
	if err != nil || !strings.Contains(string(got), `"web1"`) {
		t.Fatalf("ansible-inventory from the managed runtime: %v\n%s", err, got)
	}
	again, err := Install(ctx, InstallOptions{Root: root, Wheels: realWheels()})
	if err != nil || !again.AlreadyInstalled {
		t.Fatalf("a second Install() = %+v, %v, want it already installed", again, err)
	}
}

// TestARealInstallRefusesATamperedHash changes every hash the lock records for ansible-core and
// shows real pip refuses the install, nothing is left behind, and no runtime is made current.
func TestARealInstallRefusesATamperedHash(t *testing.T) {
	t.Parallel()
	requireRealInstall(t)
	lock, err := LockFor("")
	if err != nil {
		t.Fatalf("LockFor() error = %v", err)
	}
	content := string(lock.Content)
	for _, r := range lock.Requirements {
		if normalizeName(r.Name) != "ansible-core" {
			continue
		}
		for _, h := range r.Hashes {
			flipped := "0"
			if h[0] == '0' {
				flipped = "1"
			}
			content = strings.ReplaceAll(content, h, flipped+h[1:])
		}
	}
	tampered, err := ParseLock([]byte(content))
	if err != nil {
		t.Fatalf("ParseLock(tampered) error = %v", err)
	}
	root := t.TempDir()
	_, err = Install(context.Background(), InstallOptions{Root: root, lock: tampered,
		Wheels: realWheels()})
	if !errors.Is(err, ErrHashMismatch) {
		t.Fatalf("Install() with a tampered hash error = %v, want %v", err, ErrHashMismatch)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("ReadDir() error = %v", err)
	}
	for _, e := range entries {
		if e.Name() != ".lock" {
			t.Errorf("a refused install left %s", filepath.Join(root, e.Name()))
		}
	}
	if cur, err := Current(root); cur != nil || err != nil {
		t.Errorf("a refused install made %+v current (err %v)", cur, err)
	}
}
