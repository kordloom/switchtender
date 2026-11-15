//go:build !windows

package project

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
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

// TestOnlyASyncForARunInstallsDependencies pins the install switch. A requirements file names code
// from anywhere, and ansible-galaxy runs it, so a project's dependencies are installed only by the
// syncs that prepare a run being executed: Sync and SyncRef, which a claimed run calls. Fetch and
// FetchRef, which the approval gate and a held run's pin call before anyone approves, move the
// checkout and report the commit and install nothing. A counting ansible-galaxy stands in.
func TestOnlyASyncForARunInstallsDependencies(t *testing.T) {
	t.Parallel()
	const ref = "refs/pull/7/head"
	tests := []struct {
		// Call is the sync made, returning the commit it reports.
		Call func(s *Syncer, p *Project) (string, error)
		// WantInstalls is whether ansible-galaxy ran.
		WantInstalls bool
	}{{ // Test 0: The gate's fetch installs nothing.
		Call: func(s *Syncer, p *Project) (string, error) { return s.Fetch(p, "") },
	}, { // Test 1: The gate's fetch of a ref installs nothing.
		Call: func(s *Syncer, p *Project) (string, error) { return s.FetchRef(p, "", ref) },
	}, { // Test 2: A run's sync installs.
		Call: func(s *Syncer, p *Project) (string, error) {
			wt, err := s.Sync(p, "")
			defer wt.Cleanup()
			if err != nil {
				return "", err
			}
			return wt.SHA, nil
		},
		WantInstalls: true,
	}, { // Test 3: A run's sync of a ref installs.
		Call: func(s *Syncer, p *Project) (string, error) {
			wt, err := s.SyncRef(p, "", ref)
			defer wt.Cleanup()
			if err != nil {
				return "", err
			}
			return wt.SHA, nil
		},
		WantInstalls: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			repo := initTestRepo(t, map[string]string{
				"site.yml":         "---\n",
				"requirements.yml": "roles:\n  - src: local.role\n",
			})
			origin, err := git.PlainOpen(repo)
			if err != nil {
				t.Fatalf("PlainOpen() error = %v", err)
			}
			head, err := origin.Head()
			if err != nil {
				t.Fatalf("Head() error = %v", err)
			}
			if err := origin.Storer.SetReference(plumbing.NewHashReference(ref,
				head.Hash())); err != nil {
				t.Fatalf("SetReference() error = %v", err)
			}
			bin := t.TempDir()
			logPath := filepath.Join(bin, "calls.log")
			stub := filepath.Join(bin, "ansible-galaxy")
			if err := os.WriteFile(stub, []byte("#!/bin/sh\necho \"$*\" >> '"+logPath+"'\n"),
				0o700); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}
			s, err := NewSyncer(t.TempDir(), WithGalaxyCommand(func() (string, error) {
				return stub, nil
			}))
			if err != nil {
				t.Fatalf("NewSyncer() error = %v", err)
			}
			p := &Project{ID: "proj_deps", RepoURL: repo, Branch: "main", InstallDeps: true}
			sha, err := test.Call(s, p)
			if err != nil || sha != head.Hash().String() {
				t.Fatalf("sync = %q, %v, want the commit %s", sha, err, head.Hash())
			}
			calls, err := os.ReadFile(logPath)
			if installed := err == nil && len(calls) > 0; installed != test.WantInstalls {
				t.Errorf("ansible-galaxy ran %q (err %v), want it run %v", calls, err,
					test.WantInstalls)
			}
		})
	}
}
