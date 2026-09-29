package dossier

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"html/template"
	"strings"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/run"
)

// registerTemplateSource is the self-contained HTML change register.
//
//go:embed register.html
var registerTemplateSource string

// registerTemplate is the parsed change register template.
var registerTemplate = template.Must(template.New("register").Parse(registerTemplateSource))

// Decision is one approval or rejection read from the chain.
type Decision struct {
	// Verdict is Approved or Rejected.
	Verdict string
	// Actor is who decided.
	Actor string
	// At is when.
	At time.Time
	// Seq is the chain entry recording it.
	Seq int64
}

// RegisterInput is everything the change register renders, gathered by CollectRegister.
type RegisterInput struct {
	// From and To bound the period, half open: From inclusive, To exclusive.
	From, To time.Time
	// Runs are the period's top-level runs, oldest first.
	Runs []*run.Run
	// Decisions maps a run id to its approval or rejection, where the chain records one.
	Decisions map[string]Decision
	// ChainOK reports whether the whole chain verified during collection.
	ChainOK bool
	// ChainBrokeAt is the one-based position of the first break when ChainOK is false.
	ChainBrokeAt int
	// ChainCount is how many entries the chain holds.
	ChainCount int
	// Head is the chain head at collection, the receipt the document carries.
	Head *audit.Entry
	// Anchored is how many anchors still hold against the chain.
	Anchored int
	// AnchorProblems describes each anchor the chain no longer satisfies.
	AnchorProblems []string
	// GeneratedAt is when the register was collected.
	GeneratedAt time.Time
}

