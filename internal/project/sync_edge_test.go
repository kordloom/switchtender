package project

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

// TestNewSyncerRefusesAnUnusableCacheAndClearsDeadWorktrees covers the constructor's two jobs: it
// must fail rather than hand back a Syncer that cannot write, and it must clear the per-run
// directory so a crashed server does not accumulate dead checkouts on every restart.
func TestNewSyncerRefusesAnUnusableCacheAndClearsDeadWorktrees(t *testing.T) {
	t.Parallel()

	// Test 0: a path already occupied by a regular file cannot hold the cache.
	occupied := filepath.Join(t.TempDir(), "cache")
	if err := os.WriteFile(occupied, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if _, err := NewSyncer(occupied); err == nil {
		t.Error("NewSyncer() on a regular file = nil error, want a refusal")
	}

	// Test 1: an empty directory name is not a usable cache.
	if _, err := NewSyncer(""); err == nil {
		t.Error("NewSyncer(\"\") = nil error, want a refusal")
	}

	// Test 2: a worktree left behind by a crash is cleared on the next start.
	cache := t.TempDir()
	dead := filepath.Join(cache, runsSubdir, "proj_dead-123")
	if err := os.MkdirAll(dead, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if _, err := NewSyncer(cache); err != nil {
		t.Fatalf("NewSyncer() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(cache, runsSubdir)); !os.IsNotExist(err) {
		t.Errorf("the run directory survived the restart, err = %v", err)
	}

	// Test 3: a cache directory that does not exist yet is created.
	fresh := filepath.Join(t.TempDir(), "nested", "cache")
	if _, err := NewSyncer(fresh); err != nil {
		t.Fatalf("NewSyncer() on a new path error = %v", err)
	}
	info, err := os.Stat(fresh)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if !info.IsDir() {
		t.Errorf("NewSyncer() did not create a directory at %s", fresh)
	}
}

// TestWithGalaxyInstallsBothValues pins the constructor option against the behavior it exists to
// change. An option that stored the server and dropped the token would pass every test written
// against galaxyEnv alone, because that function is handed the fields rather than the option.
func TestWithGalaxyInstallsBothValues(t *testing.T) {
	t.Parallel()
	s, err := NewSyncer(t.TempDir(), WithGalaxy("https://hub.internal/api/galaxy/", "sekret"))
	if err != nil {
		t.Fatalf("NewSyncer() error = %v", err)
	}
	want := []string{
		"ANSIBLE_GALAXY_SERVER_LIST=switchtender",
		"ANSIBLE_GALAXY_SERVER_SWITCHTENDER_URL=https://hub.internal/api/galaxy/",
		"ANSIBLE_GALAXY_SERVER_SWITCHTENDER_TOKEN=sekret",
	}
	if diff := cmp.Diff(want, s.galaxyEnv()); diff != "" {
		t.Errorf("WithGalaxy did not reach the environment (-want +got):\n%s", diff)
	}

	// A Syncer built with no options resolves collections from the public Galaxy.
	plain, err := NewSyncer(t.TempDir())
	if err != nil {
		t.Fatalf("NewSyncer() error = %v", err)
	}
	if diff := cmp.Diff([]string(nil), plain.galaxyEnv(), cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("a Syncer with no galaxy option produced environment (-want +got):\n%s", diff)
	}
}

// TestRequirementKindsReadsBothFormats covers what decides whether ansible-galaxy is run at all.
// Reading a mapping as a role list would run a role install against a collections file and fail the
// sync; reading a role list as nothing would leave a play without the roles it imports.
func TestRequirementKindsReadsBothFormats(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name labels the requirements file shape.
		Name string
		// Body is the file content, or Missing when no file is written.
		Body string
		// Missing writes no file at all.
		Missing bool
		// WantRoles is whether a role install must run.
		WantRoles bool
		// WantCollections is whether a collection install must run.
		WantCollections bool
	}{
		{Name: "missing file", Missing: true}, // Test 0.
		{Name: "empty file", Body: ""},        // Test 1.
		{Name: "bare list is the old role format", Body: "- src: geerlingguy.nginx\n", WantRoles: true},                      // Test 2.
		{Name: "roles key", Body: "roles:\n  - src: a\n", WantRoles: true},                                                   // Test 3.
		{Name: "collections key", Body: "collections:\n  - name: c\n", WantCollections: true},                                // Test 4.
		{Name: "both keys", Body: "roles:\n  - src: a\ncollections:\n  - name: c\n", WantRoles: true, WantCollections: true}, // Test 5.
		{Name: "mapping with neither key", Body: "other: 1\n"},                                                               // Test 6.
		{Name: "a scalar declares nothing", Body: "just a string\n"},                                                         // Test 7.
		{Name: "invalid yaml declares nothing", Body: "roles: [unclosed\n"},                                                  // Test 8.
		{Name: "explicit null", Body: "~\n"},                                                                                 // Test 9.
		{Name: "empty list", Body: "[]\n", WantRoles: true},                                                                  // Test 10.
		{Name: "roles key set to null still asks for roles", Body: "roles:\n", WantRoles: true},                              // Test 11.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "requirements.yml")
			if !test.Missing {
				if err := os.WriteFile(path, []byte(test.Body), 0o600); err != nil {
					t.Fatalf("WriteFile() error = %v", err)
				}
			}
			roles, collections := requirementKinds(path)
			if roles != test.WantRoles || collections != test.WantCollections {
				t.Errorf("requirementKinds(%s) = (%v, %v), want (%v, %v)",
					test.Name, roles, collections, test.WantRoles, test.WantCollections)
			}
		})
	}
}

