package project

import (
	"fmt"
	"net"
	"net/url"
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
	// galaxyServer is a private Ansible Galaxy or Automation Hub URL for collection installs, empty to
	// use the default public Galaxy.
	galaxyServer string
	// galaxyToken authenticates to galaxyServer, empty for an unauthenticated server.
	galaxyToken string
	// mu guards locks.
	mu sync.Mutex
	// locks holds one mutex per project id.
	locks map[string]*sync.Mutex
}

// SyncerOption configures a Syncer.
type SyncerOption func(*Syncer)

// WithGalaxy points collection installs at a private Ansible Galaxy or Automation Hub server and its
// token, so a project's collections resolve from an internal hub instead of the public Galaxy.
func WithGalaxy(server, token string) SyncerOption {
	return func(s *Syncer) {
		s.galaxyServer = server
		s.galaxyToken = token
	}
}

// NewSyncer returns a Syncer that caches checkouts under dir, creating it if needed.
func NewSyncer(dir string, opts ...SyncerOption) (*Syncer, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create project cache: %w", err)
	}
	s := &Syncer{cacheDir: dir, locks: make(map[string]*sync.Mutex)}
	for _, opt := range opts {
		opt(s)
	}
	return s, nil
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
	if err := ValidateRepoURL(p.RepoURL); err != nil {
		return "", "", nil, err
	}

	l := s.lock(p.ID)
	l.Lock()
	defer l.Unlock()

	auth, err := authFor(sshKey)
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
			return "", "", nil, fmt.Errorf("clone %s: %w", redactRepoURL(p.RepoURL), err)
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
		galaxyEnv, err = s.installGalaxy(dir)
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
func (s *Syncer) installGalaxy(checkout string) ([]string, error) {
	galaxyDir := filepath.Join(checkout, ".galaxy")
	rolesPath := filepath.Join(galaxyDir, "roles")
	collectionsPath := filepath.Join(galaxyDir, "collections")

	var wantRoles, wantCollections bool
	for _, name := range requirementFiles {
		path := filepath.Join(checkout, name)
		roles, collections := requirementKinds(path)
		if roles {
			if out, err := s.runGalaxy(checkout, "role", "install", "-r", path, "-p", rolesPath); err != nil {
				return nil, fmt.Errorf("ansible-galaxy role install %s: %w: %s", name, err, out)
			}
			wantRoles = true
		}
		if collections {
			out, err := s.runGalaxy(checkout, "collection", "install", "-r", path, "-p", collectionsPath)
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

// runGalaxy runs an ansible-galaxy subcommand in the checkout and returns its combined output,
// pointing collection installs at a configured private galaxy server through the environment.
func (s *Syncer) runGalaxy(checkout string, args ...string) (string, error) {
	cmd := exec.Command("ansible-galaxy", args...)
	cmd.Dir = checkout
	cmd.Env = append(os.Environ(), s.galaxyEnv()...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// galaxyEnv returns the ANSIBLE_GALAXY_SERVER_ variables that point collection installs at a
// configured private galaxy server or Automation Hub, or nil when none is set.
func (s *Syncer) galaxyEnv() []string {
	if s.galaxyServer == "" {
		return nil
	}
	env := []string{
		"ANSIBLE_GALAXY_SERVER_LIST=switchtender",
		"ANSIBLE_GALAXY_SERVER_SWITCHTENDER_URL=" + s.galaxyServer,
	}
	if s.galaxyToken != "" {
		env = append(env, "ANSIBLE_GALAXY_SERVER_SWITCHTENDER_TOKEN="+s.galaxyToken)
	}
	return env
}

// fetchAndReset updates an existing checkout to the remote branch tip, discarding local drift so
// the checkout always mirrors the remote.
func fetchAndReset(repo *git.Repository, p *Project, auth transport.AuthMethod) error {
	err := repo.Fetch(&git.FetchOptions{Auth: auth, Force: true})
	if err != nil && err != git.NoErrAlreadyUpToDate {
		return fmt.Errorf("fetch %s: %w", redactRepoURL(p.RepoURL), err)
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

// blockedRepoHosts are hostnames a repository must never resolve to. The cloud metadata service in
// particular can hand back instance credentials, so a clone against it is a server-side request
// forgery.
var blockedRepoHosts = map[string]bool{
	"localhost":                true,
	"metadata.google.internal": true,
}

// allowedRepoSchemes are the transport schemes a repository URL may use. Plain http and git are
// refused for being unauthenticated and cleartext; only ssh, https, and a local file path pass.
var allowedRepoSchemes = map[string]bool{
	"ssh":   true,
	"https": true,
	"file":  true,
}

// ValidateRepoURL reports whether raw is a repository URL safe to clone. It allows only the ssh,
// https, and file transports, and rejects loopback, link-local, and cloud metadata hosts so a
// stored project cannot drive the executor into a server-side request forgery.
func ValidateRepoURL(raw string) error {
	if strings.TrimSpace(raw) == "" {
		return fmt.Errorf("%w: empty url", ErrBadRepoURL)
	}
	if err := checkRepoUserinfo(raw); err != nil {
		return err
	}
	host, scheme, err := repoURLParts(raw)
	if err != nil {
		return err
	}
	if !allowedRepoSchemes[scheme] {
		return fmt.Errorf("%w: scheme %q is not allowed", ErrBadRepoURL, scheme)
	}
	if scheme == "file" {
		return nil
	}
	return checkRepoHost(host)
}

// checkRepoUserinfo rejects a scheme-prefixed repository URL that embeds credentials. A token or
// password in the URL surfaces in clone and fetch errors and in stored project rows, so credentials
// belong in a stored credential instead. An ssh username alone passes, since it names the login and
// carries no secret.
func checkRepoUserinfo(raw string) error {
	if !strings.Contains(raw, "://") {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return nil
	}
	if _, hasPassword := u.User.Password(); hasPassword || u.Scheme != "ssh" {
		return fmt.Errorf("%w: credentials embedded in url", ErrBadRepoURL)
	}
	return nil
}

// redactRepoURL returns the URL with any embedded userinfo removed so error text never carries a
// token or password from the URL. A URL that does not parse is replaced entirely, since its shape
// is unknown. The scp-like shorthand has no password slot and passes through unchanged.
func redactRepoURL(raw string) string {
	if !strings.Contains(raw, "://") {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "<invalid url>"
	}
	u.User = nil
	return u.String()
}

// repoURLParts extracts the host and transport scheme from a repository URL, handling both the
// scheme-prefixed form and git's scp-like user@host:path shorthand, which is ssh.
func repoURLParts(raw string) (host, scheme string, err error) {
	if strings.Contains(raw, "://") {
		u, perr := url.Parse(raw)
		if perr != nil {
			return "", "", fmt.Errorf("%w: %v", ErrBadRepoURL, perr)
		}
		return u.Hostname(), u.Scheme, nil
	}
	// The scp-like shorthand is [user@]host:path, where the host part carries no slash. Anything
	// without this colon and without a scheme is a local filesystem path.
	if i := strings.IndexByte(raw, ':'); i >= 0 && !strings.Contains(raw[:i], "/") {
		hostPart := raw[:i]
		if at := strings.LastIndexByte(hostPart, '@'); at >= 0 {
			hostPart = hostPart[at+1:]
		}
		return hostPart, "ssh", nil
	}
	return "", "file", nil
}

// checkRepoHost rejects a repository host that is empty, explicitly blocked, or a loopback,
// unspecified, or link-local address, the last of which covers the cloud metadata endpoint.
func checkRepoHost(host string) error {
	if host == "" {
		return fmt.Errorf("%w: missing host", ErrBadRepoURL)
	}
	if blockedRepoHosts[strings.ToLower(host)] {
		return fmt.Errorf("%w: host %q is not allowed", ErrBadRepoURL, host)
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsLoopback() || ip.IsUnspecified() ||
			ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
			return fmt.Errorf("%w: address %q is not allowed", ErrBadRepoURL, host)
		}
	}
	return nil
}

// authFor builds the transport auth for a remote: an SSH key when provided, nothing otherwise.
func authFor(sshKey string) (transport.AuthMethod, error) {
	if sshKey == "" {
		return nil, nil
	}
	keys, err := gitssh.NewPublicKeys("git", []byte(sshKey), "")
	if err != nil {
		return nil, fmt.Errorf("parse project ssh key: %w", err)
	}
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
