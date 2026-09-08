package dossier

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/event"
	"github.com/kordloom/switchtender/internal/run"
)

// evidenceTime is the fixed instant the edge tests build their fixtures around, so a rendered
// document differs only where the facts differ.
var evidenceTime = time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)

// TestRenderRefusesWithoutARun pins that the renderer never produces a document with no subject. A
// dossier is the record of one run, so an empty one is not a lighter document, it is a document that
// asserts nothing while looking like evidence.
func TestRenderRefusesWithoutARun(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name says what the case proves.
		Name string
		// In is the collected evidence to render.
		In *Input
	}{
		{Name: "nil input", In: nil},                                         // Test 0: Nothing at all.
		{Name: "no run", In: &Input{}},                                       // Test 1: An input with no subject.
		{Name: "chain but no run", In: &Input{ChainOK: true, ChainCount: 9}}, // Test 2: Chain only.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			doc, err := Render(test.In)
			if err == nil {
				t.Fatalf("Render(%s) produced %d bytes, want a refusal", test.Name, len(doc))
			}
			if doc != nil {
				t.Errorf("Render(%s) returned a document beside its error", test.Name)
			}
		})
	}
}

// TestEntryRoleNamesWhatAChainEntryDidToTheRun pins the labeling of decision-grade events.
//
// Only the committed DECISION entry counts as a decision. The HTTP attempt the middleware records
// looks almost identical but is written whether or not the decision took, so labeling attempts
// credited a self-approval the separation gate refused to the requester, and a failed re-approve
// overwrote the true approver's name in the one document built for an auditor.
//
//nolint:funlen // Test function.
func TestEntryRoleNamesWhatAChainEntryDidToTheRun(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name says what the case proves.
		Name string
		// Entry is the chain entry to label.
		Entry *audit.Entry
		// WantRole is the label, empty for ordinary activity.
		WantRole string
	}{
		{ // Test 0: The committed approval.
			Name:     "committed approval",
			Entry:    &audit.Entry{Method: audit.MethodDecision, Path: "/runs/run_1/decision/approved"},
			WantRole: "Approved",
		},
		{ // Test 1: The committed rejection.
			Name:     "committed rejection",
			Entry:    &audit.Entry{Method: audit.MethodDecision, Path: "/runs/run_1/decision/rejected"},
			WantRole: "Rejected",
		},
		{ // Test 2: The HTTP attempt is ordinary activity, whoever pressed the button. This is the
			// case that credited a refused self-approval to the requester.
			Name:  "approval attempt",
			Entry: &audit.Entry{Method: "POST", Path: "/v1/runs/run_1/approve"},
		},
		{ // Test 3: A rejection attempt is ordinary activity too.
			Name:  "rejection attempt",
			Entry: &audit.Entry{Method: "POST", Path: "/v1/runs/run_1/reject"},
		},
		{ // Test 4: A decision entry whose path names no decision is not a decision.
			Name:  "decision method without a decision path",
			Entry: &audit.Entry{Method: audit.MethodDecision, Path: "/runs/run_1"},
		},
		{ // Test 5: A cancellation.
			Name:     "cancel",
			Entry:    &audit.Entry{Method: "POST", Path: "/v1/runs/run_1/cancel"},
			WantRole: "Canceled",
		},
		{ // Test 6: A retry.
			Name:     "retry",
			Entry:    &audit.Entry{Method: "POST", Path: "/v1/runs/run_1/retry"},
			WantRole: "Retried",
		},
		{ // Test 7: The committed outcome, which is what the run did rather than what was asked. It
			// is the line that turns a dossier from a record of requests into a record of what happened.
			Name:     "committed outcome",
			Entry:    &audit.Entry{Method: audit.MethodRun, Path: "/runs/run_1/outcome/succeeded"},
			WantRole: "Outcome",
		},
		{ // Test 8: A failed outcome is labeled the same way.
			Name:     "failed outcome",
			Entry:    &audit.Entry{Method: audit.MethodRun, Path: "/runs/run_1/outcome/failed"},
			WantRole: "Outcome",
		},
		{ // Test 9: A run-method entry that is not an outcome is ordinary activity.
			Name:  "run method without an outcome path",
			Entry: &audit.Entry{Method: audit.MethodRun, Path: "/runs/run_1/started"},
		},
		{ // Test 10: An ordinary read of the run is ordinary activity.
			Name:  "plain read",
			Entry: &audit.Entry{Method: "GET", Path: "/v1/runs/run_1"},
		},
		{ // Test 11: A launch is ordinary activity here. There is no launch role, because a run is
			// created after its request was recorded at a path naming the template, so no entry can be
			// matched to it by id. The run's receipt is what resolves it instead.
			Name:  "launch",
			Entry: &audit.Entry{Method: "POST", Path: "/v1/templates/tpl_web/launch"},
		},
		{ // Test 12: An empty entry is not a decision.
			Name:  "empty entry",
			Entry: &audit.Entry{},
		},
		{ // Test 13: A path that merely mentions cancel without ending in it is not a cancellation,
			// so a run whose id contains the word is not read as canceled.
			Name:  "cancel not at the end",
			Entry: &audit.Entry{Method: "POST", Path: "/v1/runs/run_cancel/logs"},
		},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := entryRole(test.Entry); got != test.WantRole {
				t.Errorf("entryRole(%s) = %q, want %q", test.Name, got, test.WantRole)
			}
		})
	}
}

// TestAnUnrecognizedVerdictIsNotLabeledApproved demonstrates a defect. entryRole treats every
// committed decision that does not end in "/rejected" as an approval, so a verdict the chain records
// under any other name, an expiry or a withdrawal, is presented to an auditor as an approval nobody
// gave. The register's own reader of the same entries fails closed on an unknown verdict and reports
// none, so the two documents describing one run would disagree.
func TestAnUnrecognizedVerdictIsNotLabeledApproved(t *testing.T) {
	t.Parallel()
	entry := &audit.Entry{Method: audit.MethodDecision, Path: "/runs/run_1/decision/expired"}
	if got := entryRole(entry); got == "Approved" {
		t.Errorf("entryRole(expired decision) = %q, which reports an approval nobody gave", got)
	}
	id, verdict := decisionOf(entry)
	if (verdict != "") != (entryRole(entry) != "") {
		t.Errorf("the dossier says %q and the register says %q for run %q, so the two evidence "+
			"documents disagree about the same entry", entryRole(entry), verdict, id)
	}
}