// CollectRegister gathers the period's change register: every top-level run in the window, the
// chain-recorded decision over each, and the chain's own verdict, in one streaming pass.
func CollectRegister(ctx context.Context, runs run.Store, audits audit.Store, from, to, now time.Time) (*RegisterInput, error) {
	in := &RegisterInput{From: from, To: to, GeneratedAt: now, Decisions: map[string]Decision{}}

	rows, err := runs.ListPage(ctx, run.ListFilter{After: from, Before: to, OldestFirst: true}, 0, 0)
	if err != nil {
		return nil, fmt.Errorf("list runs: %w", err)
	}
	in.Runs = rows

	// The anchors are read before the walk so their links are held against the chain in the same
	// pass. Counting anchors without checking them is how a wholesale rewrite, which is
	// internally consistent and so passes the hash walk, earns a register that calls it verified.
	var anchors []*audit.Anchor
	if store, ok := audits.(audit.AnchorStore); ok {
		var aerr error
		if anchors, aerr = store.Anchors(ctx, 0); aerr != nil {
			return nil, fmt.Errorf("read audit anchors: %w", aerr)
		}
	}

	scan := audit.NewChainScanner(true)
	anchorScan := audit.NewAnchorScanner(anchors)
	err = audits.ChainScan(ctx, func(e *audit.Entry) error {
		scan.Feed(e)
		anchorScan.Feed(e)
		in.Head = e
		if id, verdict := decisionOf(e); id != "" {
			// The newest decision wins: a rejection redone as an approval reads as the chain
			// tells it, in order.
			in.Decisions[id] = Decision{Verdict: verdict, Actor: e.Actor, At: e.At, Seq: e.Seq}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan audit chain: %w", err)
	}
	in.ChainOK, in.ChainBrokeAt, in.ChainCount = scan.Result()

	// The register's subject is the whole period's chain rather than one run, so every anchor that
	// still holds counts. It folds through the same helper so a disowned anchor is reported here
	// for the same reason it is in a dossier.
	holding, problems := foldAnchors(anchorScan, 1)
	in.Anchored = len(holding)
	in.AnchorProblems = problems
	return in, nil
}

// decisionOf reads an approval or rejection from a chain entry's path, returning the run id it
// decided and the verdict, or empty strings for ordinary activity.
func decisionOf(e *audit.Entry) (id, verdict string) {
	switch {
	case strings.HasSuffix(e.Path, "/approve"):
		verdict = "Approved"
	case strings.HasSuffix(e.Path, "/reject"):
		verdict = "Rejected"
	default:
		return "", ""
	}
	parts := strings.Split(strings.TrimSuffix(strings.TrimSuffix(e.Path, "/approve"), "/reject"), "/")
	if len(parts) == 0 {
		return "", ""
	}
	return parts[len(parts)-1], verdict
}

// registerRow is one change as rendered.
type registerRow struct {
	// When is the run creation time, formatted.
	When string
	// Run is the run id.
	Run string
	// Change describes what the run did.
	Change string
	// Actor is who asked for it.
	Actor string
	// Source is where it came from.
	Source string
	// Risk is the graded blast radius.
	Risk string
	// Held names the approval rule that stopped this change, as it was named at the hold. Empty
	// when nothing held it.
	Held string
	// Decision is the chain-recorded approval or rejection, or a dash when none was required.
	Decision string
	// DecisionSeq is the chain entry recording the decision, zero when none.
	DecisionSeq int64
	// Outcome is the run's terminal or current status.
	Outcome string
	// DryRun is true for a no-change run.
	DryRun bool
}

// registerView is the data rendered into the register.
type registerView struct {
	// Status is the machine verdict class: verified, unanchored, or broken.
	Status string
	// StatusText is the banner sentence.
	StatusText string
	// From and To are the period bounds, formatted.
	From, To string
	// Rows are the changes, oldest first.
	Rows []registerRow
	// Total, Approved, Rejected, Held tally the rows.
	Total, Approved, Rejected int
	// Failed tallies rows whose outcome is a failure.
	Failed int
	// ChainCount is the whole chain's entry count.
	ChainCount int
	// Receipt is the chain head's seq:link at collection.
	Receipt string
	// AnchorProblems describes each anchor the chain no longer satisfies.
	AnchorProblems []string
	// GeneratedAt is when the register was made, formatted.
	GeneratedAt string
}

// RenderRegister writes the change register as a self-contained HTML document.
func RenderRegister(in *RegisterInput) ([]byte, error) {
	if in == nil {
		return nil, fmt.Errorf("render register: no input")
	}
	v := registerView{
		From: in.From.UTC().Format("2006-01-02"), To: in.To.UTC().Format("2006-01-02"),
		ChainCount:  in.ChainCount,
		GeneratedAt: in.GeneratedAt.UTC().Format(time.RFC3339),
		Total:       len(in.Runs),
	}
	if in.Head != nil {
		v.Receipt = fmt.Sprintf("%d:%s", in.Head.Seq, in.Head.Hash)
	}
	for _, r := range in.Runs {
		risk := run.AssessRisk(r)
		row := registerRow{
			When:    r.CreatedAt.UTC().Format("2006-01-02 15:04"),
			Run:     r.ID,
			Change:  changeOf(r),
			Actor:   r.Actor,
			Source:  strings.TrimSpace(r.Source + " " + r.SourceID),
			Risk:    risk.Level,
			Held:    r.HeldByPolicy,
			Outcome: string(r.Status),
			DryRun:  r.DryRun,
		}
		if d, ok := in.Decisions[r.ID]; ok {
			row.Decision = d.Verdict + " by " + d.Actor
			row.DecisionSeq = d.Seq
			switch d.Verdict {
			case "Approved":
				v.Approved++
			case "Rejected":
				v.Rejected++
			}
		}
		if r.Status == run.StatusFailed {
			v.Failed++
		}
		v.Rows = append(v.Rows, row)
	}
	v.AnchorProblems = in.AnchorProblems
	switch {
	case !in.ChainOK:
		v.Status = "broken"
		v.StatusText = fmt.Sprintf("The audit chain is broken at entry %d. This register cannot "+
			"be relied on until the break is explained.", in.ChainBrokeAt)
	case len(in.AnchorProblems) > 0:
		// A wholesale rewrite is self-consistent, so the hash walk passes and only the anchors
		// disagree. That disagreement is the finding.
		v.Status = "broken"
		v.StatusText = fmt.Sprintf("The chain no longer satisfies %d of its own anchors, so "+
			"history was rewritten or lost. This register cannot be relied on until that is "+
			"explained.", len(in.AnchorProblems))
	case in.Anchored == 0:
		v.Status = "unanchored"
		v.StatusText = "The chain verifies, but it is unanchored, so the record rests on this " +
			"install alone."
	default:
		v.Status = "verified"
		v.StatusText = fmt.Sprintf("The chain verifies and carries %d anchor(s) fixing it outside "+
			"this install.", in.Anchored)
	}
	var buf bytes.Buffer
	if err := registerTemplate.Execute(&buf, v); err != nil {
		return nil, fmt.Errorf("render register: %w", err)
	}
	return buf.Bytes(), nil
}

// changeOf describes what a run did in one line.
func changeOf(r *run.Run) string {
	tool := r.Tool
	if tool == "" {
		tool = "ansible"
	}
	what := r.Playbook
	if what == "" {
		what = r.Command
	}
	if len(what) > 80 {
		what = what[:77] + "..."
	}
	return strings.TrimSpace(tool + " " + what)
}
