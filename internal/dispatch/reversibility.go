package dispatch

import (
	"io/fs"
	"os"
	"path"
	"path/filepath"

	"github.com/kordloom/switchtender/internal/run"
)

// CheckoutReader reads a project's files as they stood at one commit.
type CheckoutReader interface {
	// ReadCommit calls fn with the project's files at commit, or at the checkout's current commit
	// when commit is empty, and the full hash that resolved to.
	ReadCommit(projectID, commit string, fn func(fsys fs.FS, sha string) error) error
}

// graded returns a copy of r carrying the best reversibility grade available at submission time.
//
// The gate is the reason this grade exists. A rule written to hold anything that cannot be undone
// has to fire before the run executes, and at that moment there is no outcome to read: the only
// evidence is the playbook and what it pulls in, whose text the run names but does not carry.
//
// It returns a copy rather than filling the run in place. The grade is computed, never stored, and
// the run is about to be written and folded into the audit chain's content digest, so attaching a
// derived field to the object being persisted would change what the receipt commits to.
func (d *Dispatcher) graded(r *run.Run) *run.Run {
	if d.syncer == nil {
		return gradedFrom(r, nil)
	}
	return gradedFrom(r, d.syncer)
}

// gradedLocally grades r without reading any project checkout. It serves the plan gate's free
// functions, whose runs are Terraform or OpenTofu applies, which name no playbook to read.
func gradedLocally(r *run.Run) *run.Run {
	return gradedFrom(r, nil)
}

// gradedFrom grades r from its playbook, read through checkouts for a run drawn from a project.
func gradedFrom(r *run.Run, checkouts CheckoutReader) *run.Run {
	pb := ScanRunPlaybook(r, checkouts)
	if pb == nil {
		return r
	}
	undo := run.AssessReversibilityFrom(r, run.ReversibilityEvidence{Playbook: pb})
	cp := *r
	cp.Reversibility = &undo
	return &cp
}

// ScanRunPlaybook scans the playbook r runs and everything it pulls in, from wherever that
// playbook lives: the project checkout for a run drawn from a project, the server's filesystem for
// a playbook named by path. It returns nil when there is no playbook or it could not be read, which
// leaves the grade where the run's own fields put it.
//
// A project run's playbook path is relative to the project. It used to be opened relative to the
// server's working directory instead, which for a project run is never the project: the grade read
// nothing, or read whatever file of the same name happened to sit beside the server.
func ScanRunPlaybook(r *run.Run, checkouts CheckoutReader) *run.PlaybookSignals {
	if r == nil || run.NormalizeTool(r.Tool) != run.ToolAnsible || r.Playbook == "" {
		return nil
	}
	if r.ProjectID != "" {
		return scanProjectPlaybook(r, checkouts)
	}
	return scanLocalPlaybook(r.Playbook)
}

// scanProjectPlaybook scans a project run's playbook at the commit that decides what the run
// executes: the commit it executed, else the one it is pinned to, else the checkout's current one.
//
// The current commit is what a run being submitted is graded on, and the gate fetches the project
// first so it is the commit the run is about to execute. A held run is pinned when it is held, and
// from then on is read at exactly the commit it will execute, however far the branch moves.
func scanProjectPlaybook(r *run.Run, checkouts CheckoutReader) *run.PlaybookSignals {
	if checkouts == nil {
		return nil
	}
	commit, which := r.CommitSHA, "the commit this run executed"
	if commit == "" && r.PinnedCommit != "" {
		commit, which = r.PinnedCommit, "the commit this run is pinned to"
	}
	if commit == "" {
		which = "the project's last synced commit"
	}
	var out *run.PlaybookSignals
	err := checkouts.ReadCommit(r.ProjectID, commit, func(fsys fs.FS, sha string) error {
		signals, err := run.ScanPlaybookIn(fsys, path.Clean(r.Playbook), "the project")
		if err != nil {
			return err
		}
		signals.Source = "read at commit " + sha[:min(12, len(sha))] + ", " + which
		out = &signals
		return nil
	})
	if err != nil {
		return nil
	}
	return out
}

// scanLocalPlaybook scans a playbook named by a path on this server. The path is read wherever the
// run names it, as it always was, and the directory it resolves to bounds everything else the scan
// reads, so a role or include cannot reach a file outside it.
func scanLocalPlaybook(name string) *run.PlaybookSignals {
	abs, err := filepath.Abs(name)
	if err != nil {
		return nil
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil
	}
	root, err := os.OpenRoot(filepath.Dir(real))
	if err != nil {
		return nil
	}
	defer func() { _ = root.Close() }()
	signals, err := run.ScanPlaybookIn(root.FS(), filepath.Base(real), "the playbook's directory")
	if err != nil {
		return nil
	}
	return &signals
}