// TestParseReceiptRefusesAMalformedReceipt pins the reading of a run's creation receipt. A receipt
// that cannot be parsed has to read as "this run names no creation entry", because the alternative
// is redeeming it against the wrong chain position and presenting an unrelated entry as the record
// of who asked for the run.
func TestParseReceiptRefusesAMalformedReceipt(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Receipt is the stored seq:link pair.
		Receipt string
		// WantSeq is the chain position, zero when the receipt names none.
		WantSeq int64
		// WantLink is the entry hash, empty when the receipt names none.
		WantLink string
	}{
		{Receipt: "", WantSeq: 0},                             // Test 0: No receipt at all.
		{Receipt: "abcdef", WantSeq: 0},                       // Test 1: No separator.
		{Receipt: ":", WantSeq: 0},                            // Test 2: A separator and nothing else.
		{Receipt: ":abcdef", WantSeq: 0},                      // Test 3: No position.
		{Receipt: "0:abcdef", WantSeq: 0},                     // Test 4: Position zero is not a position.
		{Receipt: "-3:abcdef", WantSeq: 0},                    // Test 5: Nor a negative one.
		{Receipt: "abc:abcdef", WantSeq: 0},                   // Test 6: Nor a non-numeric one.
		{Receipt: " 4:abcdef", WantSeq: 0},                    // Test 7: Nor a padded one.
		{Receipt: "4.0:abcdef", WantSeq: 0},                   // Test 8: Nor a fractional one.
		{Receipt: "99999999999999999999:x", WantSeq: 0},       // Test 9: Nor one past an int64.
		{Receipt: "4:", WantSeq: 4, WantLink: ""},             // Test 10: A position with an empty link parses.
		{Receipt: "4:abcdef", WantSeq: 4, WantLink: "abcdef"}, // Test 11: The ordinary case.
		{Receipt: "4:ab:cd", WantSeq: 4, WantLink: "ab:cd"},   // Test 12: Only the first colon splits.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			seq, link := parseReceipt(test.Receipt)
			if seq != test.WantSeq || link != test.WantLink {
				t.Errorf("parseReceipt(%q) = (%d, %q), want (%d, %q)",
					test.Receipt, seq, link, test.WantSeq, test.WantLink)
			}
		})
	}
}

// TestMergeHostsFoldsChildrenIntoOneViewOfEachHost pins how a split or pipeline run's per-host
// outcomes are combined. A host that appears in several children sums its tallies and keeps its most
// severe outcome, because reporting the last child's verdict would let a failure on the first step
// disappear behind a success on the last.
func TestMergeHostsFoldsChildrenIntoOneViewOfEachHost(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name says what the case proves.
		Name string
		// Groups are the per-child summaries to merge.
		Groups [][]run.HostSummary
		// WantResult is the merged view, sorted by host.
		WantResult []run.HostSummary
	}{
		{ // Test 0: Nothing to merge is no host section, not an empty one.
			Name: "no groups",
		},
		{ // Test 1: An empty group merges to nothing.
			Name: "one empty group", Groups: [][]run.HostSummary{{}},
		},
		{ // Test 2: One group passes through, sorted.
			Name: "one group",
			Groups: [][]run.HostSummary{{
				{Host: "web02", OK: 2, Worst: "ok"},
				{Host: "web01", OK: 1, Worst: "changed", Changed: 1},
			}},
			WantResult: []run.HostSummary{
				{Host: "web01", OK: 1, Changed: 1, Worst: "changed"},
				{Host: "web02", OK: 2, Worst: "ok"},
			},
		},
		{ // Test 3: A host in two children sums every tally and keeps the more severe outcome.
			Name: "same host twice",
			Groups: [][]run.HostSummary{
				{{Host: "web01", OK: 3, Changed: 1, Skipped: 2, DurationSeconds: 1.5, Worst: "changed"}},
				{{Host: "web01", OK: 1, Failures: 2, Unreachable: 1, DurationSeconds: 2.5,
					Worst: "failed"}},
			},
			WantResult: []run.HostSummary{{
				Host: "web01", OK: 4, Changed: 1, Failures: 2, Unreachable: 1, Skipped: 2,
				DurationSeconds: 4, Worst: "failed",
			}},
		},
		{ // Test 4: The severity order holds whichever child came first, so a later success does not
			// overwrite an earlier failure.
			Name: "severe first",
			Groups: [][]run.HostSummary{
				{{Host: "web01", Failures: 1, Worst: "failed"}},
				{{Host: "web01", OK: 1, Worst: "ok"}},
			},
			WantResult: []run.HostSummary{
				{Host: "web01", OK: 1, Failures: 1, Worst: "failed"},
			},
		},
		{ // Test 5: Unreachable outranks changed, which outranks ok, which outranks skipped.
			Name: "full severity order",
			Groups: [][]run.HostSummary{
				{{Host: "h", Worst: "skipped"}}, {{Host: "h", Worst: "ok"}},
				{{Host: "h", Worst: "changed"}}, {{Host: "h", Worst: "unreachable"}},
			},
			WantResult: []run.HostSummary{{Host: "h", Worst: "unreachable"}},
		},
		{ // Test 6: Hosts across children are merged into one sorted table rather than concatenated.
			Name: "different hosts",
			Groups: [][]run.HostSummary{
				{{Host: "web02", OK: 1, Worst: "ok"}},
				{{Host: "web01", OK: 1, Worst: "ok"}},
				{{Host: "db01", OK: 1, Worst: "ok"}},
			},
			WantResult: []run.HostSummary{
				{Host: "db01", OK: 1, Worst: "ok"},
				{Host: "web01", OK: 1, Worst: "ok"},
				{Host: "web02", OK: 1, Worst: "ok"},
			},
		},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got := mergeHosts(test.Groups)
			if diff := cmp.Diff(test.WantResult, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("mergeHosts(%s) (-want +got):\n%s", test.Name, diff)
			}
		})
	}
}