// TestRequirementKindsIgnoresADirectory pins that a directory named requirements.yml is read as
// declaring nothing rather than crashing the sync.
func TestRequirementKindsIgnoresADirectory(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "requirements.yml")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	roles, collections := requirementKinds(dir)
	if roles || collections {
		t.Errorf("requirementKinds(directory) = (%v, %v), want (false, false)", roles, collections)
	}
}

// TestInstallGalaxySkipsAndFails covers the dependency install's two ends. A checkout with no
// requirements file must install nothing and report nothing, and a checkout that declares
// requirements the host cannot install must return the failure rather than swallow it: a play that
// imports a role it never got is a run that fails later with a confusing message.
func TestInstallGalaxySkipsAndFails(t *testing.T) {
	// Not parallel: it replaces PATH for the process so ansible-galaxy cannot resolve.
	syncer, err := NewSyncer(t.TempDir())
	if err != nil {
		t.Fatalf("NewSyncer() error = %v", err)
	}

	// Test 0: no requirements file anywhere in the checkout is a no-op.
	bare := t.TempDir()
	roles, collections, err := syncer.installGalaxy(bare)
	if err != nil {
		t.Fatalf("installGalaxy() on a checkout with no requirements error = %v", err)
	}
	if roles || collections {
		t.Errorf("installGalaxy() = (%v, %v), want nothing installed", roles, collections)
	}

	// Test 1: a declared requirement the host cannot install is returned, not swallowed.
	checkout := t.TempDir()
	if err := os.WriteFile(filepath.Join(checkout, "requirements.yml"),
		[]byte("roles:\n  - src: geerlingguy.nginx\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	t.Setenv("PATH", t.TempDir())
	if _, _, err := syncer.installGalaxy(checkout); err == nil {
		t.Error("installGalaxy() with no ansible-galaxy on PATH = nil error, want the failure surfaced")
	} else if !strings.Contains(err.Error(), "requirements.yml") {
		t.Errorf("installGalaxy() error = %v, want the requirements file named", err)
	}
}

// TestAuthForRejectsAnUnusableKey pins the two ends of building transport auth. An empty key means
// a public remote and must not be turned into a broken auth method, and a key that is not a private
// key must fail at the parse rather than at a confusing point inside the clone.
func TestAuthForRejectsAnUnusableKey(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name labels the key material.
		Name string
		// Key is the PEM private key text.
		Key string
		// WantAuth is whether an auth method must be produced.
		WantAuth bool
		// WantErr is whether the parse must fail.
		WantErr bool
	}{
		{Name: "empty is a public remote", Key: "", WantAuth: false, WantErr: false},              // Test 0.
		{Name: "garbage", Key: "not a key", WantErr: true},                                        // Test 1.
		{Name: "a public key is not a private key", Key: "ssh-rsa AAAAB3Nz", WantErr: true},       // Test 2.
		{Name: "truncated pem", Key: "-----BEGIN OPENSSH PRIVATE KEY-----\nabc\n", WantErr: true}, // Test 3.
		{Name: "whitespace only", Key: "   \n\t", WantErr: true},                                  // Test 4.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			auth, err := authFor(test.Key)
			if test.WantErr {
				if err == nil {
					t.Fatalf("authFor(%s) = %v, want a parse failure", test.Name, auth)
				}
				if !strings.Contains(err.Error(), "parse project ssh key") {
					t.Errorf("authFor(%s) error = %v, want it named as a key parse failure", test.Name, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("authFor(%s) error = %v", test.Name, err)
			}
			if (auth != nil) != test.WantAuth {
				t.Errorf("authFor(%s) auth = %v, want present = %v", test.Name, auth, test.WantAuth)
			}
		})
	}
}

// TestBranchRefMapsTheRemoteDefault pins that an empty branch stays empty, which is what tells
// go-git to take whatever the remote's HEAD points at. A branch name turned into a reference when
// the project named none would clone a branch called "" and fail.
func TestBranchRefMapsTheRemoteDefault(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// In is the project's branch field.
		In string
		// WantRef is the reference name handed to go-git.
		WantRef plumbing.ReferenceName
	}{
		{In: "", WantRef: ""},                                  // Test 0: The remote default.
		{In: "main", WantRef: "refs/heads/main"},               // Test 1: An ordinary branch.
		{In: "release/4.2", WantRef: "refs/heads/release/4.2"}, // Test 2: A slash in the name.
		{In: "fix-Ünicode", WantRef: "refs/heads/fix-Ünicode"}, // Test 3: Non-ASCII names pass.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if diff := cmp.Diff(test.WantRef, branchRef(test.In)); diff != "" {
				t.Errorf("branchRef(%q) mismatch (-want +got):\n%s", test.In, diff)
			}
		})
	}
}

