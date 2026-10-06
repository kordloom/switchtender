//go:build !windows

package project

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestInstallGalaxyUsesTheLocatedCommand pins that a project's requirements are installed by the
// ansible-galaxy the syncer is told to use, such as the managed runtime's, and not the one on PATH.
func TestInstallGalaxyUsesTheLocatedCommand(t *testing.T) {
	t.Parallel()
	bin := t.TempDir()
	logPath := filepath.Join(bin, "calls.log")
	stub := filepath.Join(bin, "ansible-galaxy")
	script := "#!/bin/sh\necho \"$*\" >> '" + logPath + "'\n"
	if err := os.WriteFile(stub, []byte(script), 0o700); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	syncer, err := NewSyncer(t.TempDir(), WithGalaxyCommand(func() (string, error) {
		return stub, nil
	}))
	if err != nil {
		t.Fatalf("NewSyncer() error = %v", err)
	}
	checkout := t.TempDir()
	if err := os.WriteFile(filepath.Join(checkout, "requirements.yml"),
		[]byte("roles:\n  - src: geerlingguy.nginx\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	roles, _, err := syncer.installGalaxy(checkout)
	if err != nil || !roles {
		t.Fatalf("installGalaxy() = %v, %v, want the roles installed", roles, err)
	}
	got, err := os.ReadFile(logPath)
	if err != nil || !strings.HasPrefix(string(got), "role install -r ") {
		t.Errorf("the located ansible-galaxy ran as %q (err %v), want a role install", got, err)
	}
}

// TestInstallGalaxyFailsWhenTheLocatedAnsibleCannotBeUsed pins that a project's requirements are
// not installed by some other ansible-galaxy when the Ansible selected cannot be used: the install
// fails with the reason instead.
func TestInstallGalaxyFailsWhenTheLocatedAnsibleCannotBeUsed(t *testing.T) {
	t.Parallel()
	broken := errors.New("the managed runtime in use cannot be used")
	syncer, err := NewSyncer(t.TempDir(), WithGalaxyCommand(func() (string, error) {
		return "", broken
	}))
	if err != nil {
		t.Fatalf("NewSyncer() error = %v", err)
	}
	checkout := t.TempDir()
	if err := os.WriteFile(filepath.Join(checkout, "requirements.yml"),
		[]byte("roles:\n  - src: geerlingguy.nginx\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if _, _, err := syncer.installGalaxy(checkout); !errors.Is(err, broken) {
		t.Errorf("installGalaxy() error = %v, want %v", err, broken)
	}
}