// childRuns is a run store whose parent has the children it is given, so a dossier for a split or a
// pipeline can be collected without standing a dispatcher behind it.
type childRuns struct {
	run.Store
	// shards are the shard children of any parent.
	shards []*run.Run
	// steps are the pipeline step children of any parent.
	steps []*run.Run
	// shardErr is returned instead of shards when set.
	shardErr error
	// stepErr is returned instead of steps when set.
	stepErr error
}

// Shards returns the configured shard children.
func (c *childRuns) Shards(context.Context, string) ([]*run.Run, error) {
	return c.shards, c.shardErr
}

// Steps returns the configured pipeline step children.
func (c *childRuns) Steps(context.Context, string) ([]*run.Run, error) {
	return c.steps, c.stepErr
}

// TestDossierFoldsChildOutcomesWhenTheParentRecordsNone pins the split and pipeline cases. A parent
// records no events of its own, so a dossier that only read the parent would show no host section
// and assert by omission that nothing ran across the whole fleet the split touched.
func TestDossierFoldsChildOutcomesWhenTheParentRecordsNone(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name says what the case proves.
		Name string
		// Pipeline routes the children through Steps rather than Shards.
		Pipeline bool
		// WantHosts are the host names the dossier must report.
		WantHosts []string
	}{
		{Name: "shard children", WantHosts: []string{"web-00", "web-01", "web-02"}},          // Test 0.
		{Name: "pipeline children", Pipeline: true, WantHosts: []string{"web-00", "web-01"}}, // Test 1.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			base := run.NewMemStore()
			ended := evidenceTime.Add(time.Minute)
			if err := base.Save(ctx, &run.Run{
				ID: "run_parent", Kind: "split", Playbook: "site.yml",
				Status: run.StatusSucceeded, CreatedAt: evidenceTime, EndedAt: &ended,
			}); err != nil {
				t.Fatalf("Save() error = %v", err)
			}
			var children []*run.Run
			for i, hosts := range [][]string{{"web-00", "web-01"}, {"web-02"}} {
				if test.Pipeline && i == 1 {
					break
				}
				id := fmt.Sprintf("run_child%d", i)
				seedHostRun(t, base, id, evidenceTime, hosts)
				child, err := base.Get(ctx, id)
				if err != nil {
					t.Fatalf("Get() error = %v", err)
				}
				children = append(children, child)
			}
			store := &childRuns{Store: base}
			if test.Pipeline {
				store.steps = children
			} else {
				store.shards = children
			}

			in, err := Collect(ctx, store, audit.NewMemStore(), "", "run_parent", evidenceTime)
			if err != nil {
				t.Fatalf("Collect() error = %v", err)
			}
			var gotHosts []string
			for _, h := range in.Hosts {
				gotHosts = append(gotHosts, h.Host)
			}
			if diff := cmp.Diff(test.WantHosts, gotHosts, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("hosts in the parent's dossier (-want +got):\n%s", diff)
			}
		})
	}
}

// seedHostRun stores a finished run whose recorded per-host summaries name hosts.
func seedHostRun(t *testing.T, store run.Store, id string, at time.Time, hosts []string) {
	t.Helper()
	ctx := context.Background()
	r := &run.Run{ID: id, Playbook: "site.yml", Status: run.StatusRunning, CreatedAt: at,
		StartedAt: &at}
	if err := store.Save(ctx, r); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	summaries := make([]run.HostSummary, 0, len(hosts))
	events := make([]event.Event, 0, len(hosts))
	for _, host := range hosts {
		summaries = append(summaries, run.HostSummary{Host: host, OK: 1, Worst: "ok", RanAt: at})
		events = append(events, event.Event{Type: event.TypeRunnerOK, Host: host, Task: "converge"})
	}
	if err := store.AppendEvents(ctx, id, events); err != nil {
		t.Fatalf("AppendEvents() error = %v", err)
	}
	// The summaries are written while the run is still running, because the store fences a terminal
	// run so a reclaimed-but-alive worker cannot overwrite a final summary.
	if err := store.SaveHostSummary(ctx, id, summaries); err != nil {
		t.Fatalf("SaveHostSummary() error = %v", err)
	}
	ended := at.Add(time.Minute)
	code := 0
	r.Status, r.EndedAt, r.ExitCode = run.StatusSucceeded, &ended, &code
	if err := store.Save(ctx, r); err != nil {
		t.Fatalf("Save(terminal) error = %v", err)
	}
}

// failingAnchors is an audit store whose anchor read fails, which is a store fault rather than an
// install with no anchors.
type failingAnchors struct {
	audit.Store
}

// Anchors always fails.
func (failingAnchors) Anchors(context.Context, int64) ([]*audit.Anchor, error) {
	return nil, errors.New("anchor store is unavailable")
}

// SaveAnchor is never called; it exists so this store satisfies audit.AnchorStore and the anchor
// read is actually attempted.
func (failingAnchors) SaveAnchor(context.Context, *audit.Anchor) error { return nil }

// DeleteAnchor is never called; it exists for the same reason as SaveAnchor.
func (failingAnchors) DeleteAnchor(context.Context, string) error { return nil }

// failingScan is an audit store whose chain walk fails partway, the way a truncated or unreadable
// chain reads.
type failingScan struct {
	audit.Store
}

// ChainScan always fails.
func (failingScan) ChainScan(context.Context, int64, func(*audit.Entry) error) error {
	return errors.New("chain is unreadable")
}