// TestMatchesProjectDetectsAStaleCheckout pins the check that decides whether a cached checkout can
// be fetched or has to be thrown away.
//
// The cached copy carries the remote and branch it was cloned with, and a fetch only ever asks that
// remote for that branch. An operator who repointed a project therefore kept running the old source
// until this check said the checkout no longer matched. Reporting a match when the project moved is
// the dangerous direction, so every mismatch here has to be seen.
func TestMatchesProjectDetectsAStaleCheckout(t *testing.T) {
	t.Parallel()
	const remote = "https://example.com/org/repo.git"

	// Test 0: a repository with no origin at all cannot be reused.
	bare, err := git.PlainInit(t.TempDir(), false)
	if err != nil {
		t.Fatalf("PlainInit() error = %v", err)
	}
	if matchesProject(bare, &Project{RepoURL: remote}) {
		t.Error("matchesProject() = true for a checkout with no origin remote")
	}

	withOrigin := func(t *testing.T, url string) *git.Repository {
		t.Helper()
		repo, err := git.PlainInit(t.TempDir(), false)
		if err != nil {
			t.Fatalf("PlainInit() error = %v", err)
		}
		if _, err := repo.CreateRemote(&config.RemoteConfig{Name: "origin", URLs: []string{url}}); err != nil {
			t.Fatalf("CreateRemote() error = %v", err)
		}
		return repo
	}

	// Test 1: a checkout taken from a different remote is stale.
	if matchesProject(withOrigin(t, "https://example.com/org/other.git"), &Project{RepoURL: remote}) {
		t.Error("matchesProject() = true after the project was repointed at another remote")
	}

	// Test 2: the same remote with no branch pinned follows whatever the checkout is on.
	if !matchesProject(withOrigin(t, remote), &Project{RepoURL: remote}) {
		t.Error("matchesProject() = false for the same remote with no branch pinned")
	}

	// Test 3: a pinned branch that the checkout is not on is stale. A freshly initialized
	// repository has no commit, so its head does not resolve, which is itself a mismatch.
	if matchesProject(withOrigin(t, remote), &Project{RepoURL: remote, Branch: "release"}) {
		t.Error("matchesProject() = true for a checkout whose head does not match the pinned branch")
	}
}

