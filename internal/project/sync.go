package project

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/kordloom/switchtender/internal/util"

	"github.com/go-git/go-git/v5"
	gitconfig "github.com/go-git/go-git/v5/config"
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

// runsSubdir holds the per-run isolated worktrees, one directory per Sync call, kept apart from the
// canonical per-project checkouts that live directly under the cache directory.
const runsSubdir = ".runs"

// NewSyncer returns a Syncer that caches checkouts under dir, creating it if needed.
func NewSyncer(dir string, opts ...SyncerOption) (*Syncer, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create project cache: %w", err)
	}
	// A worktree is removed when its run ends, but a crash leaves it behind. Clearing the run
	// directory on startup keeps a restarted server from accumulating dead checkouts.
	_ = os.RemoveAll(filepath.Join(dir, runsSubdir))
	s := &Syncer{cacheDir: dir, locks: make(map[string]*sync.Mutex)}
	for _, opt := range opts {
		opt(s)
	}
	return s, nil
}

// Worktree is an isolated checkout of a project at one commit, private to a single Sync caller.
//
// A run executes here rather than in the shared per-project checkout, so a second run of the same
// project can fetch and hard reset that shared checkout without changing the files under the first.
// Before this, the checkout was shared and the reset happened while the earlier run was still
// reading it: the run recorded one commit and executed a mix of that commit and the next, which made
// the commit stamped on the audit record a claim the files on disk did not support.
type Worktree struct {
	// Dir is the isolated checkout to execute in.
	Dir string
	// SHA is the commit the checkout sits on, the value stamped on the run.
	SHA string
	// GalaxyEnv exposes the checkout's installed Ansible roles and collections to the run.
	GalaxyEnv []string
	// cleanup removes the isolated checkout.
	cleanup func()
}

