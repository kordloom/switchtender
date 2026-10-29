package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// TestUpgradeTeamStartsFromThePreviousPaidRelease pins where upgrade-team's starting image comes
// from. A tag from before the paid wall pulls its public image, which still carries the paid code.
// A tag after it builds from the paid module commit its gitlink names, and a commit the local
// module does not hold stops the run before any cluster exists.
func TestUpgradeTeamStartsFromThePreviousPaidRelease(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Want       error
		WantResult previousPaid
		Flag       string
		Gitlink    string
	}{{ // Test 0: The previous tag predates the gitlink, so its public image is pulled.
		Gitlink:    "",
		WantResult: previousPaid{Image: "ghcr.io/kordloom/switchtender:1.0.0", Version: "1.0.0"},
		Want:       nil,
	}, { // Test 1: The previous tag records a commit the module holds, so it is built.
		Gitlink: "module",
		WantResult: previousPaid{
			Image: "switchtender-paid:1.0.0", Ref: "refs/tags/v1.0.0", Commit: "module",
			Version: "1.0.0",
		},
		Want: nil,
	}, { // Test 2: The previous tag records a commit the module does not hold.
		Gitlink: "0123456789abcdef0123456789abcdef01234567", Want: ErrPreviousPaidMissing,
	}, { // Test 3: An explicit image wins over the tags.
		Flag: "registry.example/switchtender-paid:0.9.0", Gitlink: "module",
		WantResult: previousPaid{Image: "registry.example/switchtender-paid:0.9.0"}, Want: nil,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			repo, moduleHead := newPaidReleaseRepo(t, test.Gitlink)
			want := test.WantResult
			if want.Commit == "module" {
				want.Commit = moduleHead
			}
			got, err := resolvePreviousPaid(test.Flag, repo)
			if !errors.Is(err, test.Want) {
				t.Fatalf("resolvePreviousPaid() error = %v, want %v", err, test.Want)
			}
			if diff := cmp.Diff(want, got); diff != "" {
				t.Errorf("mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// newPaidReleaseRepo builds a public repository with a paid module checked out at paidModule, a
// v1.0.0 tag whose tree records gitlink there ("module" for the module's own head, empty for no
// gitlink), and a working tree whose chart builds 1.1.0. It returns the repository and the
// module's head commit.
func newPaidReleaseRepo(t *testing.T, gitlink string) (string, string) {
	t.Helper()
	repo := t.TempDir()
	module := filepath.Join(repo, paidModule)
	git := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
			"GIT_AUTHOR_NAME=supertest", "GIT_AUTHOR_EMAIL=supertest@example.invalid",
			"GIT_COMMITTER_NAME=supertest", "GIT_COMMITTER_EMAIL=supertest@example.invalid")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(path, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatalf("MkdirAll() error = %v", err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("WriteFile() error = %v", err)
		}
	}
	chart := filepath.Join(repo, "deploy", "helm", "switchtender", "Chart.yaml")

	write(filepath.Join(module, "build", "go.mod"), "module example.invalid/paid\n")
	git(module, "init", "-q")
	git(module, "add", ".")
	git(module, "commit", "-q", "-m", "paid")
	moduleHead := git(module, "rev-parse", "HEAD")

	git(repo, "init", "-q")
	write(chart, "appVersion: 1.0.0\n")
	git(repo, "add", "deploy")
	switch gitlink {
	case "":
	case "module":
		git(repo, "update-index", "--add", "--cacheinfo", "160000,"+moduleHead+","+paidModule)
	default:
		git(repo, "update-index", "--add", "--cacheinfo", "160000,"+gitlink+","+paidModule)
	}
	git(repo, "commit", "-q", "-m", "one")
	git(repo, "tag", "v1.0.0")
	write(chart, "appVersion: 1.1.0\n")
	git(repo, "commit", "-q", "-am", "two")
	return repo, moduleHead
}