// TestSyncRefusesAnUnsafeRemoteBeforeTouchingDisk pins that the URL check runs first. A refusal that
// happened after the clone was attempted would have already made the request the check exists to
// prevent, and would leave a directory behind naming a project that was never allowed to sync.
func TestSyncRefusesAnUnsafeRemoteBeforeTouchingDisk(t *testing.T) {
	t.Parallel()
	refused := []string{
		"https://127.0.0.1/repo.git",
		"https://169.254.169.254/repo.git",
		"http://example.com/repo.git",
		"https://oauth2:tok3n@example.com/repo.git",
		"",
	}
	for testNum, url := range refused {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			cache := t.TempDir()
			s, err := NewSyncer(cache)
			if err != nil {
				t.Fatalf("NewSyncer() error = %v", err)
			}
			p := &Project{ID: "proj_refused", RepoURL: url}
			wt, err := s.Sync(p, "")
			if !errors.Is(err, ErrBadRepoURL) {
				t.Fatalf("Sync(%q) error = %v, want ErrBadRepoURL", url, err)
			}
			if wt != nil {
				t.Errorf("Sync(%q) returned a worktree alongside its refusal", url)
			}
			if _, err := os.Stat(filepath.Join(cache, p.ID)); !os.IsNotExist(err) {
				t.Errorf("Sync(%q) created a checkout for a refused remote, err = %v", url, err)
			}
		})
	}
}

// TestSyncRefusesABadKeyBeforeCloning pins that an unusable project key fails the sync rather than
// silently falling back to an unauthenticated clone. A private remote reached anonymously either
// fails confusingly or, worse, succeeds against a public repository of the same name.
func TestSyncRefusesABadKeyBeforeCloning(t *testing.T) {
	t.Parallel()
	cache := t.TempDir()
	s, err := NewSyncer(cache)
	if err != nil {
		t.Fatalf("NewSyncer() error = %v", err)
	}
	p := &Project{ID: "proj_key", RepoURL: "ssh://git@example.com/org/repo.git"}
	if _, err := s.Sync(p, "not a private key"); err == nil {
		t.Fatal("Sync() with an unusable key = nil error, want a refusal")
	} else if !strings.Contains(err.Error(), "parse project ssh key") {
		t.Errorf("Sync() error = %v, want the key parse named", err)
	}
	if _, err := os.Stat(filepath.Join(cache, p.ID)); !os.IsNotExist(err) {
		t.Errorf("Sync() created a checkout despite refusing the key, err = %v", err)
	}
}

// TestWorktreeCleanupIsSafeAndRemovesTheCopy pins the contract the callers rely on: a run's deferred
// cleanup must tolerate a nil worktree and a second call, because a sync that failed hands back nil
// and a caller may clean up on more than one path. It must also actually delete the copy, or a busy
// server fills its disk with per-run checkouts.
func TestWorktreeCleanupIsSafeAndRemovesTheCopy(t *testing.T) {
	t.Parallel()

	// Test 0: cleanup on a nil worktree and on one with no cleanup function does nothing.
	var nilWorktree *Worktree
	nilWorktree.Cleanup()
	(&Worktree{}).Cleanup()

	// Test 1: cleanup removes the directory and a second call is harmless.
	dir := t.TempDir()
	target := filepath.Join(dir, "run")
	if err := os.MkdirAll(filepath.Join(target, "roles"), 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	wt := &Worktree{Dir: target, cleanup: func() { _ = os.RemoveAll(target) }}
	wt.Cleanup()
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("Cleanup() left the run checkout behind, err = %v", err)
	}
	wt.Cleanup()
}

