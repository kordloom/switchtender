// Package dossier assembles and renders the per-run evidence dossier: one self-contained HTML
// document tying a run's spec, provenance, approval decisions, host outcomes, chain entries,
// receipt, and anchors together, so an auditor's sample request is answered with one document
// instead of five screenshots.
package dossier

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"html/template"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/run"
)

// templateSource is the self-contained HTML dossier.
//
//go:embed dossier.html
var templateSource string

// dossierTemplate is the parsed dossier template.
var dossierTemplate = template.Must(template.New("dossier").Parse(templateSource))

// Input is everything the renderer needs, gathered by Collect.
// The fields carry JSON names because the same collected record is served as data, not only rendered
// as a page: a program reading a run's evidence, an AI agent above all, needs the record rather than
// the markup.
type Input struct {
	// Run is the run the dossier is about.
	Run *run.Run `json:"run"`
	// Hosts is the run's per-host outcome, derived from its recorded events.
	Hosts []run.HostSummary `json:"hosts,omitempty"`
	// Entries are the chain entries that recorded this run: its creation, its approval or
	// rejection, its cancellation, in chain order.
	Entries []*audit.Entry `json:"entries,omitempty"`
	// ChainOK reports whether the whole chain verified during collection.
	ChainOK bool `json:"chain_ok"`
	// ChainBrokeAt is the one-based position of the first break when ChainOK is false.
	ChainBrokeAt int `json:"chain_broke_at,omitempty"`
	// ChainCount is how many entries the chain holds.
	ChainCount int `json:"chain_count"`
	// Head is the chain head at collection, the receipt the document carries.
	Head *audit.Entry `json:"head,omitempty"`
	// Covering are the anchors that still hold against the chain and sit at or after the position
	// the run was recorded by, each one an independent fixation of history containing the run.
	Covering []*audit.Anchor `json:"covering_anchors,omitempty"`
	// AnchorProblems describes each anchor the chain no longer satisfies. A chain can hash-verify
	// perfectly and still have been rewritten wholesale or lost its tail; this is what catches it.
	AnchorProblems []string `json:"anchor_problems,omitempty"`
	// Launch is the chain entry that recorded the request which created this run, resolved by
	// redeeming the run's receipt against the chain. Nil when the run carries no receipt, as a
	// seeded or pre-upgrade run does, or when the chain no longer holds it.
	Launch *audit.Entry `json:"launch,omitempty"`
	// ReceiptMissing is true when the run carries a receipt the chain does not hold, which is what
	// a dropped or rewritten creation entry looks like.
	ReceiptMissing bool `json:"receipt_missing,omitempty"`
	// RecordedBy is the highest chain position reached by the run's own time, the position an
	// anchor must reach to fix history that already held the run.
	RecordedBy int64 `json:"recorded_by,omitempty"`
	// GeneratedAt is when the dossier was collected.
	GeneratedAt time.Time `json:"generated_at"`
}

