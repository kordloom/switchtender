package project

import (
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"runtime"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/client"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/go-git/go-git/v5/plumbing/transport/server"

	"github.com/kordloom/switchtender/internal/safedial"
)

// ErrLinkedCheckout is returned when a local repository's .git is a file rather than a folder, as
// in a linked worktree or a submodule. Its history lives somewhere else, which a project does not
// follow, so the project is pointed at the repository that holds the history.
var ErrLinkedCheckout = errors.New("this folder's .git is a file, as in a linked worktree or a " +
	"submodule, so it holds no history to sync: point the project at the repository it links to")

// init guards the HTTPS transport go-git dials with, so a repository URL cannot reach an address
// ValidateRepoURL would have refused, and serves a local repository from inside this process.
//
// The URL check reads the host as written, which settles the question only for a URL that names an
// address. A name is a different matter: it can resolve to 127.0.0.1 or to a metadata endpoint, and
// it can resolve to one thing when the project is saved and another when the clone runs, so a check
// performed on the text can always be walked around. The refusal has to happen once the address is
// known, which is at the dial, and this is where go-git does its dialing.
//
// The off-host policy is used rather than the plain one because it is the policy the URL check
// already expresses: checkRepoHost refuses loopback, the unspecified address, and link-local, and
// leaves the private ranges alone so a git host inside the network still clones.
//
// Only HTTPS is guarded. It is the scheme that reaches the services this protects against, since a
// metadata endpoint and an internal admin API both answer HTTP, and it is the one go-git lets a
// caller hand a client to. An SSH remote still gets the name check, and an SSH daemon is not a
// useful target for a request forgery, so the gap is narrow and stated rather than hidden.
//
// The file transport go-git ships runs git-upload-pack as a program, so a project whose remote is a
// folder on this server failed to sync wherever no git binary is installed, the release image
// among them. It is replaced by one that reads the repository in this process, with the loader
// below, so a local project syncs the same way on every install.
func init() {
	client.InstallProtocol("https", githttp.NewClient(&http.Client{
		Transport: safedial.OffHostTransport(),
	}))
	client.InstallProtocol("file", server.NewClient(localRepoLoader{}))
}

// localRepoLoader opens a repository on this server's filesystem for go-git's in-process
// upload-pack.
//
// The loader go-git ships reads a working copy's folder as though it were the repository, so a
// non-bare repository answered with no references. This one steps into .git for a working copy and
// reads a bare repository where it stands.
type localRepoLoader struct{}

// Load returns the object and reference store of the repository ep names: the .git folder of a
// working copy, or the folder itself for a bare repository. A folder whose .git is a file is
// refused with ErrLinkedCheckout, and a folder that holds no repository with
// transport.ErrRepositoryNotFound.
func (localRepoLoader) Load(ep *transport.Endpoint) (storer.Storer, error) {
	dir := localRepoPath(ep.Path)
	info, err := os.Stat(filepath.Join(dir, git.GitDirName))
	switch {
	case err == nil && !info.IsDir():
		return nil, fmt.Errorf("%w: %s", ErrLinkedCheckout, dir)
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		return nil, fmt.Errorf("read local repository %s: %w", dir, err)
	}
	repo, err := git.PlainOpen(dir)
	if errors.Is(err, git.ErrRepositoryNotExists) {
		return nil, transport.ErrRepositoryNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("open local repository %s: %w", dir, err)
	}
	return repo.Storer, nil
}

// localRepoPath returns the folder a file endpoint's path names. On Windows a file URL's path
// carries a slash ahead of its drive letter, which is dropped the way go-git's own file transport
// drops it.
func localRepoPath(p string) string {
	if runtime.GOOS == "windows" && len(p) >= 3 && p[0] == '/' && p[2] == ':' {
		return p[1:]
	}
	return p
}