// TestGalaxyEnvInPointsAtTheCopy pins that the run's environment names the directory the run
// executes in. The dependencies are installed into the canonical checkout and copied, so an
// environment still pointing at the canonical path would hand every concurrent run the same mutable
// role tree, which is the sharing the isolated worktree exists to end.
func TestGalaxyEnvInPointsAtTheCopy(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Roles is whether role dependencies were installed.
		Roles bool
		// Collections is whether collection dependencies were installed.
		Collections bool
		// WantEnv is the environment the run receives.
		WantEnv []string
	}{
		{Roles: false, Collections: false, WantEnv: nil}, // Test 0: Nothing installed, nothing set.
		{Roles: true, Collections: false, WantEnv: []string{
			"ANSIBLE_ROLES_PATH=/run/dir/.galaxy/roles",
		}}, // Test 1: Roles only.
		{Roles: false, Collections: true, WantEnv: []string{
			"ANSIBLE_COLLECTIONS_PATH=/run/dir/.galaxy/collections",
		}}, // Test 2: Collections only.
		{Roles: true, Collections: true, WantEnv: []string{
			"ANSIBLE_ROLES_PATH=/run/dir/.galaxy/roles",
			"ANSIBLE_COLLECTIONS_PATH=/run/dir/.galaxy/collections",
		}}, // Test 3: Both.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got := galaxyEnvIn(filepath.FromSlash("/run/dir"), test.Roles, test.Collections)
			want := make([]string, 0, len(test.WantEnv))
			for _, e := range test.WantEnv {
				want = append(want, filepath.FromSlash(e))
			}
			if diff := cmp.Diff(want, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("galaxyEnvIn() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestCopyTreeFailsWhenTheDestinationIsMissing pins that the copy reports a failure rather than
// producing a half-populated run checkout. A run executing an incomplete copy would run a subset of
// the commit its record names, which is the mismatch the isolated worktree exists to prevent.
func TestCopyTreeFailsWhenTheDestinationIsMissing(t *testing.T) {
	t.Parallel()
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "site.yml"), []byte("---\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	// Test 0: a destination that does not exist fails on the first regular file.
	missing := filepath.Join(t.TempDir(), "absent")
	if err := copyTree(src, missing, nil); err == nil {
		t.Error("copyTree() into a missing destination = nil error, want a failure")
	}

	// Test 1: a source that does not exist fails rather than copying nothing quietly.
	if err := copyTree(filepath.Join(t.TempDir(), "gone"), t.TempDir(), nil); err == nil {
		t.Error("copyTree() from a missing source = nil error, want a failure")
	}

	// Test 2: an empty source tree copies cleanly and leaves an empty destination.
	dst := t.TempDir()
	if err := copyTree(t.TempDir(), dst, nil); err != nil {
		t.Errorf("copyTree() of an empty tree error = %v", err)
	}
	entries, err := os.ReadDir(dst)
	if err != nil {
		t.Fatalf("ReadDir() error = %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("copyTree() of an empty tree wrote %d entries", len(entries))
	}
}

// TestCopyFileFailsOnAnUnreadableSource pins that a file the copy cannot open stops the isolation
// rather than leaving a truncated file in the run checkout.
func TestCopyFileFailsOnAnUnreadableSource(t *testing.T) {
	t.Parallel()
	dst := filepath.Join(t.TempDir(), "out.yml")

	// Test 0: a source that does not exist.
	if err := copyFile(filepath.Join(t.TempDir(), "missing.yml"), dst, 0o600); err == nil {
		t.Error("copyFile() from a missing source = nil error, want a failure")
	}

	// Test 1: a destination inside a directory that does not exist.
	src := filepath.Join(t.TempDir(), "in.yml")
	if err := os.WriteFile(src, []byte("---\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if err := copyFile(src, filepath.Join(t.TempDir(), "nope", "out.yml"), 0o600); err == nil {
		t.Error("copyFile() into a missing directory = nil error, want a failure")
	}

	// Test 2: an ordinary copy keeps the bytes and the mode.
	if err := copyFile(src, dst, 0o640); err != nil {
		t.Fatalf("copyFile() error = %v", err)
	}
	body, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if diff := cmp.Diff("---\n", string(body)); diff != "" {
		t.Errorf("copyFile() content mismatch (-want +got):\n%s", diff)
	}
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Errorf("copyFile() mode = %v, want 0640", info.Mode().Perm())
	}
}

// TestWithinRepoBoundaries covers the path confinement used for every playbook and inventory path a
// template names. The joined path is handed straight to ansible-playbook, so an escape here means
// the repository, or whoever can edit a template, chooses what the control node reads.
func TestWithinRepoBoundaries(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "plays", "nested"), 0o750); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	for _, rel := range []string{"site.yml", "plays/deploy.yml", "plays/nested/ünïcode.yml"} {
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(rel)), []byte("---\n"), 0o600); err != nil {
			t.Fatalf("WriteFile(%s) error = %v", rel, err)
		}
	}

	tests := []struct {
		// Rel is the repository-relative path a template named.
		Rel string
		// Want is the error class expected, nil when the path is inside the repository.
		Want error
	}{
		{Rel: "site.yml", Want: nil},                          // Test 0: A plain file.
		{Rel: "./site.yml", Want: nil},                        // Test 1: A dot prefix.
		{Rel: "plays/deploy.yml", Want: nil},                  // Test 2: A nested file.
		{Rel: "plays/../site.yml", Want: nil},                 // Test 3: Traversal that stays inside.
		{Rel: "plays/nested/ünïcode.yml", Want: nil},          // Test 4: A non-ASCII name.
		{Rel: "", Want: nil},                                  // Test 5: Empty names the root itself.
		{Rel: ".", Want: nil},                                 // Test 6: Dot names the root itself.
		{Rel: "not/written/yet.yml", Want: nil},               // Test 7: A path the sync has not written.
		{Rel: strings.Repeat("a", 200) + ".yml", Want: nil},   // Test 8: A very long name.
		{Rel: "../outside.yml", Want: ErrEscapesRepo},         // Test 9: Traversal.
		{Rel: "..", Want: ErrEscapesRepo},                     // Test 10: The parent itself.
		{Rel: "plays/../../etc/passwd", Want: ErrEscapesRepo}, // Test 11: Deep traversal.
		{Rel: "/etc/passwd", Want: ErrEscapesRepo},            // Test 12: An absolute path.
		{Rel: "/", Want: ErrEscapesRepo},                      // Test 13: The filesystem root.
		// Test 14: A sibling whose name merely starts with the root's name is still outside.
		{Rel: "../" + filepath.Base(root) + "-evil/secret.yml", Want: ErrEscapesRepo},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got, err := WithinRepo(root, test.Rel)
			if !errors.Is(err, test.Want) {
				t.Fatalf("WithinRepo(%q) = (%q, %v), want %v", test.Rel, got, err, test.Want)
			}
			if test.Want != nil {
				return
			}
			if got != root && !strings.HasPrefix(got, root+string(filepath.Separator)) {
				t.Errorf("WithinRepo(%q) = %q, which is not inside %q", test.Rel, got, root)
			}
		})
	}
}