// TestCollectFailsRatherThanAssertingByOmission pins the direction every store fault errs in.
//
// This is the one property an evidence document cannot get wrong. A read that failed is not a read
// that came back empty: rendering a dossier with no host section, no anchors, or no chain entries
// over a failed read would assert by omission that nothing ran, nothing anchored it, and nothing
// recorded it, which is exactly the picture a tamper produces.
func TestCollectFailsRatherThanAssertingByOmission(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name says what the case proves.
		Name string
		// Runs wraps the seeded run store with a fault, or returns it unchanged.
		Runs func(run.Store) run.Store
		// Audits wraps the seeded audit store with a fault, or returns it unchanged.
		Audits func(audit.Store) audit.Store
	}{
		{ // Test 0: A failed shard read is not a run without shards.
			Name: "shard read fails",
			Runs: func(s run.Store) run.Store {
				return &childRuns{Store: s, shardErr: errors.New("shard read failed")}
			},
		},
		{ // Test 1: A failed pipeline step read is not a run without steps.
			Name: "step read fails",
			Runs: func(s run.Store) run.Store {
				return &childRuns{Store: s, stepErr: errors.New("step read failed")}
			},
		},
		{ // Test 2: A failed anchor read is not an unanchored install, which is a far quieter verdict
			// than the store being unreadable.
			Name:   "anchor read fails",
			Audits: func(s audit.Store) audit.Store { return failingAnchors{Store: s} },
		},
		{ // Test 3: A failed chain walk is not a chain that verified.
			Name:   "chain walk fails",
			Audits: func(s audit.Store) audit.Store { return failingScan{Store: s} },
		},
		{ // Test 4: A failed per-host read is not a run that touched no hosts.
			Name: "host summary read fails",
			Runs: func(s run.Store) run.Store { return &eventsFail{Store: s} },
		},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			runs, audits, id := seedEvidence(t)
			if test.Runs != nil {
				runs = test.Runs(runs)
			}
			if test.Audits != nil {
				audits = test.Audits(audits)
			}
			in, err := Collect(context.Background(), runs, audits, "", id, evidenceTime)
			if err == nil {
				t.Fatalf("Collect over %s returned evidence, want a refusal", test.Name)
			}
			if in != nil {
				t.Errorf("Collect over %s returned an input beside its error", test.Name)
			}
		})
	}
}

