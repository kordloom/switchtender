package project

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"gopkg.in/yaml.v3"
)

// Syncer keeps local checkouts of project repositories, one per project under a cache directory.
// Sync is safe for concurrent use; runs of the same project serialize on a per-project lock.
type Syncer struct {
	// cacheDir is the directory holding one checkout per project.
	cacheDir string
	// mu guards locks.
	mu sync.Mutex
	// locks holds one mutex per project id.
	locks map[string]*sync.Mutex
}

// NewSyncer returns a Syncer that caches checkouts under dir, creating it if needed.
func NewSyncer(dir string) (*Syncer, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create project cache: %w", err)
	}
	return &Syncer{cacheDir: dir, locks: make(map[string]*sync.Mutex)}, nil
}

// lock returns the mutex for a project id, creating it on first use.
func (s *Syncer) lock(id string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.locks[id]
	if !ok {
		l = &sync.Mutex{}
		s.locks[id] = l
	}
	return l
}

// Sync brings the project's checkout up to date with its remote and returns the checkout path, the
// commit it now sits on, and any environment entries that point Ansible at the project's installed
// role and collection dependencies. sshKey is the PEM private key for private remotes, empty
// otherwise. When the project has InstallDeps set, a present requirements file triggers an
// ansible-galaxy install into a project-scoped path under the checkout, all under the sync lock.
func (s *Syncer) Sync(p *Project, sshKey string) (dir, sha string, galaxyEnv []string, err error) {
	l := s.lock(p.ID)
	l.Lock()
	defer l.Unlock()

	auth, err := authFor(p.RepoURL, sshKey)
	if err != nil {
		return "", "", nil, err
	}
	dir = filepath.Join(s.cacheDir, p.ID)

	repo, err := git.PlainOpen(dir)
	if err != nil {
		repo, err = git.PlainClone(dir, false, &git.CloneOptions{
			URL: p.RepoURL, Auth: auth, ReferenceName: branchRef(p.Branch), SingleBranch: true,
		})
		if err != nil {
			return "", "", nil, fmt.Errorf("clone %s: %w", p.RepoURL, err)
		}
	} else {
		if err := fetchAndReset(repo, p, auth); err != nil {
			return "", "", nil, err
		}
	}

	head, err := repo.Head()
	if err != nil {
		return "", "", nil, fmt.Errorf("resolve head: %w", err)
	}
	sha = head.Hash().String()

	if p.InstallDeps {
		galaxyEnv, err = installGalaxy(dir)
		if err != nil {
			return "", "", nil, err
		}
	}
	return dir, sha, galaxyEnv, nil
}

// requirementFiles names the checkout-relative paths where Ansible dependency requirements live.
var requirementFiles = []string{
	"requirements.yml",
	"roles/requirements.yml",
	"collections/requirements.yml",
}

// installGalaxy installs the checkout's Ansible role and collection requirements into a
// project-scoped path and returns the environment entries that expose them to a run. A missing
// requirements file is a no-op; a galaxy failure is a real dependency problem and is returned so
// the run fails with the galaxy output.
func installGalaxy(checkout string) ([]string, error) {
	galaxyDir := filepath.Join(checkout, ".galaxy")
	rolesPath := filepath.Join(galaxyDir, "roles")
	collectionsPath := filepath.Join(galaxyDir, "collections")

	var wantRoles, wantCollections bool
	for _, name := range requirementFiles {
		path := filepath.Join(checkout, name)
		roles, collections := requirementKinds(path)
		if roles {
			if out, err := runGalaxy(checkout, "role", "install", "-r", path, "-p", rolesPath); err != nil {
				return nil, fmt.Errorf("ansible-galaxy role install %s: %w: %s", name, err, out)
			}
			wantRoles = true
		}
		if collections {
			out, err := runGalaxy(checkout, "collection", "install", "-r", path, "-p", collectionsPath)
			if err != nil {
				return nil, fmt.Errorf("ansible-galaxy collection install %s: %w: %s", name, err, out)
			}
			wantCollections = true
		}
	}

	var env []string
	if wantRoles {
		env = append(env, "ANSIBLE_ROLES_PATH="+rolesPath)
	}
	if wantCollections {
		env = append(env, "ANSIBLE_COLLECTIONS_PATH="+collectionsPath)
	}
	return env, nil
}

// requirementKinds reports whether a requirements file declares roles or collections. A bare list
// is the old role-only format; a mapping names each kind by key. A missing or unreadable file
// declares neither, so it is skipped.
func requirementKinds(path string) (roles, collections bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return false, false
	}
	var doc any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return false, false
	}
	switch v := doc.(type) {
	case []any:
		return true, false
	case map[string]any:
		_, roles = v["roles"]
		_, collections = v["collections"]
		return roles, collections
	default:
		return false, false
	}
}

// runGalaxy runs an ansible-galaxy subcommand in the checkout and returns its combined output.
func runGalaxy(checkout string, args ...string) (string, error) {
	cmd := exec.Command("ansible-galaxy", args...)
	cmd.Dir = checkout
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// fetchAndReset updates an existing checkout to the remote branch tip, discarding local drift so
// the checkout always mirrors the remote.
func fetchAndReset(repo *git.Repository, p *Project, auth transport.AuthMethod) error {
	err := repo.Fetch(&git.FetchOptions{Auth: auth, Force: true})
	if err != nil && err != git.NoErrAlreadyUpToDate {
		return fmt.Errorf("fetch %s: %w", p.RepoURL, err)
	}

	branch := p.Branch
	if branch == "" {
		head, err := repo.Head()
		if err != nil {
			return fmt.Errorf("resolve head: %w", err)
		}
		branch = head.Name().Short()
	}
	remote, err := repo.Reference(
		plumbing.NewRemoteReferenceName("origin", branch), true)
	if err != nil {
		return fmt.Errorf("resolve origin/%s: %w", branch, err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		return fmt.Errorf("open worktree: %w", err)
	}
	if err := wt.Reset(&git.ResetOptions{Commit: remote.Hash(), Mode: git.HardReset}); err != nil {
		return fmt.Errorf("reset to origin/%s: %w", branch, err)
	}
	return nil
}

// authFor builds the transport auth for a remote: an SSH key when provided, nothing otherwise.
func authFor(repoURL, sshKey string) (transport.AuthMethod, error) {
	if sshKey == "" {
		return nil, nil
	}
	keys, err := gitssh.NewPublicKeys("git", []byte(sshKey), "")
	if err != nil {
		return nil, fmt.Errorf("parse project ssh key: %w", err)
	}
	_ = repoURL
	return keys, nil
}

// branchRef converts a branch name to a reference, zero value for the remote default.
func branchRef(branch string) plumbing.ReferenceName {
	if branch == "" {
		return ""
	}
	return plumbing.NewBranchReferenceName(branch)
}

// WithinRepo joins rel onto root and confirms the result stays inside root, defeating traversal
// through dot dot segments or absolute paths.
func WithinRepo(root, rel string) (string, error) {
	if filepath.IsAbs(rel) {
		return "", ErrEscapesRepo
	}
	joined := filepath.Clean(filepath.Join(root, rel))
	if joined != root && !strings.HasPrefix(joined, root+string(filepath.Separator)) {
		return "", ErrEscapesRepo
	}
	return joined, nil
}
