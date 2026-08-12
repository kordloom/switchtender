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

// MaxRegisterRuns is how many changes one register renders when the caller names no other bound.
// The register is a single HTML document held whole in memory and handed to a browser, so a period
// nobody bounded is a document nobody can open and a request that can take the process down with
// it. The bound is spent in the store query rather than on the rows after they arrive, because a
// hundred thousand runs already decoded have already cost what the bound exists to save.
const MaxRegisterRuns = 5000

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
	// Runs are the period's top-level runs, oldest first, at most Limit of them.
	Runs []*run.Run
	// Limit is the cap the store query carried, so the document can say what bound it was read
	// under rather than leaving the reader to assume there was none.
	Limit int
	// Truncated reports that the period holds more changes than Limit, so Runs is the earliest
	// page of it and not the whole period.
	Truncated bool
	// CoveredTo is the creation time of the first change left out, the point the document stops
	// covering. It is zero when Truncated is false. A caller writing consecutive registers resumes
	// from here, since the changes at this instant are the ones the page cut through.
	CoveredTo time.Time
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

// CollectRegister gathers the period's change register: the window's top-level runs, the
// chain-recorded decision over each, and the chain's own verdict, in one streaming pass. limit
// caps how many changes the document carries, defaulting to MaxRegisterRuns when it is not
// positive, and a period holding more than that comes back marked truncated.
func CollectRegister(ctx context.Context, runs run.Store, audits audit.Store, from, to, now time.Time,
	limit int) (*RegisterInput, error) {
	if limit <= 0 {
		limit = MaxRegisterRuns
	}
	in := &RegisterInput{From: from, To: to, GeneratedAt: now, Limit: limit,
		Decisions: map[string]Decision{}}

	// One row past the cap is asked for and never rendered. It is what separates a period that
	// ends exactly on the cap from one that runs past it, and its creation time is the instant the
	// document stops covering, which is what a caller writing the next register has to resume from.
	rows, err := runs.ListPage(ctx, run.ListFilter{After: from, Before: to, OldestFirst: true},
		limit+1, 0)
	if err != nil {
		return nil, fmt.Errorf("list runs: %w", err)
	}
	if len(rows) > limit {
		in.Truncated = true
		in.CoveredTo = rows[limit].CreatedAt
		rows = rows[:limit]
		// Rows sharing the instant the page cut through are dropped, so the document covers a
		// clean half-open range and the register resuming at that instant repeats nothing. When
		// the whole page shares it there is no clean cut, and the rows stay: the next register
		// then repeats them, which an auditor can reconcile, where dropping them would lose them.
		if kept := trimAt(rows, in.CoveredTo); len(kept) > 0 {
			rows = kept
		}
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
	err = audits.ChainScan(ctx, 0, func(e *audit.Entry) error {
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

// trimAt returns rows, which are oldest first, without the tail created at or after at.
func trimAt(rows []*run.Run, at time.Time) []*run.Run {
	cut := len(rows)
	for cut > 0 && !rows[cut-1].CreatedAt.Before(at) {
		cut--
	}
	return rows[:cut]
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
	// Truncated reports that the period held more changes than the document carries.
	Truncated bool
	// Limit is the cap the store query carried, named in the truncation notice.
	Limit int
	// CoveredTo is the instant the document stops covering, formatted, empty unless Truncated.
	CoveredTo string
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
		Truncated:   in.Truncated,
		Limit:       in.Limit,
	}
	// A truncated register that does not say so is the worst artifact this package can produce: it
	// reads as the whole period, so an auditor sampling it believes they saw everything. The notice
	// is rendered from the same fields the bound was applied with.
	if in.Truncated {
		v.CoveredTo = in.CoveredTo.UTC().Format(time.RFC3339)
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