// TestDossierRedactsAnInlineSecretInAScript pins that the evidence document does not publish a
// secret the run's own script carried.
//
// A bash, python, powershell, or go run stores its whole script in Command, and a script holds
// whatever it was written with: an inline password, a connection string, a token on a command line.
// The dossier is the document handed to an outside auditor, so rendering that field verbatim
// published whatever the script held to whoever the export was mailed to.
func TestDossierRedactsAnInlineSecretInAScript(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	runs := run.NewMemStore()
	if err := runs.Save(ctx, &run.Run{
		ID: "run_secret", Tool: run.ToolBash, Status: run.StatusSucceeded, CreatedAt: evidenceTime,
		Command: "export DB_PASSWORD=hunter2secret\nAWS_SECRET_ACCESS_KEY=abc123xyz aws s3 ls",
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	in, err := Collect(ctx, runs, audit.NewMemStore(), "", "run_secret", evidenceTime)
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	doc, err := Render(in)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	html := string(doc)
	for _, secret := range []string{"hunter2secret", "abc123xyz"} {
		if strings.Contains(html, secret) {
			t.Errorf("the rendered dossier publishes %q from the run's script", secret)
		}
	}
	if !strings.Contains(html, "[redacted]") {
		t.Errorf("the dossier drops the script instead of marking what was redacted:\n%s", html)
	}
	// The script itself still has to be legible, or an auditor cannot tell what ran.
	if !strings.Contains(html, "aws s3 ls") {
		t.Error("the dossier redacted the whole command rather than the secret values in it")
	}
}

// TestDossierEscapesEveryValueThatCameFromOutside pins that untrusted text cannot become markup. The
// dossier is opened in a browser by whoever the export was mailed to, and an actor name, a label, or
// a script that closed the document's own tags would let a run's own fields rewrite the evidence a
// reader sees.
func TestDossierEscapesEveryValueThatCameFromOutside(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	runs := run.NewMemStore()
	const payload = `<script>alert(1)</script>`
	if err := runs.Save(ctx, &run.Run{
		ID: "run_" + payload, Playbook: payload, Inventory: payload, Status: run.StatusSucceeded,
		CreatedAt: evidenceTime, Actor: payload, Source: payload, Intent: payload,
		Error: payload, Warning: payload,
		Labels: map[string]string{payload: payload},
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	audits := audit.NewMemStore()
	if err := audits.Append(ctx, &audit.Entry{
		ID: audit.NewID(), At: evidenceTime, Actor: payload, Method: "POST",
		Path: "/v1/runs/run_" + payload + "/cancel",
	}); err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	in, err := Collect(ctx, runs, audits, "", "run_"+payload, evidenceTime)
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	doc, err := Render(in)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	html := string(doc)
	if strings.Contains(html, "<script>alert(1)</script>") {
		t.Error("the dossier renders a run's own field as live markup")
	}
	// The value still has to be readable, or escaping it silently dropped the evidence.
	if !strings.Contains(html, "alert(1)") {
		t.Errorf("the escaped value is gone from the document entirely:\n%s", html)
	}
}

// TestDossierIsSelfContained pins that the evidence document renders from a disk. It is mailed to an
// auditor and opened from a file, so anything it fetches is a section of the evidence that silently
// does not render for the person reading it, and any request it makes tells a third party which run
// is being audited and when.
func TestDossierIsSelfContained(t *testing.T) {
	t.Parallel()
	runs, audits, id := seedEvidence(t)
	in, err := Collect(context.Background(), runs, audits, "", id, evidenceTime)
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	doc, err := Render(in)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	html := string(doc)
	external := regexp.MustCompile(
		`(?i)(<link\b|<img\b|<iframe\b|<script[^>]+\bsrc=|@import\b|url\(\s*['"]?https?:)`)
	if found := external.FindString(html); found != "" {
		t.Errorf("the dossier reaches outside the file: %q", found)
	}
	for _, want := range []string{"@media (prefers-color-scheme: dark)", "@media print"} {
		if !strings.Contains(html, want) {
			t.Errorf("the dossier stylesheet is missing %q", want)
		}
	}
	if !utf8.ValidString(html) {
		t.Error("the dossier is not valid UTF-8, so a reader's browser will render mojibake")
	}
}

// TestDossierBannerFollowsTheEvidenceBelowIt pins the headline against every verdict the collected
// evidence can produce, and the order the verdicts are decided in.
//
// The banner is what a reader takes away. A document whose headline says verified over a table
// reporting a refused anchor, or says merely unanchored over a chain its own anchors disown, is
// worse than one with no banner at all: it launders the finding the document exists to surface.
//
//nolint:funlen // Test function.
func TestDossierBannerFollowsTheEvidenceBelowIt(t *testing.T) {
	t.Parallel()
	entry := &audit.Entry{Seq: 4, At: evidenceTime, Actor: "root", Method: "POST",
		Path: "/v1/runs/run_1/cancel", Hash: "abc123"}
	anchor := &audit.Anchor{ID: "anc_1", Type: "rfc3161", Shape: audit.AnchorShapeLinear, Seq: 4,
		Link: "abc123", At: evidenceTime, Ref: "https://tsa"}
	base := func() *Input {
		return &Input{
			Run:         &run.Run{ID: "run_1", Status: run.StatusSucceeded, CreatedAt: evidenceTime},
			ChainOK:     true,
			ChainCount:  4,
			GeneratedAt: evidenceTime,
		}
	}
	tests := []struct {
		// Name says what the case proves.
		Name string
		// Build shapes the collected evidence.
		Build func(*Input)
		// WantStatus is the machine verdict class.
		WantStatus string
		// WantText is a phrase the banner must carry.
		WantText string
		// WantAbsent is a phrase the banner must not carry.
		WantAbsent string
	}{
		{ // Test 0: A broken hash walk leads, whatever else is true.
			Name: "broken chain",
			Build: func(in *Input) {
				in.ChainOK, in.ChainBrokeAt = false, 2
				in.Entries = []*audit.Entry{entry}
				in.Covering = []*audit.Anchor{anchor}
			},
			WantStatus: "broken", WantText: "broken at entry 2",
		},
		{ // Test 1: A receipt the chain cannot answer outranks an anchor that holds. The record of
			// who asked for the run is missing, and no amount of anchoring supplies it.
			Name: "receipt missing",
			Build: func(in *Input) {
				in.ReceiptMissing = true
				in.Covering = []*audit.Anchor{anchor}
			},
			WantStatus: "broken", WantText: "record of who asked for this run is missing",
		},
		{ // Test 2: A chain the anchors disown is a rewrite. The hash walk passing proves only that
			// the chain is self-consistent, which a wholesale rewrite also is.
			Name: "anchor disowns the chain",
			Build: func(in *Input) {
				in.AnchorProblems = []string{"Anchor 4 no longer holds."}
				in.Entries = []*audit.Entry{entry}
			},
			WantStatus: "broken", WantText: "history was rewritten or lost",
		},
		{ // Test 3: No anchor at all is a warning that names the one-command remedy.
			Name:       "unanchored",
			Build:      func(in *Input) { in.Entries = []*audit.Entry{entry} },
			WantStatus: "unanchored", WantText: "switchtender audit anchor",
		},
		{ // Test 4: Anchors over a chain that names the run nowhere fix the history around it rather
			// than a record of it. Claiming otherwise would assert a record that does not exist.
			Name: "anchored but the run is unnamed",
			Build: func(in *Input) {
				in.Covering = []*audit.Anchor{anchor}
			},
			WantStatus: "unanchored", WantText: "holds no entry naming this run",
			WantAbsent: "fix history containing this run",
		},
		{ // Test 5: An anchor whose embedded token does not fix its own link is a finding, not a
			// footnote. A refused token is the one signal somebody who rewrote the anchors table
			// cannot forge, so the headline must not talk over it.
			Name: "refused timestamp token",
			Build: func(in *Input) {
				refused := *anchor
				refused.Proof = "cHJvb2Y="
				in.Covering = []*audit.Anchor{&refused}
				in.Entries = []*audit.Entry{entry}
			},
			WantStatus: "broken", WantText: "does not fix the link recorded beside it",
			WantAbsent: "1 anchor(s) fix history",
		},
		{ // Test 6: Everything holding is the verified case, and it counts only the anchors that do.
			Name: "verified",
			Build: func(in *Input) {
				in.Covering = []*audit.Anchor{anchor}
				in.Entries = []*audit.Entry{entry}
			},
			WantStatus: "verified", WantText: "1 anchor(s) fix history containing this run",
		},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			in := base()
			test.Build(in)
			doc, err := Render(in)
			if err != nil {
				t.Fatalf("Render(%s) error = %v", test.Name, err)
			}
			html := string(doc)
			if !strings.Contains(html, `<div class="banner `+test.WantStatus+`">`) {
				t.Errorf("%s: banner class is not %q:\n%s", test.Name, test.WantStatus,
					bannerOf(html))
			}
			if !strings.Contains(html, test.WantText) {
				t.Errorf("%s: banner does not carry %q:\n%s", test.Name, test.WantText, bannerOf(html))
			}
			if test.WantAbsent != "" && strings.Contains(html, test.WantAbsent) {
				t.Errorf("%s: banner still carries %q, which the evidence below it contradicts",
					test.Name, test.WantAbsent)
			}
		})
	}
}

// bannerOf returns the document's banner element, for a readable failure message.
func bannerOf(html string) string {
	open := strings.Index(html, `<div class="banner`)
	if open < 0 {
		return html
	}
	end := strings.Index(html[open:], "</div>")
	if end < 0 {
		return html[open:]
	}
	return html[open : open+end]
}

// TestRunMetaRecordsTheControlsAChangeReviewAsksAbout pins the attribute list. A change-management
// review asks about separation of duties, which rules were in force, whether the run was pinned to
// one revision, and whether it was a rehearsal, and a document that leaves those out leaves the
// reader to infer them from names and timestamps.
//
//nolint:funlen // Test function.
func TestRunMetaRecordsTheControlsAChangeReviewAsksAbout(t *testing.T) {
	t.Parallel()
	started := evidenceTime
	ended := evidenceTime.Add(90 * time.Second)
	exit := 3
	shards := 4
	r := &run.Run{
		ID: "run_full", Kind: "split", Tool: run.ToolAnsible, Playbook: "site.yml",
		Command: "unused", Inventory: "prod.ini", InventoryID: "inv_1", ProjectID: "prj_1",
		CommitSHA: "cafebabe", DryRun: true, ShardCount: &shards, Queue: "eu-west",
		Image: "registry/exec:1", HeldByPolicy: "prod holds", RequireDistinctApprover: true,
		PinnedCommit: "deadbeef",
		PolicySet: &run.PolicySet{Digest: "0123456789abcdef", Count: 2,
			Rules: []string{"hold prod", "hold high risk"}},
		Risk:  &run.Risk{Level: run.RiskHigh, Reasons: []string{"touches prod"}},
		Actor: "deploy-bot", Source: "template", SourceID: "tpl_web", Intent: "patch",
		Labels:    map[string]string{"zulu": "1", "alpha": "2"},
		CreatedAt: evidenceTime.Add(-time.Minute), StartedAt: &started, EndedAt: &ended,
		ExitCode: &exit, Error: "task failed", Warning: "host skipped",
		Status: run.StatusFailed,
	}
	rows := runMeta(r)
	got := map[string]string{}
	var order []string
	for _, row := range rows {
		got[row.K] = row.V
		order = append(order, row.K)
	}
	tests := []struct {
		// Key is the attribute label.
		Key string
		// WantValue is the rendered value.
		WantValue string
	}{
		{Key: "Run", WantValue: "run_full"},                      // Test 0.
		{Key: "Kind", WantValue: "split"},                        // Test 1.
		{Key: "Dry run", WantValue: "yes, no changes were made"}, // Test 2.
		{Key: "Shards", WantValue: "4"},                          // Test 3.
		{Key: "Approver rule", // Test 4: Separation of duties.
			WantValue: "a different person than the requester was required"},
		{Key: "Pinned to commit", WantValue: "deadbeef"}, // Test 5.
		{Key: "Rules in force", // Test 6: What the rules were.
			WantValue: "2 (0123456789ab): hold prod; hold high risk"},
		{Key: "Risk", WantValue: "high touches prod"},  // Test 7.
		{Key: "Source", WantValue: "template tpl_web"}, // Test 8.
		{Key: "Labels", WantValue: "alpha=2, zulu=1"},  // Test 9: Sorted, so two generations
		// of one dossier differ only where the facts differ.
		{Key: "Duration", WantValue: "1m30s"},       // Test 10.
		{Key: "Exit code", WantValue: "3"},          // Test 11.
		{Key: "Error", WantValue: "task failed"},    // Test 12.
		{Key: "Warning", WantValue: "host skipped"}, // Test 13.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Key), func(t *testing.T) {
			t.Parallel()
			if got[test.Key] != test.WantValue {
				t.Errorf("%s = %q, want %q", test.Key, got[test.Key], test.WantValue)
			}
		})
	}
	// The order is the reading order of the document, so what was run leads and how it ended trails.
	if order[0] != "Run" || order[len(order)-1] != "Warning" {
		t.Errorf("attribute order runs %q to %q, want Run to Warning", order[0], order[len(order)-1])
	}
}

