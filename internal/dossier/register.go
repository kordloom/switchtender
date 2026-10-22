package dossier

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"html/template"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/binding"
	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/scrub"
	"github.com/kordloom/switchtender/internal/util"
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
	// OnBehalfOf is the account whose authority the decider used, empty when it acted as itself or
	// when the account only repeats the decider's name.
	OnBehalfOf string
	// At is when.
	At time.Time
	// Seq is the chain entry recording it.
	Seq int64
	// EntryID is the id of the chain entry recording it.
	EntryID string
	// StepID is the workflow approval step the decision was made on, empty for a whole run.
	StepID string
	// Seal is what holding the row the approval released to its seal found, one of the Seal
	// states, set by AttachRegisterSeals and empty when nothing checked it.
	Seal string
	// SealText says what the check found when the seal did not hold.
	SealText string
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
	// Pruned counts changes the chain records in this period that the run store no longer holds,
	// because retention removed them. It is zero on a truncated register, which cannot tell a
	// change the cap dropped from one retention removed, and which already says it is partial.
	//
	// It exists because their absence is otherwise indistinguishable from their never having
	// happened. A register is read as the account of a period, and retention deletes runs while
	// the chain keeps every entry, so a period whose runs have aged out rendered as a document
	// listing nothing and still calling itself verified: an auditor reads a quiet quarter where
	// there were forty changes, two of them failures and one refused by an approver. Saying how
	// many are missing turns a false account into an incomplete one, which is the difference
	// between misleading an auditor and telling them where to look.
	Pruned int
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
// chain-recorded decision over each, and the chain's own verdict, in one streaming pass.
// producer is the install identity the tree profile's leaves bind to, which checking a tree anchor
// requires. limit caps how many changes the document carries, defaulting to MaxRegisterRuns when
// it is not positive, and a period holding more than that comes back marked truncated.
func CollectRegister(ctx context.Context, runs run.Store, audits audit.Store,
	producer audit.Identity,
	from, to, now time.Time, limit int) (*RegisterInput, error) {
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

	// What the store still holds for this period, so the walk below can tell a pruned change from
	// one that is simply listed.
	listed := make(map[string]bool, len(rows))
	for _, r := range rows {
		listed[r.ID] = true
	}
	pruned := map[string]bool{}

	scan := audit.NewChainScanner(true)
	anchorScan := audit.NewAnchorScanner(anchors, producer)
	credits := creditedBy(ctx, runs)
	err = audits.ChainScan(ctx, 0, func(e *audit.Entry) error {
		scan.Feed(e)
		anchorScan.Feed(e)
		in.Head = e
		// A change the chain records inside this period whose run the store no longer holds has
		// been pruned by retention. The chain is the durable half of the record, so it is what
		// says the period was not empty.
		if id := outcomeOf(e); id != "" && !e.At.Before(from) && e.At.Before(to) && !listed[id] {
			pruned[id] = true
		}
		if id, verdict := decisionOf(e); id != "" && credits(e) {
			// The decision the run, or its approval step, stores as its winner is the one
			// credited. A run decided before winners were stored credits the newest decision,
			// as the chain tells it, in order.
			d := Decision{Verdict: verdict, Actor: e.Actor, OnBehalfOf: onBehalfOf(e),
				At: e.At, Seq: e.Seq, EntryID: e.ID}
			if _, stepID, _, isStep := outcome.ParseStepDecisionPath(e.Path); isStep {
				d.StepID = stepID
			}
			in.Decisions[id] = d
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
	// Counted only when this document is the whole period. The membership set is built from the
	// page the cap already cut, so on a truncated register every change the cap dropped looks
	// exactly like one retention removed, and the banner then tells an auditor that runs still
	// sitting in the store were destroyed. Distinguishing the two would mean asking the store about
	// every change beyond the cap, which on the period that reaches the cap is the whole point of
	// having one.
	//
	// So a truncated register says nothing here and says on its face that it is not the whole
	// period, which is the honest pair. Under-reporting a pruned period is silence; over-reporting
	// one is an accusation of data destruction that the store itself contradicts.
	if !in.Truncated {
		in.Pruned = len(pruned)
	}
	return in, nil
}

// AttachRegisterSeals holds the row each credited approval in the register released, the run or
// its approval step, to the seal the process made over it, which covers who the approval's entry
// says decided, and records what each check found on the decision. A rejection seals nothing and is
// left unchecked. A nil verifier, an install with no binding key, marks every approval unchecked,
// and so does a run the store no longer holds, which retention pruned: neither is a drift.
func AttachRegisterSeals(ctx context.Context, in *RegisterInput, v *binding.Verifier,
	runs run.Store) {
	if in == nil {
		return
	}
	listed := make(map[string]*run.Run, len(in.Runs))
	for _, r := range in.Runs {
		listed[r.ID] = r
	}
	for id, d := range in.Decisions {
		if d.Verdict != "Approved" {
			continue
		}
		d.Seal, d.SealText = registerSeal(ctx, v, runs, listed[id], id, d)
		in.Decisions[id] = d
	}
}

// registerSeal checks the row the approval d on run id released and returns the Seal state and,
// when it did not hold, what the check found. r is the run as listed, nil to read it from runs.
func registerSeal(ctx context.Context, v *binding.Verifier, runs run.Store, r *run.Run, id string,
	d Decision) (string, string) {
	if v == nil {
		return SealUnchecked, noKeyText
	}
	if r == nil {
		got, err := runs.Get(ctx, id)
		switch {
		case errors.Is(err, run.ErrNotFound):
			return SealUnchecked, "the run is no longer in the store, which retention prunes"
		case err != nil:
			return SealUnchecked, "the run could not be read: " + err.Error()
		}
		r = got
	}
	var err error
	switch {
	case d.StepID != "":
		var node *run.Run
		if node, err = runs.Get(ctx, d.StepID); err != nil {
			return SealUnchecked, "the approval step could not be read: " + err.Error()
		}
		err = gateCheck(ctx, v, runs, r.ID, node)
	case r.DecisionID == "":
		if err = v.Sealed(ctx, r); err == nil {
			err = fmt.Errorf("run %s: %w", r.ID, errNamesNoDecision)
		}
	default:
		err = v.Sealed(ctx, r)
	}
	c := sealCheckOf(r.ID, "", d.EntryID, err)
	return c.Status, c.Text
}

// trimAt returns rows, which are oldest first, without the tail created at or after at.
func trimAt(rows []*run.Run, at time.Time) []*run.Run {
	cut := len(rows)
	for cut > 0 && !rows[cut-1].CreatedAt.Before(at) {
		cut--
	}
	return rows[:cut]
}

// creditedBy returns the check the register credits a decision entry with: the run, or the approval
// step, it names must credit it. The subjects are read as decision entries arrive, which are few.
// A subject that cannot be read is credited, as it was before winners were stored.
func creditedBy(ctx context.Context, runs run.Store) func(*audit.Entry) bool {
	subjects := map[string]*run.Run{}
	return func(e *audit.Entry) bool {
		id, ok := strings.CutPrefix(e.Path, "/runs/")
		if !ok {
			return true
		}
		if _, stepID, _, isStep := outcome.ParseStepDecisionPath(e.Path); isStep {
			id = stepID
		} else {
			id, _, _ = strings.Cut(id, "/")
		}
		subject, seen := subjects[id]
		if !seen {
			got, err := runs.Get(ctx, id)
			if err != nil {
				return true
			}
			subject, subjects[id] = got, got
		}
		return outcome.Credits(subject, e.ID)
	}
}

// decisionOf reads an approval or rejection from a chain entry, returning the run id it decided
// and the verdict, or empty strings for ordinary activity.
//
// Only the committed DECISION entry counts. The HTTP attempt the middleware records looks almost
// identical, but it is written whether or not the decision took: a self-approval the separation
// gate refused still logged the attempt, and the register then read "Approved by" the requester,
// naming the wrong person in the one document built for an auditor. A later failed re-approve
// likewise overwrote the true approver. The DECISION entry is written only when the decision
// committed, carries the deciding actor, and its digest binds the exact spec decided on.
func decisionOf(e *audit.Entry) (id, verdict string) {
	if e.Method != audit.MethodDecision {
		return "", ""
	}
	// A workflow approval step's decision is credited to the workflow it released or stopped, so a
	// workflow that waited for a person mid-run does not read as one nobody decided on. Its request
	// and its timeout are not decisions anybody made, so they are not credited.
	if runID, _, verdict, ok := outcome.ParseStepDecisionPath(e.Path); ok {
		switch verdict {
		case outcome.StepApproved:
			return runID, "Approved"
		case outcome.StepRejected:
			return runID, "Rejected"
		}
		return "", ""
	}
	runID, v, ok := strings.Cut(strings.TrimPrefix(e.Path, "/runs/"), "/decision/")
	if !ok || runID == "" {
		return "", ""
	}
	switch v {
	case "approved":
		return runID, "Approved"
	case "rejected":
		return runID, "Rejected"
	}
	return "", ""
}

// RegisterRow is one change in the register: one row of the document's table and one element of
// the JSON form's rows, built once by BuildRegister for both.
type RegisterRow struct {
	// When is the run creation time in UTC, formatted as YYYY-MM-DD HH:MM.
	When string `json:"when"`
	// Run is the run id.
	Run string `json:"run"`
	// Change describes what the run did, with any secret its script assigned redacted.
	Change string `json:"change"`
	// Actor is who asked for it, empty when the run records nobody.
	Actor string `json:"actor"`
	// Source is where it came from.
	Source string `json:"source"`
	// Risk is the graded blast radius.
	Risk string `json:"risk"`
	// Held names the approval rule that stopped this change, as it was named at the hold. Empty
	// when nothing held it.
	Held string `json:"held"`
	// Decision is the chain-recorded approval or rejection, empty when none was required.
	Decision string `json:"decision"`
	// DecisionSeq is the chain entry recording the decision, zero when none.
	DecisionSeq int64 `json:"decision_seq"`
	// Outcome is the run's terminal or current status.
	Outcome string `json:"outcome"`
	// DryRun is true for a run submitted in its tool's no-change mode.
	DryRun bool `json:"dry_run"`
	// NotChangeFree is true for a dry run the gate did not find change free: its playbook forces
	// work to run for real, or its configuration runs a program while it plans, or the scan could
	// not read all of it. That is not the no-change run the dry-run mark alone would claim.
	NotChangeFree bool `json:"not_change_free"`
}

// Register is the change register as data: what the HTML document renders and what the JSON form
// of the register endpoint returns. Both are built by BuildRegister from one RegisterInput, so the
// page a reader opens and the document they download cannot disagree.
type Register struct {
	// Status is the machine verdict class: verified, unanchored, or broken.
	Status string `json:"status"`
	// StatusText is the banner sentence.
	StatusText string `json:"status_text"`
	// From is the first day of the period, inclusive, formatted as a date.
	From string `json:"from"`
	// To is the day the period ends, exclusive, formatted as a date.
	To string `json:"to"`
	// Rows are the changes, oldest first.
	Rows []RegisterRow `json:"rows"`
	// Total counts the rows.
	Total int `json:"total"`
	// Approved counts the rows whose decision is an approval.
	Approved int `json:"approved"`
	// Rejected counts the rows whose decision is a rejection.
	Rejected int `json:"rejected"`
	// Truncated reports that the period held more changes than the document carries.
	Truncated bool `json:"truncated"`
	// TruncatedText is the notice a truncated register carries, naming the cap, how many changes it
	// holds, and the instant it stops covering. Empty unless Truncated.
	TruncatedText string `json:"truncated_text"`
	// Limit is the cap the store query carried, named in the truncation notice.
	Limit int `json:"limit"`
	// CoveredTo is the instant the document stops covering, formatted, empty unless Truncated.
	CoveredTo string `json:"covered_to"`
	// Failed tallies rows whose outcome is a failure.
	Failed int `json:"failed"`
	// Drifted tallies approvals whose row's seal no longer matches the entry naming the approver.
	Drifted int `json:"drifted"`
	// Pruned counts changes the chain records in this period that the run store no longer holds,
	// so the document says it is incomplete rather than reading as an account of a quiet period.
	Pruned int `json:"pruned"`
	// PrunedText is the notice an incomplete register carries, naming how many changes retention
	// removed. Empty when Pruned is zero.
	PrunedText string `json:"pruned_text"`
	// ChainCount is the whole chain's entry count.
	ChainCount int `json:"chain_count"`
	// Receipt is the chain head's seq:link at collection.
	Receipt string `json:"receipt"`
	// AnchorProblems describes each anchor the chain no longer satisfies.
	AnchorProblems []string `json:"anchor_problems"`
	// GeneratedAt is when the register was made, formatted.
	GeneratedAt string `json:"generated_at"`
}

// BuildRegister turns a collected register into the rows, tallies, verdict, and notices the
// document renders. It is the only place rows are made, so the HTML document and the JSON form of
// the register endpoint carry the same changes in the same words.
func BuildRegister(in *RegisterInput) (*Register, error) {
	if in == nil {
		return nil, fmt.Errorf("build register: no input")
	}
	v := &Register{
		From: in.From.UTC().Format("2006-01-02"), To: in.To.UTC().Format("2006-01-02"),
		ChainCount:  in.ChainCount,
		GeneratedAt: in.GeneratedAt.UTC().Format(time.RFC3339),
		Total:       len(in.Runs),
		Truncated:   in.Truncated,
		Pruned:      in.Pruned,
		Limit:       in.Limit,
		Rows:        make([]RegisterRow, 0, len(in.Runs)),
	}
	// A truncated register that does not say so is the worst artifact this package can produce: it
	// reads as the whole period, so an auditor sampling it believes they saw everything. The notice
	// is rendered from the same fields the bound was applied with.
	if in.Truncated {
		v.CoveredTo = in.CoveredTo.UTC().Format(time.RFC3339)
		v.TruncatedText = fmt.Sprintf("This register is truncated and is not the whole period. A "+
			"register carries at most %d changes; this one carries the earliest %d and stops at %s. "+
			"Changes created after %s are not in this document. The register that starts at %s "+
			"carries the rest of the period.", v.Limit, v.Total, v.CoveredTo, v.CoveredTo,
			v.CoveredTo)
	}
	if in.Pruned > 0 {
		v.PrunedText = fmt.Sprintf("This register is incomplete. The chain records %d further %s in "+
			"this period whose runs this install no longer holds, because retention removed them. "+
			"Their entries remain in the chain and still verify; what they did is no longer readable "+
			"here.", in.Pruned, util.Plural(in.Pruned, "change", "changes"))
	}
	if in.Head != nil {
		v.Receipt = fmt.Sprintf("%d:%s", in.Head.Seq, in.Head.Hash)
	}
	for _, r := range in.Runs {
		risk := run.AssessRisk(r)
		row := RegisterRow{
			When:          r.CreatedAt.UTC().Format("2006-01-02 15:04"),
			Run:           r.ID,
			Change:        changeOf(r),
			Actor:         r.Actor,
			Source:        strings.TrimSpace(r.Source + " " + r.SourceID),
			Risk:          risk.Level,
			Held:          r.HeldByPolicy,
			Outcome:       string(r.Status),
			DryRun:        r.DryRun,
			NotChangeFree: r.DryRun && !r.ChangeFree(),
		}
		if d, ok := in.Decisions[r.ID]; ok {
			// A decision an install serving open on loopback recorded before decisions named their
			// caller carries no actor, and "Approved by" followed by nothing reads as a name that
			// failed to render.
			row.Decision = d.Verdict + " by " + d.Actor
			if d.Actor == "" {
				row.Decision = d.Verdict + " with no actor recorded"
			}
			// A token's label alone reads the same for two tokens on different accounts, so the
			// account it acted for follows it.
			if d.OnBehalfOf != "" {
				row.Decision += " on behalf of " + d.OnBehalfOf
			}
			// The name is the approval entry's, which a writer can rewrite; the seal over the row
			// it released is what says whether it still stands.
			switch d.Seal {
			case SealDrifted:
				row.Decision += ", seal drifted"
				v.Drifted++
			case SealUnsealed:
				row.Decision += ", unsealed"
			case SealUnchecked:
				row.Decision += ", seal unchecked"
			}
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
			"install alone. One command fixes its head at a public timestamp authority: " +
			"switchtender audit anchor. An independent witness (switchtender witness) can then " +
			"countersign what it saw."
	default:
		v.Status = "verified"
		v.StatusText = fmt.Sprintf("The chain verifies and carries %d %s fixing it outside this "+
			"install.", in.Anchored, util.Plural(in.Anchored, "anchor", "anchors"))
	}
	if v.Drifted > 0 {
		v.StatusText += fmt.Sprintf(" %d %s no longer %s the seal on the row %s released, so the "+
			"%s on %s not trusted.", v.Drifted, util.Plural(v.Drifted, "approval", "approvals"),
			util.Plural(v.Drifted, "matches", "match"), util.Plural(v.Drifted, "it", "they"),
			util.Plural(v.Drifted, "name", "names"), util.Plural(v.Drifted, "it is", "them are"))
	}
	return v, nil
}

// RenderRegister writes the change register as a self-contained HTML document.
func RenderRegister(in *RegisterInput) ([]byte, error) {
	if in == nil {
		return nil, fmt.Errorf("render register: no input")
	}
	v, err := BuildRegister(in)
	if err != nil {
		return nil, fmt.Errorf("render register: %w", err)
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
		// A bash, python, powershell, or go run keeps its whole script here, and a script carries
		// whatever it was written with: an inline password, a connection string, a token on a command
		// line. The register is the one document built to be mailed to an outside auditor, so it
		// published them verbatim in the Change column and in the CSV export beside it. The dossier
		// already runs this same field through the same redactor for the same reason; this is the
		// derived document that skipped it. Redacted before the truncation, so a secret cannot
		// survive as a fragment of a cut line.
		what, _ = util.RedactAssignments(r.Command, scrub.Marker)
	}
	// The cut is counted in runes, not bytes. A byte cut split the last character of a script
	// written in any non-ASCII alphabet, so the Change column and the CSV export beside it carried
	// bytes that are not valid UTF-8, and a reader that validates its input rejects the whole
	// document over one row.
	if utf8.RuneCountInString(what) > 80 {
		what = string([]rune(what)[:77]) + "..."
	}
	return strings.TrimSpace(tool + " " + what)
}

// outcomeOf returns the run id an entry records the outcome of, or empty when it is not one.
//
// The outcome entry is used rather than the creation entry because it is the only one that names
// its run: a creation is recorded before the run has an id. It is written once when the run
// finishes, so counting them counts changes rather than chain entries, and the instant it carries
// is the completion rather than the creation the period is otherwise measured by. For a run that
// starts and finishes inside a period those agree, and for one that straddles a boundary the count
// is off by that run in whichever direction it crossed, which is a better answer than silence.
func outcomeOf(e *audit.Entry) string {
	if e.Method != audit.MethodRun {
		return ""
	}
	id, _, ok := strings.Cut(strings.TrimPrefix(e.Path, "/runs/"), "/outcome/")
	if !ok {
		return ""
	}
	return id
}