// Cleanup removes the worktree. It is safe to call on a nil worktree and safe to call more than once.
func (w *Worktree) Cleanup() {
	if w != nil && w.cleanup != nil {
		w.cleanup()
	}
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

// Sync brings the project's canonical checkout up to date with its remote, then returns a private
// per-run Worktree copied from it: the commit it sits on and the environment that exposes its
// installed role and collection dependencies. sshKey is the PEM private key for private remotes,
// empty otherwise. When the project has InstallDeps set, a present requirements file triggers an
// ansible-galaxy install into the canonical checkout, all under the sync lock, and the result is
// copied into the worktree. The caller runs in the worktree and calls its Cleanup when the run ends.
func (s *Syncer) Sync(p *Project, sshKey string) (*Worktree, error) {
	if err := ValidateRepoURL(p.RepoURL); err != nil {
		return nil, err
	}

	l := s.lock(p.ID)
	l.Lock()
	defer l.Unlock()

	auth, err := authFor(sshKey)
	if err != nil {
		return nil, err
	}
	canonical := filepath.Join(s.cacheDir, p.ID)

	repo, err := git.PlainOpen(canonical)
	// A checkout that no longer matches the project it belongs to is discarded and taken again. The
	// cached copy carries the remote and branch it was cloned with, and fetching only ever asks that
	// remote for that branch, so an operator who repointed a project kept running the code it used to
	// hold. That is the dangerous direction: the change is made precisely because the old source is
	// wrong, and nothing in the run says it was ignored. Re-cloning costs one fetch on a change that
	// is rare, and it is correct for a moved remote and a switched branch alike.
	if err == nil && !matchesProject(repo, p) {
		if rmErr := os.RemoveAll(canonical); rmErr != nil {
			return nil, fmt.Errorf("discard stale checkout: %w", rmErr)
		}
		err = git.ErrRepositoryNotExists
	}
	if err != nil {
		repo, err = git.PlainClone(canonical, false, &git.CloneOptions{
			URL: p.RepoURL, Auth: auth, ReferenceName: branchRef(p.Branch), SingleBranch: true,
		})
		if err != nil {
			return nil, fmt.Errorf("clone %s: %w", redactRepoURL(p.RepoURL), err)
		}
		// A clone that pinned no branch is left tracking the branch it landed on, so the update path
		// finds it. The checkout is discarded when that fails, because a checkout the next sync
		// cannot update is worse than no checkout at all: it is kept, it never advances, and every
		// run of the project executes the commit of the first clone.
		if p.Branch == "" {
			if err := trackClonedBranch(repo); err != nil {
				_ = os.RemoveAll(canonical)
				return nil, err
			}
		}
	} else {
		if err := fetchAndReset(repo, p, auth); err != nil {
			return nil, err
		}
	}

	head, err := repo.Head()
	if err != nil {
		return nil, fmt.Errorf("resolve head: %w", err)
	}
	sha := head.Hash().String()

	var wantRoles, wantCollections bool
	if p.InstallDeps {
		wantRoles, wantCollections, err = s.installGalaxy(canonical)
		if err != nil {
			return nil, err
		}
	}

	// Copy the canonical checkout to a private directory while the lock is held, so the copy is a
	// consistent snapshot of this exact commit. The run executes from the copy, so the next sync,
	// which resets the canonical checkout, cannot reach it. The galaxy dependencies were installed
	// into the canonical checkout and are copied with it, so the copy is self-contained and no
	// dependency install runs per run.
	runDir, err := s.isolate(p.ID, canonical)
	if err != nil {
		return nil, err
	}
	return &Worktree{
		Dir:       runDir,
		SHA:       sha,
		GalaxyEnv: galaxyEnvIn(runDir, wantRoles, wantCollections),
		cleanup:   func() { _ = os.RemoveAll(runDir) },
	}, nil
}

// isolate copies a project's canonical checkout to a fresh per-run directory and returns its path.
// The .git directory is left behind: a run executes source, not history, and the repository metadata
// is the largest part of a checkout and the part a run never reads.
func (s *Syncer) isolate(projectID, canonical string) (string, error) {
	base := filepath.Join(s.cacheDir, runsSubdir)
	if err := os.MkdirAll(base, 0o700); err != nil {
		return "", fmt.Errorf("create run checkout base: %w", err)
	}
	runDir, err := os.MkdirTemp(base, projectID+"-")
	if err != nil {
		return "", fmt.Errorf("create run checkout: %w", err)
	}
	if err := copyTree(canonical, runDir, func(rel string) bool {
		return rel == ".git" || strings.HasPrefix(rel, ".git"+string(os.PathSeparator))
	}); err != nil {
		_ = os.RemoveAll(runDir)
		return "", fmt.Errorf("isolate checkout: %w", err)
	}
	return runDir, nil
}

// requirementFiles names the checkout-relative paths where Ansible dependency requirements live.
var requirementFiles = []string{
	"requirements.yml",
	"roles/requirements.yml",
	"collections/requirements.yml",
}

// installGalaxy installs the checkout's Ansible role and collection requirements into a
// project-scoped path under the checkout, and reports which kinds were installed so a run's
// environment can point at them wherever the checkout is executed from. A missing requirements file
// is a no-op; a galaxy failure is a real dependency problem and is returned so the run fails with the
// galaxy output.
func (s *Syncer) installGalaxy(checkout string) (wantRoles, wantCollections bool, err error) {
	galaxyDir := filepath.Join(checkout, ".galaxy")
	rolesPath := filepath.Join(galaxyDir, "roles")
	collectionsPath := filepath.Join(galaxyDir, "collections")

	for _, name := range requirementFiles {
		path := filepath.Join(checkout, name)
		roles, collections := requirementKinds(path)
		if roles {
			if out, err := s.runGalaxy(checkout, "role", "install", "-r", path, "-p", rolesPath); err != nil {
				return false, false, fmt.Errorf("ansible-galaxy role install %s: %w: %s", name, err, out)
			}
			wantRoles = true
		}
		if collections {
			out, err := s.runGalaxy(checkout, "collection", "install", "-r", path, "-p", collectionsPath)
			if err != nil {
				return false, false, fmt.Errorf("ansible-galaxy collection install %s: %w: %s", name, err, out)
			}
			wantCollections = true
		}
	}
	return wantRoles, wantCollections, nil
}

// galaxyEnvIn returns the Ansible environment that points at the roles and collections installed
// under dir, for the kinds that were installed. The paths are computed from dir so a copy of the
// checkout exposes its own dependencies rather than the checkout they were copied from.
func galaxyEnvIn(dir string, wantRoles, wantCollections bool) []string {
	var env []string
	if wantRoles {
		env = append(env, "ANSIBLE_ROLES_PATH="+filepath.Join(dir, ".galaxy", "roles"))
	}
	if wantCollections {
		env = append(env, "ANSIBLE_COLLECTIONS_PATH="+filepath.Join(dir, ".galaxy", "collections"))
	}
	return env
}

// copyTree recursively copies src to dst, which must already exist. A relative path for which skip
// returns true, and everything beneath it, is left out. File modes are preserved so an executable
// script stays executable, and a symlink is recreated as a symlink rather than followed, so the copy
// carries the same links the checkout did and no link is dereferenced across the tree boundary.
func copyTree(src, dst string, skip func(rel string) bool) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if skip != nil && skip(rel) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		target := filepath.Join(dst, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			return os.MkdirAll(target, info.Mode().Perm())
		case info.Mode()&os.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		default:
			return copyFile(path, target, info.Mode().Perm())
		}
	})
}