// TestWithinRepoFailsClosedOnAnEmptyRoot pins that a missing checkout root refuses every path rather
// than resolving relative to the working directory. A caller that reached this with an empty root
// has a bug, and the safe answer is a refusal rather than a file somewhere else on the host.
func TestWithinRepoFailsClosedOnAnEmptyRoot(t *testing.T) {
	t.Parallel()
	for testNum, rel := range []string{"site.yml", "plays/deploy.yml", "..", "/etc/passwd"} {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got, err := WithinRepo("", rel); !errors.Is(err, ErrEscapesRepo) {
				t.Errorf("WithinRepo(\"\", %q) = (%q, %v), want ErrEscapesRepo", rel, got, err)
			}
		})
	}
}

// TestSyncSurfacesADependencyInstallFailure pins that a project whose declared Ansible dependencies
// cannot be installed fails the sync rather than handing back a worktree missing them.
//
// A play that imports a role it never received fails later with a message about the role, not about
// the install, and the run's record would name a commit whose dependencies were never resolved. The
// refusal has to happen here, before anything executes.
func TestSyncSurfacesADependencyInstallFailure(t *testing.T) {
	// Not parallel: it puts a failing ansible-galaxy at the front of the process PATH.
	repo := initTestRepo(t, map[string]string{
		"site.yml":         "---\n",
		"requirements.yml": "roles:\n  - src: geerlingguy.nginx\n",
	})
	cache := t.TempDir()
	s, err := NewSyncer(cache)
	if err != nil {
		t.Fatalf("NewSyncer() error = %v", err)
	}
	stubs := t.TempDir()
	stub := filepath.Join(stubs, "ansible-galaxy")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\necho 'ERROR! role not found'\nexit 1\n"), 0o700); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	t.Setenv("PATH", stubs+string(os.PathListSeparator)+os.Getenv("PATH"))

	p := &Project{ID: "proj_deps", RepoURL: repo, Branch: "main", InstallDeps: true}
	wt, err := s.Sync(p, "")
	if err == nil {
		wt.Cleanup()
		t.Fatal("Sync() with an uninstallable dependency = nil error, want the failure surfaced")
	}
	if !strings.Contains(err.Error(), "ansible-galaxy") {
		t.Errorf("Sync() error = %v, want the galaxy install named", err)
	}
	if !strings.Contains(err.Error(), "role not found") {
		t.Errorf("Sync() error = %v, want the galaxy output carried so the run says why", err)
	}
	if wt != nil {
		t.Error("Sync() returned a worktree alongside its dependency failure")
	}
	// No per-run checkout is left behind for a sync that failed.
	if entries, err := os.ReadDir(filepath.Join(cache, runsSubdir)); err == nil && len(entries) > 0 {
		t.Errorf("a failed sync left %d run checkouts behind", len(entries))
	}
}

