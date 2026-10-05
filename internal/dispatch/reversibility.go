package dispatch

import (
	"errors"
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
	return gradedFrom(r, d.checkoutReader(), d.scanPlan)
}

// gradedLocally grades r without reading any project checkout. It serves the plan gate's free
// functions, whose runs are Terraform or OpenTofu applies, which name no playbook to read.
func gradedLocally(r *run.Run) *run.Run {
	return gradedFrom(r, nil, scanConfiguration)
}

// planScanner reads the Terraform or OpenTofu configuration a dry run of r plans.
type planScanner func(r *run.Run, checkouts CheckoutReader) run.DryRunScan

// checkoutReader returns the project checkouts the gate reads, or nil when there are none.
func (d *Dispatcher) checkoutReader() CheckoutReader {
	if d.syncer == nil {
		return nil
	}
	return d.syncer
}

// gradedFrom grades r from its playbook, read through checkouts for a run drawn from a project.
//
// A dry run also carries what the gate's scan of it found, so every rule matched against the copy,
// and the grade computed from it, treats a dry run that is not change free as the change it may be:
// an Ansible dry run whose playbook forces real work under check mode, or a Terraform or OpenTofu
// plan whose configuration runs a program while it plans. The run that executes is untouched: it
// still runs in its tool's no-change mode, and only the judgment about it changes.
func gradedFrom(r *run.Run, checkouts CheckoutReader, plans planScanner) *run.Run {
	pb, unread := scanRunPlaybook(r, checkouts)
	scan, scanned := dryRunScan(r, pb, unread, checkouts, plans)
	if pb == nil && !scanned {
		return r
	}
	cp := *r
	if scanned {
		cp.DryRunScans = []run.DryRunScan{scan}
	}
	if pb != nil {
		undo := run.AssessReversibilityFrom(&cp, run.ReversibilityEvidence{Playbook: pb})
		cp.Reversibility = &undo
	}
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
	pb, _ := scanRunPlaybook(r, checkouts)
	return pb
}

// scanRunPlaybook is ScanRunPlaybook that also says why a playbook it could not read was unread,
// empty when it was read or there was nothing to read.
func scanRunPlaybook(r *run.Run, checkouts CheckoutReader) (*run.PlaybookSignals, string) {
	if r == nil || run.NormalizeTool(r.Tool) != run.ToolAnsible || r.Playbook == "" {
		return nil, ""
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
func scanProjectPlaybook(r *run.Run, checkouts CheckoutReader) (*run.PlaybookSignals, string) {
	if checkouts == nil {
		return nil, noCheckout
	}
	commit, which := commitToRead(r)
	var out *run.PlaybookSignals
	var scanErr error
	err := checkouts.ReadCommit(r.ProjectID, commit, func(fsys fs.FS, sha string) error {
		signals, err := run.ScanPlaybookIn(fsys, path.Clean(r.Playbook), "the project")
		if err != nil {
			scanErr = err
			return err
		}
		signals.Source = readAt(sha, which)
		out = &signals
		return nil
	})
	switch {
	case err == nil:
		return out, ""
	case scanErr != nil:
		return nil, unreadWhy(scanErr, "not in the project")
	default:
		return nil, "the project's checkout could not be read"
	}
}

// scanLocalPlaybook scans a playbook named by a path on this server. The path is read wherever the
// run names it, as it always was, and the directory it resolves to bounds everything else the scan
// reads, so a role or include cannot reach a file outside it.
func scanLocalPlaybook(name string) (*run.PlaybookSignals, string) {
	root, base, err := openLocal(name, true)
	if err != nil {
		return nil, unreadWhy(err, "not on this server")
	}
	defer func() { _ = root.Close() }()
	signals, err := run.ScanPlaybookIn(root.FS(), base, "the playbook's directory")
	if err != nil {
		return nil, unreadWhy(err, "not on this server")
	}
	return &signals, ""
}

// noCheckout says why a project run's files could not be read on a server holding no checkouts.
const noCheckout = "no checkout of its project is on this server"

// commitToRead returns the commit that decides what a project run executes, and how to describe it:
// the commit it executed, else the one it is pinned to, else the checkout's current one, which is
// what a run being submitted is about to execute.
func commitToRead(r *run.Run) (string, string) {
	switch {
	case r.CommitSHA != "":
		return r.CommitSHA, "the commit this run executed"
	case r.PinnedCommit != "":
		return r.PinnedCommit, "the commit this run is pinned to"
	}
	return "", "the project's last synced commit"
}

// readAt describes the commit a scan read, for its record.
func readAt(sha, which string) string {
	return "read at commit " + sha[:min(12, len(sha))] + ", " + which
}

// openLocal opens the directory a path on this server resolves to, with every symbolic link
// followed, so a scan reads inside it and nowhere else. For a file it opens the file's directory
// and returns the file's name within it; for a directory, the directory itself and ".".
func openLocal(name string, file bool) (*os.Root, string, error) {
	abs, err := filepath.Abs(name)
	if err != nil {
		return nil, "", err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, "", err
	}
	dir, base := real, "."
	if file {
		dir, base = filepath.Dir(real), filepath.Base(real)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, "", err
	}
	return root, base, nil
}

// unreadWhy says why a file could not be read, in the words a scan's record uses: missing gives
// missing, and anything else could not be read or parsed.
func unreadWhy(err error, missing string) string {
	if errors.Is(err, fs.ErrNotExist) {
		return missing
	}
	return "it could not be read or parsed"
}