// copyFile copies one regular file's contents to dst with the given mode.
func copyFile(src, dst string, mode fs.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
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
	// Installing a requirements file runs code the repository chose, so it is handed the host's
	// environment without SwitchTender's own configuration in it.
	cmd.Env = append(util.RunEnviron(), s.galaxyEnv()...)
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
	tracking := plumbing.NewRemoteReferenceName("origin", branch)
	remote, err := repo.Reference(tracking, true)
	// A checkout taken before the clone pinned its branch carries only refs/remotes/origin/HEAD, so
	// the reference above is missing and the sync fails. Pinning the branch and fetching once more
	// repairs that checkout in place, which matters because nothing else would: the checkout matches
	// its project, so it is kept, and every run of the project would fail at sync from then on.
	if err != nil && p.Branch == "" {
		if fixErr := trackClonedBranch(repo); fixErr == nil {
			if fetchErr := repo.Fetch(&git.FetchOptions{Auth: auth, Force: true}); fetchErr != nil &&
				fetchErr != git.NoErrAlreadyUpToDate {
				return fmt.Errorf("fetch %s: %w", redactRepoURL(p.RepoURL), fetchErr)
			}
			remote, err = repo.Reference(tracking, true)
		}
	}
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

// checkRepoUserinfo rejects a repository URL that embeds credentials. A token or password in the
// URL surfaces in clone and fetch errors and in stored project rows, so credentials belong in a
// stored credential instead. An ssh username alone passes, since it names the login and carries no
// secret.
//
// The scp-like shorthand is held to the same rule. It has no password slot in its syntax, but
// go-git reads everything before the last "@" as the login, so "tok:secret@host:path" is a remote
// it dials with the secret in it, and only the scheme-prefixed spelling was ever refused.
func checkRepoUserinfo(raw string) error {
	if !strings.Contains(raw, "://") {
		if end := scpUserinfoEnd(raw); strings.Contains(raw[:end], ":") {
			return fmt.Errorf("%w: credentials embedded in url", ErrBadRepoURL)
		}
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
// is unknown.
//
// The scp-like shorthand has no password slot, but that is a statement about the syntax rather than
// about what people write. go-git reads everything before the last "@" as the login, so
// "tok:secret@host:path" is a remote it dials with the whole string as the login, and every clone
// and fetch failure interpolated that secret into logs, run records, and API responses. A shorthand
// login carrying a colon is that password shape and is cut at the same "@" go-git splits on, which
// leaves the host and path the error text needs. A bare login names the ssh user and holds no
// secret, which is the same line checkRepoUserinfo draws, so it is kept.
func redactRepoURL(raw string) string {
	if !strings.Contains(raw, "://") {
		if end := scpUserinfoEnd(raw); strings.Contains(raw[:end], ":") {
			return raw[end:]
		}
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
	// A value that begins with a dash is refused before anything is inferred from it. Nothing here
	// classifies it as a scheme, so "--upload-pack=/bin/sh" read as a local file path and passed.
	// go-git is the clone driver today so it is inert, but a validator that accepts an option is
	// contributing nothing, and the day anything shells out to git it becomes execution.
	if strings.HasPrefix(raw, "-") {
		return "", "", fmt.Errorf("%w: %q looks like a command option, not a repository",
			ErrBadRepoURL, raw)
	}
	// The scp-like shorthand is [user@]host:path, where the host part carries no slash. Anything
	// without this colon and without a scheme is a local filesystem path.
	//
	// The userinfo is cut off before the colon is looked for, because go-git splits the shorthand at
	// the last "@" and the login may hold a colon of its own. Searching the whole value for the first
	// colon read "u:p@127.0.0.1:repo.git" as host "u", which the host check found nothing wrong with
	// while go-git dialed 127.0.0.1, so a loopback and metadata address walked straight through the
	// one check that exists to refuse them.
	rest := raw[scpUserinfoEnd(raw):]
	if i := strings.IndexByte(rest, ':'); i >= 0 && !strings.Contains(rest[:i], "/") {
		return rest[:i], "ssh", nil
	}
	return "", "file", nil
}

// scpUserinfoEnd returns the offset where the host begins in git's scp-like user@host:path
// shorthand, which is just past the last "@" ahead of the path, and zero when there is no userinfo.
// go-git splits the shorthand there, so everything in front of that "@" is login text rather than
// host text. An "@" that sits after the first slash belongs to the path and is left alone, which
// keeps a local path such as "/srv/git/a@b/repo.git" intact.
func scpUserinfoEnd(raw string) int {
	at := strings.LastIndexByte(raw, '@')
	if at < 0 {
		return 0
	}
	if slash := strings.IndexByte(raw, '/'); slash >= 0 && slash < at {
		return 0
	}
	return at + 1
}

// parseLooseIP parses a host as an IP address, accepting the shorthand forms a resolver honors but
// net.ParseIP does not: a dotted form with fewer than four parts, a bare 32-bit integer, and the
// octal and hex spellings of a part. All of them reach 127.0.0.1, so a validator that only
// understands the canonical spelling blocks the obvious way in and leaves the rest beside it.
func parseLooseIP(host string) net.IP {
	if ip := net.ParseIP(host); ip != nil {
		return ip
	}
	if n, err := parseInetAtonPart(host); err == nil {
		return net.IPv4(byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
	}
	parts := strings.Split(host, ".")
	if len(parts) < 2 || len(parts) > 4 {
		return nil
	}
	nums := make([]uint64, len(parts))
	for i, p := range parts {
		n, err := parseInetAtonPart(p)
		if err != nil {
			return nil
		}
		nums[i] = n
	}
	// The last part carries the remaining octets, which is how 127.1 means 127.0.0.1.
	var v uint64
	for i := 0; i < len(nums)-1; i++ {
		if nums[i] > 255 {
			return nil
		}
		v |= nums[i] << (8 * (3 - uint(i)))
	}
	if nums[len(nums)-1] > (uint64(1)<<(8*(4-uint(len(nums)-1))))-1 {
		return nil
	}
	v |= nums[len(nums)-1]
	return net.IPv4(byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
}

// parseInetAtonPart parses one part of an address the way inet_aton does, which is what the
// platform resolver uses on macOS and on glibc: a "0x" prefix is hex, a leading zero is octal, and
// everything else is decimal. Reading every part as decimal left "0177.0.0.1" and "0x7f.0.0.1"
// looking like names rather than addresses, so the host check never saw the loopback they both
// reach.
func parseInetAtonPart(part string) (uint64, error) {
	base := 10
	switch {
	case len(part) > 2 && part[0] == '0' && (part[1] == 'x' || part[1] == 'X'):
		base, part = 16, part[2:]
	case len(part) > 1 && part[0] == '0':
		base, part = 8, part[1:]
	}
	n, err := strconv.ParseUint(part, base, 32)
	if err != nil {
		return 0, fmt.Errorf("parse address part: %w", err)
	}
	return n, nil
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
	// Resolved before the address tests, because a host is not always written as an address.
	// net.ParseIP returns nil for "127.1" and for "2130706433", both of which reach loopback, so
	// the checks below never ran on them and the shorthand forms passed.
	if ip := parseLooseIP(host); ip != nil {
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
// through dot dot segments, absolute paths, and symlinks committed to the repository.
//
// The lexical check alone was not containment. Git stores a symlink's target verbatim, so a
// repository could commit "esc -> /" and name a playbook of "esc/etc/shadow": the joined path is
// inside root by every string test, and the file that opens is not. The path is handed straight to
// ansible-playbook and to ansible-inventory, so the repository chose what got read. The browse path
// in this same package already resolved symlinks after joining; this one did not.
//
// A path that does not exist yet is allowed through on the lexical result. Resolution can only
// report what is there, and a caller naming a file the sync has not written is a missing file
// rather than an escape.
func WithinRepo(root, rel string) (string, error) {
	if filepath.IsAbs(rel) {
		return "", ErrEscapesRepo
	}
	joined := filepath.Clean(filepath.Join(root, rel))
	if joined != root && !strings.HasPrefix(joined, root+string(filepath.Separator)) {
		return "", ErrEscapesRepo
	}
	resolved, err := filepath.EvalSymlinks(joined)
	if errors.Is(err, fs.ErrNotExist) {
		return joined, nil
	}
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrEscapesRepo, err)
	}
	// The root is resolved too, so a symlink anywhere above the checkout does not read as an escape.
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		realRoot = root
	}
	if resolved != realRoot && !strings.HasPrefix(resolved, realRoot+string(filepath.Separator)) {
		return "", ErrEscapesRepo
	}
	return joined, nil
}

// trackClonedBranch points a fresh clone's origin remote at the branch the clone landed on and
// writes the matching remote-tracking reference.
//
// A project pinning no branch follows the remote default, so the clone names no reference, and
// go-git takes such a clone with "+HEAD:refs/remotes/origin/HEAD" as the remote's only refspec, so
// refs/remotes/origin/<branch> is never written. The update path reads the branch name off the
// local head and resolves refs/remotes/origin/<branch>, so every sync after the first clone failed
// with "reference not found", and nothing repaired it: matchesProject reports a match for a project
// with no branch pinned, so the stale checkout was kept rather than taken again, and every run of
// that project failed at sync from then on. Giving the clone the layout a branch-pinned clone gets
// makes "empty means the remote default" hold on the second sync as well as the first.
func trackClonedBranch(repo *git.Repository) error {
	head, err := repo.Head()
	if err != nil {
		return fmt.Errorf("resolve cloned head: %w", err)
	}
	if !head.Name().IsBranch() {
		return fmt.Errorf("clone is not on a branch: %s", head.Name())
	}
	branch := head.Name().Short()
	cfg, err := repo.Config()
	if err != nil {
		return fmt.Errorf("read checkout config: %w", err)
	}
	origin, ok := cfg.Remotes["origin"]
	if !ok {
		return errors.New("clone has no origin remote")
	}
	origin.Fetch = []gitconfig.RefSpec{gitconfig.RefSpec(
		fmt.Sprintf("+refs/heads/%s:refs/remotes/origin/%s", branch, branch))}
	if err := repo.SetConfig(cfg); err != nil {
		return fmt.Errorf("track branch %s: %w", branch, err)
	}
	tracking := plumbing.NewRemoteReferenceName("origin", branch)
	if err := repo.Storer.SetReference(plumbing.NewHashReference(tracking, head.Hash())); err != nil {
		return fmt.Errorf("write %s: %w", tracking, err)
	}
	return nil
}

// matchesProject reports whether an existing checkout was taken from the project's current remote and
// branch. A checkout that does not match is stale in a way fetching cannot repair, because a fetch
// asks the remote the checkout already has.
func matchesProject(repo *git.Repository, p *Project) bool {
	origin, err := repo.Remote("origin")
	if err != nil || origin.Config() == nil || len(origin.Config().URLs) == 0 {
		return false
	}
	if origin.Config().URLs[0] != p.RepoURL {
		return false
	}
	if p.Branch == "" {
		// The project follows the remote's default branch, so whatever the checkout is on is right.
		return true
	}
	head, err := repo.Head()
	if err != nil {
		return false
	}
	return head.Name().Short() == p.Branch
}