// TestSyncInstallsDeclaredDependenciesIntoTheRunCheckout pins the successful half of the same path:
// the roles are installed into the canonical checkout, copied with it, and the run's environment
// points at the copy rather than at the shared tree they were installed into.
func TestSyncInstallsDeclaredDependenciesIntoTheRunCheckout(t *testing.T) {
	// Not parallel: it puts a stub ansible-galaxy at the front of the process PATH.
	repo := initTestRepo(t, map[string]string{
		"site.yml":         "---\n",
		"requirements.yml": "roles:\n  - src: local.role\n",
	})
	s, err := NewSyncer(t.TempDir())
	if err != nil {
		t.Fatalf("NewSyncer() error = %v", err)
	}
	stubs := t.TempDir()
	// The stub writes a file where a real install would put the role, so the copy can be observed.
	script := "#!/bin/sh\nfor a in \"$@\"; do prev=$cur; cur=$a; if [ \"$prev\" = \"-p\" ]; then " +
		"mkdir -p \"$a/local.role\" && echo installed > \"$a/local.role/meta.yml\"; fi; done\nexit 0\n"
	if err := os.WriteFile(filepath.Join(stubs, "ansible-galaxy"), []byte(script), 0o700); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	t.Setenv("PATH", stubs+string(os.PathListSeparator)+os.Getenv("PATH"))

	p := &Project{ID: "proj_roles", RepoURL: repo, Branch: "main", InstallDeps: true}
	wt, err := s.Sync(p, "")
	if err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	defer wt.Cleanup()

	want := "ANSIBLE_ROLES_PATH=" + filepath.Join(wt.Dir, ".galaxy", "roles")
	if diff := cmp.Diff([]string{want}, wt.GalaxyEnv, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("the run environment does not point at its own copy (-want +got):\n%s", diff)
	}
	if _, err := os.Stat(filepath.Join(wt.Dir, ".galaxy", "roles", "local.role", "meta.yml")); err != nil {
		t.Errorf("the installed role was not copied into the run checkout: %v", err)
	}
}

