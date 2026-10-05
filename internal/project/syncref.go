package project

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-git/go-git/v5"
	gitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// fetchedRef is the local reference a ref sync lands its fetch on. It sits outside refs/heads and
// refs/remotes, so nothing the branch sync reads or resets ever sees it, and it is rewritten on
// every ref sync under the project's lock.
const fetchedRef = plumbing.ReferenceName("refs/switchtender/fetched")

// ValidateFetchRef reports whether ref is a full git reference a run may ask the syncer to fetch.
// It must sit under refs/, pass git's reference name rules, and not begin with a dash, so a stored
// run cannot turn its ref into a fetch option or a refspec that writes somewhere else.
func ValidateFetchRef(ref string) error {
	if !strings.HasPrefix(ref, "refs/") || strings.ContainsAny(ref, ":+^~*?[\\ \t\r\n") {
		return fmt.Errorf("%w: %q", ErrBadRef, ref)
	}
	if err := plumbing.ReferenceName(ref).Validate(); err != nil {
		return fmt.Errorf("%w: %q: %v", ErrBadRef, ref, err)
	}
	return nil
}

// SyncRef fetches ref from the project's remote and returns a private per-run Worktree holding the
// commit it points at. It is how a run executes a commit that sits on no branch the project tracks,
// such as the head of a pull request, which GitHub publishes as refs/pull/N/head and GitLab as
// refs/merge-requests/N/head on the base repository.
//
// The canonical checkout is brought up to date exactly as Sync does, because the fetch needs a
// repository to land in, but its files are never moved off the branch: the fetched commit is
// written straight from the object store into the run's own directory. The project's file browser
// and every branch run keep reading the branch, and nothing has to put the checkout back afterward.
// Dependencies the commit declares are installed into the run's directory, not the canonical one.
//
// The caller compares the returned SHA against the commit it expected, which is how a ref that
// moved after a webhook named its head refuses to run.
func (s *Syncer) SyncRef(p *Project, sshKey, ref string) (*Worktree, error) {
	if err := ValidateRepoURL(p.RepoURL); err != nil {
		return nil, err
	}
	if err := ValidateFetchRef(ref); err != nil {
		return nil, err
	}
	l := s.lock(p.ID)
	l.Lock()
	defer l.Unlock()

	commit, err := s.fetchRefLocked(p, sshKey, ref)
	if err != nil {
		return nil, err
	}

	runDir, cleanup, err := s.newRunCheckout(p.ID)
	if err != nil {
		return nil, err
	}
	if err := writeCommit(commit, runDir); err != nil {
		cleanup()
		return nil, fmt.Errorf("write commit %s: %w", commit.Hash, err)
	}
	var wantRoles, wantCollections bool
	if p.InstallDeps {
		if wantRoles, wantCollections, err = s.installGalaxy(runDir); err != nil {
			cleanup()
			return nil, err
		}
	}
	return &Worktree{
		Dir:       runDir,
		SHA:       commit.Hash.String(),
		GalaxyEnv: galaxyEnvIn(runDir, wantRoles, wantCollections),
		cleanup:   cleanup,
	}, nil
}

// FetchRef fetches ref from the project's remote into the canonical checkout's object store and
// returns the commit it points at, without writing a worktree. It lets the commit be read with
// ReadCommit before any run is created, which is how a pull request's head is read for the gate.
func (s *Syncer) FetchRef(p *Project, sshKey, ref string) (string, error) {
	if err := ValidateRepoURL(p.RepoURL); err != nil {
		return "", err
	}
	if err := ValidateFetchRef(ref); err != nil {
		return "", err
	}
	l := s.lock(p.ID)
	l.Lock()
	defer l.Unlock()
	commit, err := s.fetchRefLocked(p, sshKey, ref)
	if err != nil {
		return "", err
	}
	return commit.Hash.String(), nil
}

// fetchRefLocked brings the canonical checkout up to date, fetches ref onto fetchedRef, and returns
// the commit it points at. The caller holds the project's lock and has validated the URL and ref.
func (s *Syncer) fetchRefLocked(p *Project, sshKey, ref string) (*object.Commit, error) {
	canonical, _, _, _, err := s.update(p, sshKey)
	if err != nil {
		return nil, err
	}
	repo, err := git.PlainOpen(canonical)
	if err != nil {
		return nil, fmt.Errorf("open checkout: %w", err)
	}
	auth, err := authFor(sshKey)
	if err != nil {
		return nil, err
	}
	spec := gitconfig.RefSpec("+" + ref + ":" + fetchedRef.String())
	err = repo.Fetch(&git.FetchOptions{Auth: auth, Force: true, RefSpecs: []gitconfig.RefSpec{spec}})
	if err != nil && err != git.NoErrAlreadyUpToDate {
		return nil, fmt.Errorf("fetch %s from %s: %w", ref, redactRepoURL(p.RepoURL), err)
	}
	fetched, err := repo.Reference(fetchedRef, true)
	if err != nil {
		return nil, fmt.Errorf("resolve fetched %s: %w", ref, err)
	}
	commit, err := repo.CommitObject(fetched.Hash())
	if err != nil {
		return nil, fmt.Errorf("read commit %s: %w", fetched.Hash(), err)
	}
	return commit, nil
}

// writeCommit writes every file of commit's tree under dir, which must exist and be empty. File
// modes are kept, so a script stays executable, and a symlink is written as a symlink rather than
// followed. A file is written only through real directories: on a case-insensitive filesystem a
// tree can carry a link and a directory whose names differ only by case, and creating the directory
// through the link would write outside the checkout. Anything under a .git directory is skipped, as
// the branch sync's copy skips it.
func writeCommit(commit *object.Commit, dir string) error {
	tree, err := commit.Tree()
	if err != nil {
		return err
	}
	return tree.Files().ForEach(func(f *object.File) error {
		first, _, _ := strings.Cut(f.Name, "/")
		if strings.EqualFold(first, ".git") {
			return nil
		}
		rel := filepath.FromSlash(f.Name)
		if !filepath.IsLocal(rel) {
			return fmt.Errorf("%s: %w", f.Name, ErrEscapesRepo)
		}
		if err := realParents(dir, rel); err != nil {
			return fmt.Errorf("%s: %w", f.Name, err)
		}
		target := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if f.Mode == filemode.Symlink {
			link, err := f.Contents()
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		}
		perm := os.FileMode(0o644)
		if f.Mode == filemode.Executable {
			perm = 0o755
		}
		return writeBlob(f, target, perm)
	})
}

// realParents reports an error when any directory on the way from dir to rel already exists as
// something other than a real directory. The first missing one ends the walk, since everything
// below it is about to be created as a plain directory.
func realParents(dir, rel string) error {
	cur := dir
	for _, part := range strings.Split(filepath.Dir(rel), string(filepath.Separator)) {
		if part == "." || part == "" {
			continue
		}
		cur = filepath.Join(cur, part)
		info, err := os.Lstat(cur)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return ErrEscapesRepo
		}
	}
	return nil
}

// writeBlob copies one file's blob to target with perm, refusing to replace anything already there.
func writeBlob(f *object.File, target string, perm os.FileMode) error {
	in, err := f.Reader()
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
