package dispatch

import (
	"io"
	"os"

	"github.com/kordloom/switchtender/internal/run"
)

// maxPlaybookGradeBytes caps how much of a playbook is read to grade a submission.
const maxPlaybookGradeBytes = 1 << 20

// graded returns a copy of r carrying the best reversibility grade available at submission time.
//
// The gate is the reason this grade exists. A rule written to hold anything that cannot be undone
// has to fire before the run executes, and at that moment there is no outcome to read: the only
// evidence is the playbook, whose text the run names but does not carry.
//
// It returns a copy rather than filling the run in place. The grade is computed, never stored, and
// the run is about to be written and folded into the audit chain's content digest, so attaching a
// derived field to the object being persisted would change what the receipt commits to.
func graded(r *run.Run) *run.Run {
	if r == nil {
		return nil
	}
	if run.NormalizeTool(r.Tool) != run.ToolAnsible || r.Playbook == "" {
		return r
	}
	f, err := os.Open(r.Playbook)
	if err != nil {
		return r
	}
	defer func() { _ = f.Close() }()
	content, err := io.ReadAll(io.LimitReader(f, maxPlaybookGradeBytes))
	if err != nil {
		return r
	}
	undo := run.AssessReversibilityFrom(r, run.ReversibilityEvidence{Playbook: content})
	cp := *r
	cp.Reversibility = &undo
	return &cp
}