// TestRunMetaLeavesOutWhatWasNeverRecorded pins that an unset attribute is absent rather than
// rendered blank. A row reading "Pinned to commit:" with nothing after it says a control was
// considered and left empty, which is a different claim from the control not applying.
func TestRunMetaLeavesOutWhatWasNeverRecorded(t *testing.T) {
	t.Parallel()
	rows := runMeta(&run.Run{ID: "run_bare", Status: run.StatusPending, CreatedAt: evidenceTime})
	var keys []string
	for _, row := range rows {
		keys = append(keys, row.K)
		if row.V == "" {
			t.Errorf("attribute %q was rendered with no value", row.K)
		}
	}
	want := []string{"Run", "Created"}
	if diff := cmp.Diff(want, keys, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("attributes of a bare run (-want +got):\n%s", diff)
	}
}

// TestRunMetaSaysWhenNoRuleExistedAtAll pins the difference between a run nothing held and an
// install that had no approval rules. Without the distinction the record could not tell a change
// that passed every rule from one submitted when there were no rules to pass.
func TestRunMetaSaysWhenNoRuleExistedAtAll(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name says what the case proves.
		Name string
		// Set is the rule set recorded on the run.
		Set *run.PolicySet
		// WantValue is the rendered value, empty when no row must appear.
		WantValue string
	}{
		{ // Test 0: No recorded set at all leaves the row out.
			Name: "no set recorded",
		},
		{ // Test 1: An empty set says so in words, naming its digest.
			Name:      "no rules existed",
			Set:       &run.PolicySet{Digest: "0123456789abcdef", Count: 0},
			WantValue: "none: no approval rule existed when this run was submitted (0123456789ab)",
		},
		{ // Test 2: A digest shorter than the display length is not sliced past its end.
			Name:      "short digest",
			Set:       &run.PolicySet{Digest: "abc", Count: 0},
			WantValue: "none: no approval rule existed when this run was submitted (abc)",
		},
		{ // Test 3: An empty digest is not sliced either.
			Name:      "empty digest",
			Set:       &run.PolicySet{Count: 0},
			WantValue: "none: no approval rule existed when this run was submitted ()",
		},
		{ // Test 4: A set with rules names how many, its digest, and what they were.
			Name:      "rules in force",
			Set:       &run.PolicySet{Digest: "abcdef", Count: 1, Rules: []string{"hold prod"}},
			WantValue: "1 (abcdef): hold prod",
		},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			rows := runMeta(&run.Run{ID: "run_1", CreatedAt: evidenceTime, PolicySet: test.Set})
			var got string
			for _, row := range rows {
				if row.K == "Rules in force" {
					got = row.V
				}
			}
			if got != test.WantValue {
				t.Errorf("Rules in force = %q, want %q", got, test.WantValue)
			}
		})
	}
}

