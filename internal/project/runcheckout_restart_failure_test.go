package project_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kordloom/switchtender/internal/project"
)

// TestReplicaRestartAfterRestoreKeepsLiveRunCheckouts starts a second syncer on the project cache
// a first one is executing a run from, the way a restored install's second replica, a worker
// restarted by its supervisor, or a second serve process starts on a host whose account already
// runs one. Every serve and worker on a host uses the same default cache, the user cache directory.
//
// NewSyncer clears the whole run checkout directory on start, to remove what a crash left behind,
// with no way to tell a dead checkout from one another process is executing from. The first
// process's live run loses its playbooks, roles, and includes mid-run, and the project lock that
// would have serialized the two is a mutex inside one process. The same unshared lock lets two
// processes clone and reset one canonical checkout at once: the probe that found this, two
// replicas on a restored PostgreSQL install launching two templates of one project, failed one run
// at sync with "clone ...: already up-to-date".
func TestReplicaRestartAfterRestoreKeepsLiveRunCheckouts(t *testing.T) {
	t.Parallel()
	repo := initRepo(t)
	cache := t.TempDir()
	first, err := project.NewSyncer(cache)
	if err != nil {
		t.Fatalf("NewSyncer() first process error = %v", err)
	}
	live, err := first.Sync(&project.Project{ID: "proj_live", RepoURL: repo, Branch: "main"}, "")
	if err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	defer live.Cleanup()
	playbook := filepath.Join(live.Dir, "site.yml")
	if _, err := os.Stat(playbook); err != nil {
		t.Fatalf("the live run's checkout has no playbook before the second start: %v", err)
	}

	if _, err := project.NewSyncer(cache); err != nil {
		t.Fatalf("NewSyncer() second process error = %v", err)
	}
	if _, err := os.Stat(playbook); err != nil {
		t.Errorf("a second process starting on the same cache deleted the checkout a live run "+
			"executes from: %v", err)
	}
}