// Collect gathers a run's evidence from the stores in one streaming pass over the chain. It
// returns run.ErrNotFound when the run does not exist. installID is the install the tree
// profile's leaves bind to, which checking a tree anchor requires.
func Collect(ctx context.Context, runs run.Store, audits audit.Store, installID, id string,
	now time.Time) (*Input, error) {
	r, err := runs.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	// The risk grade an approver saw belongs in the evidence.
	risk := run.AssessRisk(r)
	r.Risk = &risk
	in := &Input{Run: r, GeneratedAt: now}

	// Host outcomes derive from the recorded events, the same source the run page reads. A split
	// or pipeline parent records no events of its own; each child folds separately, because a
	// recap event replaces the fold's stats, so concatenating children would keep only the last
	// child's hosts. The folds then merge by host into the one view the run page shows. A run
	// with no events anywhere, such as one still pending, simply has no host section.
	at := r.CreatedAt
	if r.EndedAt != nil {
		at = *r.EndedAt
	}
	// A store error is never read as absence. Rendering a dossier with no host section over a
	// failed read would assert by omission that nothing ran, which is the one thing an evidence
	// document must not do quietly.
	var groups [][]run.HostSummary
	own, err := hostSummaries(ctx, runs, id, at)
	if err != nil {
		return nil, err
	}
	if len(own) > 0 {
		groups = append(groups, own)
	} else {
		for _, read := range []func(context.Context, string) ([]*run.Run, error){runs.Shards, runs.Steps} {
			children, cerr := read(ctx, id)
			if cerr != nil {
				return nil, fmt.Errorf("read run children: %w", cerr)
			}
			for _, c := range children {
				ce, ceErr := hostSummaries(ctx, runs, c.ID, at)
				if ceErr != nil {
					return nil, ceErr
				}
				if len(ce) > 0 {
					groups = append(groups, ce)
				}
			}
			if len(groups) > 0 {
				break
			}
		}
	}
	in.Hosts = mergeHosts(groups)

	// The anchors are read before the walk so their links are checked against the chain in the
	// same pass. Counting an anchor as covering without holding its link against the entry at
	// that position is how a rewritten chain earns a green banner: the rewrite is internally
	// consistent, so the hash walk passes and only the anchors disagree.
	var anchors []*audit.Anchor
	if store, ok := audits.(audit.AnchorStore); ok {
		var aerr error
		if anchors, aerr = store.Anchors(ctx, 0); aerr != nil {
			return nil, fmt.Errorf("read audit anchors: %w", aerr)
		}
	}

	// One pass gives the run's entries, the whole-chain verdict, the anchor verdicts, the head
	// receipt, and where the chain stood when the run was recorded.
	scan := audit.NewChainScanner(true)
	anchorScan := audit.NewAnchorScanner(anchors, installID)
	// The receipt is parsed once and compared as two scalars, rather than formatting a string for
	// every entry in a chain that can run to millions.
	launchSeq, launchHash := parseReceipt(r.AuditReceipt)
	err = audits.ChainScan(ctx, 0, func(e *audit.Entry) error {
		scan.Feed(e)
		anchorScan.Feed(e)
		in.Head = e
		if strings.Contains(e.Path, id) {
			in.Entries = append(in.Entries, e)
		}
		// The entry that recorded the request creating this run cannot be matched by id, because it
		// was written before the run existed. The run carries its receipt instead, and this is where
		// that receipt is redeemed against the chain being walked.
		if launchSeq > 0 && e.Seq == launchSeq && e.Hash == launchHash {
			in.Launch = e
		}
		// A run's creation is recorded at the request path, which names the template or the
		// collection rather than the run the request went on to create, so it cannot be matched
		// by id. The chain position as of the run's own time is what an anchor has to reach to
		// fix history that already held it.
		if !e.At.After(at) && e.Seq > in.RecordedBy {
			in.RecordedBy = e.Seq
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan audit chain: %w", err)
	}
	in.ChainOK, in.ChainBrokeAt, in.ChainCount = scan.Result()
	// A run holding a receipt the chain cannot produce is the case receipts exist to catch: the
	// entry that recorded its creation is gone or was rewritten.
	in.ReceiptMissing = r.AuditReceipt != "" && in.Launch == nil

	// An anchor covers the run when it still holds against the chain and sits at or after the
	// position the run was recorded by. An anchor that no longer holds is the finding itself.
	cover := in.RecordedBy
	if n := len(in.Entries); n > 0 && in.Entries[n-1].Seq > cover {
		cover = in.Entries[n-1].Seq
	}
	if in.Launch != nil && in.Launch.Seq > cover {
		cover = in.Launch.Seq
	}
	in.Covering, in.AnchorProblems = foldAnchors(anchorScan, cover)
	return in, nil
}

// foldAnchors splits an anchor scan into the anchors that still hold at or above cover and the
// problems the chain no longer satisfies. Both documents fold the same way, so the rule that a
// disowned anchor is always reported cannot hold in one and be forgotten in the other.
//
// A problem is collected whatever cover is. Gating that on coverage put the loudest evidence of a
// wholesale wipe behind the quietest case there is: a run the chain records nothing about, which is
// exactly what a wipe leaves behind. Coverage does need a position to measure from, so a cover of
// zero means nothing is covered, rather than every anchor in the install matching Seq >= 0.
func foldAnchors(scan *audit.AnchorScanner, cover int64) (covering []*audit.Anchor, problems []string) {
	_, results := scan.Results()
	for _, res := range results {
		if !res.Reached {
			problems = append(problems, res.Problem)
			continue
		}
		if cover > 0 && res.Anchor.Seq >= cover {
			covering = append(covering, res.Anchor)
		}
	}
	return covering, problems
}

// parseReceipt splits a seq:link receipt into its parts, returning a zero sequence when it is
// empty or malformed, which reads as "this run names no creation entry".
func parseReceipt(receipt string) (seq int64, link string) {
	colon := strings.IndexByte(receipt, ':')
	if colon <= 0 {
		return 0, ""
	}
	n, err := strconv.ParseInt(receipt[:colon], 10, 64)
	if err != nil || n <= 0 {
		return 0, ""
	}
	return n, receipt[colon+1:]
}

// worstRank orders host outcomes by severity, so merging keeps the more severe one.
var worstRank = map[string]int{"skipped": 0, "ok": 1, "changed": 2, "unreachable": 3, "failed": 4}

// mergeHosts combines per-child host summaries into one view: a host appearing in several children,
// as it does across pipeline steps, sums its tallies and keeps its most severe outcome. Hosts come
// back sorted by name so the table reads the same on every generation.
// eventPage is how many events are folded at a time when a run's per-host outcomes have to be rebuilt
// from its event stream. It matches the window the run export pages by.
// eventPage is a var, not a const, only so a test can shrink the window to force multi-page folding
// without seeding tens of thousands of events. Production never changes it.
var eventPage = 20_000

// hostSummaries returns one run's per-host outcomes.
//
// The stored summaries are the answer whenever there are any: whichever process executed the run folded
// them as its events streamed past and recorded them before the run went terminal, so reading them back
// is both cheaper and closer to what the run actually reported than recomputing from events.
//
// Only a run with none, an old one or one whose summaries never landed, is rebuilt, and then by paging
// the events through a fold rather than holding them all. Reading the whole stream at once is what this
// avoided: a run across thousands of hosts carries hundreds of thousands of events, the codebase measures
// that at hundreds of megabytes, and the export path was already paged for exactly that reason while the
// evidence document still loaded everything. A few concurrent requests could take the control node down,
// and the runs it was recording with it.
func hostSummaries(ctx context.Context, runs run.Store, id string,
	at time.Time) ([]run.HostSummary, error) {
	stored, err := runs.RunHostSummaries(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("read run host summaries: %w", err)
	}
	if len(stored) > 0 {
		return stored, nil
	}
	fold := run.NewSummaryFold(at)
	var after int64
	folded := false
	for {
		page, perr := runs.EventsAfter(ctx, id, after, eventPage)
		if perr != nil {
			return nil, fmt.Errorf("read run events: %w", perr)
		}
		if len(page) == 0 {
			break
		}
		fold.Add(page)
		folded = true
		// Advance by the last event's store sequence, not the page length. The sequence is a global
		// autoincrement, so a run's events are sparse and high-valued; advancing by count would never
		// pass them and would re-read the early pages forever.
		after = page[len(page)-1].Seq
		if len(page) < eventPage {
			break
		}
	}
	if !folded {
		return nil, nil
	}
	return fold.HostSummaries(), nil
}

func mergeHosts(groups [][]run.HostSummary) []run.HostSummary {
	if len(groups) == 0 {
		return nil
	}
	byHost := map[string]*run.HostSummary{}
	for _, group := range groups {
		for _, h := range group {
			got, ok := byHost[h.Host]
			if !ok {
				cp := h
				byHost[h.Host] = &cp
				continue
			}
			got.OK += h.OK
			got.Changed += h.Changed
			got.Failures += h.Failures
			got.Unreachable += h.Unreachable
			got.Skipped += h.Skipped
			got.DurationSeconds += h.DurationSeconds
			if worstRank[h.Worst] > worstRank[got.Worst] {
				got.Worst = h.Worst
			}
		}
	}
	out := make([]run.HostSummary, 0, len(byHost))
	for _, h := range byHost {
		out = append(out, *h)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Host < out[j].Host })
	return out
}

// entryRow is one chain entry as rendered.
type entryRow struct {
	// Seq is the entry's chain position.
	Seq int64
	// At is the entry time, formatted.
	At string
	// Actor is the acting principal.
	Actor string
	// Action is the method and path recorded.
	Action string
	// Role names what the entry did to the run, empty when it is ordinary activity.
	Role string
	// Receipt is the entry's seq:link pair, redeemable against the live chain.
	Receipt string
}

// anchorRow is one covering anchor as rendered.
type anchorRow struct {
	// Type is the anchor kind.
	Type string
	// Seq is the chain position it fixes.
	Seq int64
	// At is when it was taken, formatted.
	At string
	// Ref locates it.
	Ref string
	// Offline is true when the anchor embeds a timestamp token that was read here and found to fix
	// this anchor's link.
	Offline bool
	// ProofProblem says why an embedded token does not fix the link, empty when there is none or when
	// it verified.
	ProofProblem string
}

// hostRow is one host outcome as rendered.
type hostRow struct {
	// Host is the target host.
	Host string
	// Worst is the most severe outcome.
	Worst string
	// OK, Changed, Failures, Unreachable, Skipped are the task tallies.
	OK, Changed, Failures, Unreachable, Skipped int
}

// metaRow is one run attribute as rendered.
type metaRow struct {
	// K is the label.
	K string
	// V is the value.
	V string
}

// view is the data rendered into the dossier.
type view struct {
	// Status is the machine verdict class: verified, unanchored, or broken.
	Status string
	// StatusText is the banner sentence.
	StatusText string
	// RunID is the run identifier.
	RunID string
	// RunStatus is the run's terminal or current status.
	RunStatus string
	// Meta is the run attribute list.
	Meta []metaRow
	// Decisions are the chain entries carrying a role for this run.
	Decisions []entryRow
	// Entries are every chain entry naming the run.
	Entries []entryRow
	// Hosts are the per-host outcomes.
	Hosts []hostRow
	// Anchors are the covering anchors.
	Anchors []anchorRow
	// AnchorProblems describes each anchor the chain no longer satisfies.
	AnchorProblems []string
	// ReceiptMissing is true when the chain no longer holds the run's creation entry.
	ReceiptMissing bool
	// NoReceipt is true when no recorded request created this run, as for a scheduled run or one
	// created before receipts were kept.
	NoReceipt bool
	// ChainCount is the whole chain's entry count.
	ChainCount int
	// Receipt is the chain head's seq:link at collection.
	Receipt string
	// GeneratedAt is when the dossier was made, formatted.
	GeneratedAt string
}

// Render writes the dossier as a self-contained HTML document.
func Render(in *Input) ([]byte, error) {
	if in == nil || in.Run == nil {
		return nil, fmt.Errorf("render dossier: no run")
	}
	v := view{
		RunID:       in.Run.ID,
		RunStatus:   string(in.Run.Status),
		Meta:        runMeta(in.Run),
		ChainCount:  in.ChainCount,
		GeneratedAt: in.GeneratedAt.UTC().Format(time.RFC3339),
	}
	if in.Head != nil {
		v.Receipt = fmt.Sprintf("%d:%s", in.Head.Seq, in.Head.Hash)
	}
	// The launch leads the decisions: it is the record of who asked, and it is redeemable.
	if in.Launch != nil {
		launchRow := entryRow{
			Seq: in.Launch.Seq, At: in.Launch.At.UTC().Format(time.RFC3339), Actor: in.Launch.Actor,
			Action: in.Launch.Method + " " + in.Launch.Path, Role: "Launched",
			Receipt: audit.Receipt(in.Launch),
		}
		v.Decisions = append(v.Decisions, launchRow)
		v.Entries = append(v.Entries, launchRow)
	}
	v.ReceiptMissing = in.ReceiptMissing
	// Silence reads as an omission in an evidence document, so the two reasons a launch row is
	// absent are distinguished: no request authorized this run, or the entry that did is gone.
	if in.Launch == nil && !in.ReceiptMissing {
		v.NoReceipt = true
	}
	for _, e := range in.Entries {
		row := entryRow{
			Seq: e.Seq, At: e.At.UTC().Format(time.RFC3339), Actor: e.Actor,
			Action:  e.Method + " " + e.Path,
			Role:    entryRole(e),
			Receipt: fmt.Sprintf("%d:%s", e.Seq, e.Hash),
		}
		v.Entries = append(v.Entries, row)
		if row.Role != "" {
			v.Decisions = append(v.Decisions, row)
		}
	}
	for _, h := range in.Hosts {
		v.Hosts = append(v.Hosts, hostRow{
			Host: h.Host, Worst: h.Worst,
			OK: h.OK, Changed: h.Changed, Failures: h.Failures,
			Unreachable: h.Unreachable, Skipped: h.Skipped,
		})
	}
	for _, a := range in.Covering {
		// An embedded token is read here rather than described. The row used to say an anchor
		// "verifies offline" on the strength of a proof string being present, which is a claim about
		// the authority's statement made without looking at it.
		row := anchorRow{
			Type: a.Type, Seq: a.Seq, At: a.At.UTC().Format(time.RFC3339),
			Ref: a.Ref, Offline: a.Proof != "",
		}
		if a.Proof != "" {
			if err := audit.VerifyTimestampProof(a.Link, a.Proof); err != nil {
				row.Offline = false
				row.ProofProblem = err.Error()
			}
		}
		v.Anchors = append(v.Anchors, row)
	}
	v.AnchorProblems = in.AnchorProblems
	switch {
	case !in.ChainOK:
		v.Status = "broken"
		v.StatusText = fmt.Sprintf("The audit chain is broken at entry %d. Nothing below is "+
			"trustworthy until the break is explained.", in.ChainBrokeAt)
	case in.ReceiptMissing:
		// The run holds a receipt naming where its creation was recorded and the chain cannot
		// produce that entry. A server that dropped the entry cannot answer the receipt.
		v.Status = "broken"
		v.StatusText = "The chain does not hold the entry that recorded this run's creation, " +
			"though the run carries its receipt. The record of who asked for this run is missing."
	case len(in.AnchorProblems) > 0:
		// The hash walk passing proves only that the chain is self-consistent. A wholesale
		// rewrite is self-consistent too, and this is what disproves it.
		v.Status = "broken"
		v.StatusText = fmt.Sprintf("The chain no longer satisfies %d of its own anchors, so "+
			"history was rewritten or lost. Nothing below is trustworthy until that is explained.",
			len(in.AnchorProblems))
	case len(v.Anchors) == 0:
		v.Status = "unanchored"
		v.StatusText = "The chain verifies, but no anchor covers this run yet, so the record " +
			"rests on this install alone. The next anchor fixes it."
	case in.Launch == nil && len(in.Entries) == 0:
		// The anchors hold, but over a chain that names this run nowhere. Saying they "fix history
		// containing this run" would assert a record that does not exist: what they actually fix is
		// the surrounding history as it stood when the run ran. An evidence document that overstates
		// what an anchor proves is the same defect as one that understates a tamper.
		v.Status = "unanchored"
		v.StatusText = fmt.Sprintf("The chain verifies and %d anchor(s) fix it outside this "+
			"install, but it holds no entry naming this run, so they fix the history around it "+
			"rather than a record of it.", len(v.Anchors))
	default:
		v.Status = "verified"
		v.StatusText = fmt.Sprintf("The chain verifies and %d anchor(s) fix history containing "+
			"this run outside this install.", len(v.Anchors))
	}

	var buf bytes.Buffer
	if err := dossierTemplate.Execute(&buf, v); err != nil {
		return nil, fmt.Errorf("render dossier: %w", err)
	}
	return buf.Bytes(), nil
}

// entryRole names what a chain entry did to the run, empty for ordinary activity.
//
// There is no launch role. A run is created after its request was already recorded, at a path
// naming the template or the collection rather than the run, so no chain entry can be matched to
// it by id. Who asked for the run is carried by the run actor and source instead, and RecordedBy
// is the position an anchor has to reach to cover the creation.
func entryRole(e *audit.Entry) string {
	switch {
	case strings.HasSuffix(e.Path, "/approve"):
		return "Approved"
	case strings.HasSuffix(e.Path, "/reject"):
		return "Rejected"
	case strings.HasSuffix(e.Path, "/cancel"):
		return "Canceled"
	case strings.HasSuffix(e.Path, "/retry"):
		return "Retried"
	case e.Method == audit.MethodRun && strings.Contains(e.Path, "/outcome/"):
		// The outcome is the run's committed result, what it did rather than what was asked. It
		// carries a content digest over the run's evidence, so it is a decision-grade event: this is
		// the line that turns a dossier from a record of requests into a record of what happened.
		return "Outcome"
	}
	return ""
}

// runMeta flattens the run's attributes into labeled rows, skipping empty ones.
func runMeta(r *run.Run) []metaRow {
	rows := []metaRow{}
	add := func(k, v string) {
		if v != "" {
			rows = append(rows, metaRow{K: k, V: v})
		}
	}
	add("Run", r.ID)
	add("Kind", r.Kind)
	add("Tool", r.Tool)
	add("Playbook", r.Playbook)
	add("Command", r.Command)
	add("Inventory", r.Inventory)
	add("Inventory id", r.InventoryID)
	add("Project", r.ProjectID)
	add("Commit", r.CommitSHA)
	if r.DryRun {
		add("Dry run", "yes, no changes were made")
	}
	if r.ShardCount != nil && *r.ShardCount > 1 {
		add("Shards", fmt.Sprintf("%d", *r.ShardCount))
	}
	add("Queue", r.Queue)
	add("Image", r.Image)
	add("Held by", r.HeldByPolicy)
	// Separation of duties is the control a change-management review asks about first, so the evidence
	// says whether it applied to this run rather than leaving the reader to infer it from the names.
	if r.RequireDistinctApprover {
		add("Approver rule", "a different person than the requester was required")
	}
	// A pinned run could only execute one revision, which is what ties an approved plan to the apply
	// that followed it. The evidence says so rather than leaving a reader to compare two commits and
	// hope the match was enforced.
	if r.PinnedCommit != "" {
		add("Pinned to commit", r.PinnedCommit)
	}
	// What the rules were, not only which one fired. A run nothing stopped has no rule to name, and
	// without this the record could not tell that from an install that had no rules at the time.
	if set := r.PolicySet; set != nil {
		if set.Count == 0 {
			add("Rules in force", "none: no approval rule existed when this run was submitted ("+
				set.Digest[:min(12, len(set.Digest))]+")")
		} else {
			add("Rules in force", fmt.Sprintf("%d (%s): %s", set.Count,
				set.Digest[:min(12, len(set.Digest))], strings.Join(set.Rules, "; ")))
		}
	}
	if r.Risk != nil {
		add("Risk", strings.TrimSpace(r.Risk.Level+" "+strings.Join(r.Risk.Reasons, "; ")))
	}
	add("Actor", r.Actor)
	add("Source", strings.TrimSpace(r.Source+" "+r.SourceID))
	add("Intent", r.Intent)
	if len(r.Labels) > 0 {
		// Sorted for the same reason the host table is: two generations of one dossier must differ
		// only where the facts differ, or a diff-based review reports changes that are not real.
		keys := make([]string, 0, len(r.Labels))
		for k := range r.Labels {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, k+"="+r.Labels[k])
		}
		add("Labels", strings.Join(parts, ", "))
	}
	add("Created", r.CreatedAt.UTC().Format(time.RFC3339))
	if r.StartedAt != nil {
		add("Started", r.StartedAt.UTC().Format(time.RFC3339))
	}
	if r.EndedAt != nil {
		add("Ended", r.EndedAt.UTC().Format(time.RFC3339))
		if r.StartedAt != nil {
			add("Duration", r.EndedAt.Sub(*r.StartedAt).Round(time.Second).String())
		}
	}
	if r.ExitCode != nil {
		add("Exit code", fmt.Sprintf("%d", *r.ExitCode))
	}
	add("Error", r.Error)
	add("Warning", r.Warning)
	return rows
}
