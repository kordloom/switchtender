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
type Input struct {
	// Run is the run the dossier is about.
	Run *run.Run
	// Hosts is the run's per-host outcome, derived from its recorded events.
	Hosts []run.HostSummary
	// Entries are the chain entries that recorded this run: its creation, its approval or
	// rejection, its cancellation, in chain order.
	Entries []*audit.Entry
	// ChainOK reports whether the whole chain verified during collection.
	ChainOK bool
	// ChainBrokeAt is the one-based position of the first break when ChainOK is false.
	ChainBrokeAt int
	// ChainCount is how many entries the chain holds.
	ChainCount int
	// Head is the chain head at collection, the receipt the document carries.
	Head *audit.Entry
	// Covering are the anchors taken at or after the run's last chain entry, each one an
	// independent fixation of history containing the run.
	Covering []*audit.Anchor
	// GeneratedAt is when the dossier was collected.
	GeneratedAt time.Time
}

// Collect gathers a run's evidence from the stores in one streaming pass over the chain. It
// returns run.ErrNotFound when the run does not exist.
func Collect(ctx context.Context, runs run.Store, audits audit.Store, id string, now time.Time) (*Input, error) {
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
	var groups [][]run.HostSummary
	if events, eerr := runs.Events(ctx, id); eerr == nil && len(events) > 0 {
		groups = append(groups, run.HostSummariesFromStats(events, at))
	} else {
		for _, read := range []func(context.Context, string) ([]*run.Run, error){runs.Shards, runs.Steps} {
			children, cerr := read(ctx, id)
			if cerr != nil {
				continue
			}
			for _, c := range children {
				if ce, ceErr := runs.Events(ctx, c.ID); ceErr == nil && len(ce) > 0 {
					groups = append(groups, run.HostSummariesFromStats(ce, at))
				}
			}
			if len(groups) > 0 {
				break
			}
		}
	}
	in.Hosts = mergeHosts(groups)

	// One pass gives the run's entries, the whole-chain verdict, and the head receipt together.
	scan := audit.NewChainScanner(true)
	err = audits.ChainScan(ctx, func(e *audit.Entry) error {
		scan.Feed(e)
		in.Head = e
		if strings.Contains(e.Path, id) {
			in.Entries = append(in.Entries, e)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan audit chain: %w", err)
	}
	in.ChainOK, in.ChainBrokeAt, in.ChainCount = scan.Result()

	// An anchor at or after the run's last entry fixes history containing the run somewhere this
	// install cannot rewrite. Earlier anchors prove nothing about it and are left out.
	if store, ok := audits.(audit.AnchorStore); ok && len(in.Entries) > 0 {
		last := in.Entries[len(in.Entries)-1].Seq
		anchors, aerr := store.Anchors(ctx, 0)
		if aerr != nil {
			return nil, fmt.Errorf("read audit anchors: %w", aerr)
		}
		for _, a := range anchors {
			if a.Seq >= last {
				in.Covering = append(in.Covering, a)
			}
		}
	}
	return in, nil
}

// worstRank orders host outcomes by severity, so merging keeps the more severe one.
var worstRank = map[string]int{"skipped": 0, "ok": 1, "changed": 2, "unreachable": 3, "failed": 4}

// mergeHosts combines per-child host summaries into one view: a host appearing in several children,
// as it does across pipeline steps, sums its tallies and keeps its most severe outcome. Hosts come
// back sorted by name so the table reads the same on every generation.
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
	// Offline is true when the anchor embeds its own proof.
	Offline bool
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
		v.Anchors = append(v.Anchors, anchorRow{
			Type: a.Type, Seq: a.Seq, At: a.At.UTC().Format(time.RFC3339),
			Ref: a.Ref, Offline: a.Proof != "",
		})
	}
	switch {
	case !in.ChainOK:
		v.Status = "broken"
		v.StatusText = fmt.Sprintf("The audit chain is broken at entry %d. Nothing below is "+
			"trustworthy until the break is explained.", in.ChainBrokeAt)
	case len(v.Anchors) == 0:
		v.Status = "unanchored"
		v.StatusText = "The chain verifies, but no anchor covers this run yet, so the record " +
			"rests on this install alone. The next anchor fixes it."
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
func entryRole(e *audit.Entry) string {
	switch {
	case strings.HasSuffix(e.Path, "/approve"):
		return "Approved"
	case strings.HasSuffix(e.Path, "/reject"):
		return "Rejected"
	case strings.HasSuffix(e.Path, "/cancel"):
		return "Canceled"
	case strings.HasSuffix(e.Path, "/launch"), e.Method == "POST" && strings.HasSuffix(e.Path, "/runs"):
		return "Launched"
	case strings.HasSuffix(e.Path, "/retry"):
		return "Retried"
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
	if r.Risk != nil {
		add("Risk", strings.TrimSpace(r.Risk.Level+" "+strings.Join(r.Risk.Reasons, "; ")))
	}
	add("Actor", r.Actor)
	add("Source", strings.TrimSpace(r.Source+" "+r.SourceID))
	add("Intent", r.Intent)
	if len(r.Labels) > 0 {
		parts := make([]string, 0, len(r.Labels))
		for k, val := range r.Labels {
			parts = append(parts, k+"="+val)
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