// TestCollectGradesTheRiskAnApproverSaw pins that the dossier carries the risk grade rather than
// leaving a reader to recompute it. The grade is what the approval policies keyed on, so it is part
// of the record of why the run was held or was not.
func TestCollectGradesTheRiskAnApproverSaw(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	runs := run.NewMemStore()
	if err := runs.Save(ctx, &run.Run{
		ID: "run_wide", Tool: run.ToolAnsible, Playbook: "site.yml", Limit: "all",
		Status: run.StatusSucceeded, CreatedAt: evidenceTime,
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	in, err := Collect(ctx, runs, audit.NewMemStore(), "", "run_wide", evidenceTime)
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	if in.Run.Risk == nil {
		t.Fatal("the dossier carries no risk grade, so the record cannot say what an approver saw")
	}
	doc, err := Render(in)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	if !strings.Contains(string(doc), "targets the whole inventory") {
		t.Errorf("the dossier does not report the blast radius the grade was built from:\n%s",
			string(doc))
	}
}

// TestCollectResolvesTheReceiptOnlyOnAnExactMatch pins the redemption of a run's creation receipt. A
// receipt names both a chain position and the hash at it, and matching on the position alone would
// resolve to whatever entry now sits there, presenting an unrelated request as the record of who
// asked for this run, which is precisely the substitution a receipt exists to detect.
func TestCollectResolvesTheReceiptOnlyOnAnExactMatch(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name says what the case proves.
		Name string
		// Receipt is what the run carries, with %d and %s filled from the real launch entry.
		Receipt string
		// WantLaunch is whether the launch entry must resolve.
		WantLaunch bool
		// WantMissing is whether the run must be reported as holding a receipt the chain cannot answer.
		WantMissing bool
	}{
		{Name: "exact", Receipt: "%d:%s", WantLaunch: true}, // Test 0.
		{Name: "right position wrong hash", Receipt: "%d:deadbeef", // Test 1: The substitution.
			WantMissing: true},
		{Name: "wrong position right hash", Receipt: "99:%s", // Test 2: A moved entry.
			WantMissing: true},
		{Name: "no receipt", Receipt: ""}, // Test 3: A scheduled run names no creation entry.
		{Name: "unparsable receipt", Receipt: "garbage", // Test 4: A run holding a receipt that
			// cannot be read is reported the same way as one the chain cannot answer. That is the
			// fail-closed reading: a corrupt receipt field is evidence of something wrong with the
			// record, not evidence that no request created the run.
			WantMissing: true},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			runs := run.NewMemStore()
			audits := audit.NewMemStore()
			launch := &audit.Entry{ID: audit.NewID(), At: evidenceTime, Actor: "deploy-bot",
				Method: "POST", Path: "/v1/templates/tpl_web/launch"}
			if err := audits.Append(ctx, launch); err != nil {
				t.Fatalf("Append() error = %v", err)
			}
			receipt := test.Receipt
			if strings.Contains(receipt, "%") {
				receipt = fmt.Sprintf(strings.ReplaceAll(test.Receipt, "%s", "%[2]s"),
					launch.Seq, launch.Hash)
			}
			if err := runs.Save(ctx, &run.Run{
				ID: "run_1", Playbook: "site.yml", Status: run.StatusSucceeded,
				CreatedAt: evidenceTime, AuditReceipt: receipt,
			}); err != nil {
				t.Fatalf("Save() error = %v", err)
			}
			in, err := Collect(ctx, runs, audits, "", "run_1", evidenceTime)
			if err != nil {
				t.Fatalf("Collect() error = %v", err)
			}
			if (in.Launch != nil) != test.WantLaunch {
				t.Errorf("launch resolved = %v, want %v (receipt %q)",
					in.Launch != nil, test.WantLaunch, receipt)
			}
			if in.ReceiptMissing != test.WantMissing {
				t.Errorf("ReceiptMissing = %v, want %v (receipt %q)",
					in.ReceiptMissing, test.WantMissing, receipt)
			}
			doc, err := Render(in)
			if err != nil {
				t.Fatalf("Render() error = %v", err)
			}
			// Silence reads as an omission in an evidence document, so the two reasons a launch row
			// is absent have to be told apart in the document itself.
			noReceipt := strings.Contains(string(doc), "No recorded request created this run")
			if want := !test.WantLaunch && !test.WantMissing; noReceipt != want {
				t.Errorf("the no-receipt paragraph = %v, want %v", noReceipt, want)
			}
		})
	}
}

// TestCollectOnAMissingRunReportsNotFound pins that a dossier is never produced for a run that does
// not exist, and that the reason is the store's own not-found rather than a generic failure the
// caller has to guess at.
func TestCollectOnAMissingRunReportsNotFound(t *testing.T) {
	t.Parallel()
	runs, audits, _ := seedEvidence(t)
	for testNum, id := range []string{"run_ghost", "", "   ", "run_dossier1x"} {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			in, err := Collect(context.Background(), runs, audits, "", id, evidenceTime)
			if !errors.Is(err, run.ErrNotFound) {
				t.Errorf("Collect(%q) error = %v, want run.ErrNotFound", id, err)
			}
			if in != nil {
				t.Errorf("Collect(%q) returned evidence for a run that does not exist", id)
			}
		})
	}
}

// summaryFail is a run store whose stored per-host summaries read fails for one run id, so a fault
// on a child can be told from a fault on the parent.
type summaryFail struct {
	run.Store
	// failFor is the run id whose summary read fails.
	failFor string
}

// RunHostSummaries fails for the configured id and delegates otherwise.
func (s *summaryFail) RunHostSummaries(ctx context.Context, id string) ([]run.HostSummary, error) {
	if id == s.failFor {
		return nil, errors.New("summary store is unavailable")
	}
	return s.Store.RunHostSummaries(ctx, id)
}

// eventPageFail is a run store that reports no stored summaries and then fails the paged event read,
// which is the fold path a run with no recorded summaries takes.
type eventPageFail struct {
	run.Store
}

