package dispatchtest

import (
	"context"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/kordloom/switchtender/internal/project"
)

// LocalProjectID is the id LocalProject stores its project under.
const LocalProjectID = "proj_local"

// HeldPlaybook is the playbook LocalProject commits when a test names no files of its own: one
// play that does nothing, which is all a test about a held run needs from it.
const HeldPlaybook = "---\n- hosts: all\n  tasks: []\n"

// Project is a project over a git repository in a folder of the test's own, with the store and the
// syncer a dispatcher reads it through.
type Project struct {
	// ID is the project's id in Store.
	ID string
	// Store holds the project.
	Store project.Store
	// Syncer syncs the project into a cache of the test's own.
	Syncer *project.Syncer
	// Repo is the repository's folder, which the project's remote names.
	Repo string
	// Head is the commit main held when the repository was built.
	Head string
}

// LocalProject builds a git repository committing files on main, and a project named
// LocalProjectID whose remote is that folder. A run an approval stands in front of must come from
// a project, so a test that holds an Ansible or Terraform run names this one. A nil files commits
// site.yml holding HeldPlaybook. The repository is built in this process and synced by the
// in-process file transport, so no git binary is needed.
func LocalProject(t *testing.T, files map[string]string) Project {
	t.Helper()
	if files == nil {
		files = map[string]string{"site.yml": HeldPlaybook}
	}
	dir := t.TempDir()
	WriteFiles(t, dir, files)
	repo, err := git.PlainInitWithOptions(dir, &git.PlainInitOptions{
		InitOptions: git.InitOptions{DefaultBranch: plumbing.Main},
	})
	if err != nil {
		t.Fatalf("PlainInit(%s) error = %v", dir, err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree() error = %v", err)
	}
	if err := wt.AddGlob("."); err != nil {
		t.Fatalf("AddGlob() error = %v", err)
	}
	head, err := wt.Commit("first", &git.CommitOptions{
		Author: &object.Signature{Name: "test", Email: "test@example.com", When: time.Now()},
	})
	if err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	syncer, err := project.NewSyncer(t.TempDir())
	if err != nil {
		t.Fatalf("NewSyncer() error = %v", err)
	}
	store := project.NewMemStore()
	p := &project.Project{
		ID: LocalProjectID, Name: "local", RepoURL: dir, Branch: "main", CreatedAt: time.Now(),
	}
	if err := store.Save(context.Background(), p); err != nil {
		t.Fatalf("Save(project) error = %v", err)
	}
	return Project{ID: p.ID, Store: store, Syncer: syncer, Repo: dir, Head: head.String()}
}

// Fetch brings the project's checkout up to date, as an earlier sync of the project leaves it, so
// a gate that reads the checkout without fetching first finds the committed files.
func (p Project) Fetch(t *testing.T) {
	t.Helper()
	got, err := p.Store.Get(context.Background(), p.ID)
	if err != nil {
		t.Fatalf("Get(project) error = %v", err)
	}
	if _, err := p.Syncer.Fetch(got, ""); err != nil {
		t.Fatalf("Fetch(project) error = %v", err)
	}
}

// SaveIn stores the project in store as well, for a test whose dispatcher and server read projects
// from a store of their own.
func (p Project) SaveIn(t *testing.T, store project.Store) {
	t.Helper()
	got, err := p.Store.Get(context.Background(), p.ID)
	if err != nil {
		t.Fatalf("Get(project) error = %v", err)
	}
	if err := store.Save(context.Background(), got); err != nil {
		t.Fatalf("Save(project) error = %v", err)
	}
}