// TestSyncFollowsTheRemoteDefaultBranch demonstrates that a project pinning no branch can be synced
// exactly once and fails on every sync after that.
//
// Project.Branch is documented as "Empty means the remote default", and the first clone works. A
// clone that names no reference leaves refs/remotes/origin/HEAD behind and no refs/remotes/origin/
// <name>. The update path then reads the branch name off the local head and looks up
// refs/remotes/origin/<name>, which was never written, so every later sync returns "resolve
// origin/main: reference not found". Nothing repairs it: matchesProject reports a match for an
// empty branch, so the stale checkout is kept rather than re-cloned, and every run of that project
// fails at sync from then on.
func TestSyncFollowsTheRemoteDefaultBranch(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t, map[string]string{"site.yml": "---\n"})
	s, err := NewSyncer(t.TempDir())
	if err != nil {
		t.Fatalf("NewSyncer() error = %v", err)
	}
	p := &Project{ID: "proj_default", RepoURL: repo}

	first, err := s.Sync(p, "")
	if err != nil {
		t.Fatalf("Sync() clone error = %v", err)
	}
	defer first.Cleanup()

	commitTestFile(t, repo, "site.yml", "---\n# second\n")

	second, err := s.Sync(p, "")
	if err != nil {
		t.Fatalf("Sync() update error = %v", err)
	}
	defer second.Cleanup()
	if second.SHA == first.SHA {
		t.Error("the commit did not advance, so a project on the remote default never updates")
	}
	body, err := os.ReadFile(filepath.Join(second.Dir, "site.yml"))
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if !strings.Contains(string(body), "second") {
		t.Errorf("the update did not reach the run checkout, content = %q", body)
	}
}

// initTestRepo builds a local git repository holding the given files, committed on main, and
// returns its path. It uses go-git so the test does not need the git binary.
func initTestRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	repo, err := git.PlainInitWithOptions(dir, &git.PlainInitOptions{
		InitOptions: git.InitOptions{DefaultBranch: plumbing.Main},
	})
	if err != nil {
		t.Fatalf("PlainInitWithOptions() error = %v", err)
	}
	for rel, body := range files {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatalf("MkdirAll(%s) error = %v", rel, err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatalf("WriteFile(%s) error = %v", rel, err)
		}
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree() error = %v", err)
	}
	if err := wt.AddGlob("."); err != nil {
		t.Fatalf("AddGlob() error = %v", err)
	}
	if _, err := wt.Commit("first", &git.CommitOptions{
		Author: &object.Signature{Name: "test", Email: "test@example.invalid", When: time.Now()},
	}); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	return dir
}

// commitTestFile writes a file into an existing test repository and commits it.
func commitTestFile(t *testing.T, dir, rel, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(rel)), []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	repo, err := git.PlainOpen(dir)
	if err != nil {
		t.Fatalf("PlainOpen() error = %v", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree() error = %v", err)
	}
	if err := wt.AddGlob("."); err != nil {
		t.Fatalf("AddGlob() error = %v", err)
	}
	if _, err := wt.Commit("next", &git.CommitOptions{
		Author: &object.Signature{Name: "test", Email: "test@example.invalid", When: time.Now()},
	}); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
}