// RunHostSummaries reports none, so the caller falls through to folding the event stream.
func (eventPageFail) RunHostSummaries(context.Context, string) ([]run.HostSummary, error) {
	return nil, nil
}

// EventsAfter always fails.
func (eventPageFail) EventsAfter(context.Context, string, int64, int) ([]event.Event, error) {
	return nil, errors.New("event store is unavailable")
}

// TestCollectFailsOnAChildsOwnReadFault pins the fault path one level down. A split parent records
// no events of its own, so its host section is folded entirely from its children, and a child whose
// outcomes could not be read is a hole in that section rather than a child that touched no hosts.
func TestCollectFailsOnAChildsOwnReadFault(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := run.NewMemStore()
	ended := evidenceTime.Add(time.Minute)
	if err := base.Save(ctx, &run.Run{
		ID: "run_parent", Kind: "split", Playbook: "site.yml", Status: run.StatusSucceeded,
		CreatedAt: evidenceTime, EndedAt: &ended,
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	seedHostRun(t, base, "run_child0", evidenceTime, []string{"web-00"})
	child, err := base.Get(ctx, "run_child0")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	store := &childRuns{Store: &summaryFail{Store: base, failFor: "run_child0"},
		shards: []*run.Run{child}}

	in, cerr := Collect(ctx, store, audit.NewMemStore(), "", "run_parent", evidenceTime)
	if cerr == nil {
		t.Fatalf("Collect over a failed child read returned evidence with %d hosts", len(in.Hosts))
	}
}

// TestCollectFailsWhenTheEventFoldCannotBeRead pins the other half of the per-host read. A run whose
// summaries were never stored is rebuilt by paging its events, and a page that could not be read is
// not a page with nothing in it: rendering the run with no host section would assert by omission
// that it touched nothing.
func TestCollectFailsWhenTheEventFoldCannotBeRead(t *testing.T) {
	t.Parallel()
	runs, audits, id := seedEvidence(t)
	in, err := Collect(context.Background(), eventPageFail{Store: runs}, audits, "", id, evidenceTime)
	if err == nil {
		t.Fatalf("Collect over a failed event page returned evidence with %d hosts", len(in.Hosts))
	}
	if !strings.Contains(err.Error(), "read run events") {
		t.Errorf("error = %v, want it to name the read that failed", err)
	}
}

// TestCoverageReachesTheLaunchEntryEvenWhenItIsTheOnlyThingNamingTheRun pins the coverage position
// an anchor has to reach. A run whose creation was recorded after its own recorded time, which clock
// skew between the API node and the executor produces, has no chain position from its own time, and
// the launch entry the receipt redeems is then the only position there is to measure from. Missing
// it would report an anchored install as unanchored for exactly the runs a receipt does resolve.
func TestCoverageReachesTheLaunchEntryEvenWhenItIsTheOnlyThingNamingTheRun(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	runs := run.NewMemStore()
	audits := audit.NewMemStore()
	// The run finished before the chain held anything, and the entry that recorded its creation
	// carries a later time than the run does.
	ended := evidenceTime.Add(time.Minute)
	launch := &audit.Entry{ID: audit.NewID(), At: evidenceTime.Add(time.Hour), Actor: "deploy-bot",
		Method: "POST", Path: "/v1/templates/tpl_web/launch"}
	if err := audits.Append(ctx, launch); err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	if err := runs.Save(ctx, &run.Run{
		ID: "run_skewed", Playbook: "site.yml", Status: run.StatusSucceeded,
		CreatedAt: evidenceTime, EndedAt: &ended, Actor: "deploy-bot",
		AuditReceipt: fmt.Sprintf("%d:%s", launch.Seq, launch.Hash),
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	chain, err := audits.Chain(ctx)
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	if err := audits.(audit.AnchorStore).SaveAnchor(ctx, &audit.Anchor{
		ID: "anc_1", Type: "rfc3161", Shape: audit.AnchorShapeLinear, Seq: 1, Link: chain[0].Hash,
		At: evidenceTime.Add(2 * time.Hour), Ref: "https://tsa",
	}); err != nil {
		t.Fatalf("SaveAnchor() error = %v", err)
	}

	in, err := Collect(ctx, runs, audits, "", "run_skewed", evidenceTime.Add(3*time.Hour))
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	if in.RecordedBy != 0 || in.Launch == nil {
		t.Fatalf("setup wrong: RecordedBy = %d, launch = %v; the launch must be the only position "+
			"there is to measure coverage from", in.RecordedBy, in.Launch)
	}
	if len(in.Covering) != 1 {
		t.Fatalf("covering anchors = %d, want the anchor over the launch position", len(in.Covering))
	}
	doc, err := Render(in)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	if !strings.Contains(string(doc), "fix history containing this run") {
		t.Errorf("an anchored run resolved by its receipt is reported as unanchored:\n%s",
			bannerOf(string(doc)))
	}
}

// TestDossierRendersTheHostTableItCollected pins that the per-host outcomes reach the document. The
// host table is what turns a dossier from a record of a request into a record of what the change did
// on each machine, so collecting the outcomes and then not rendering them would lose exactly that.
func TestDossierRendersTheHostTableItCollected(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := run.NewMemStore()
	seedHostRun(t, base, "run_hosts", evidenceTime, []string{"web-01", "db-01"})

	in, err := Collect(ctx, base, audit.NewMemStore(), "", "run_hosts", evidenceTime)
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	doc, err := Render(in)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	html := string(doc)
	if !strings.Contains(html, "What happened on each host") {
		t.Fatalf("the dossier has no host section for a run that recorded outcomes:\n%s", html)
	}
	for _, want := range []string{"<td>web-01</td>", "<td>db-01</td>"} {
		if !strings.Contains(html, want) {
			t.Errorf("the host table is missing %q", want)
		}
	}
	// The table is sorted by host, so two generations of one dossier differ only where the facts do.
	if strings.Index(html, "<td>db-01</td>") > strings.Index(html, "<td>web-01</td>") {
		t.Error("the host table is not sorted, so a regenerated dossier diffs against itself")
	}
}
