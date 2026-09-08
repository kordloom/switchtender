package dossier

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/run"
)

// TestRenderRegisterRefusesNilInput pins that the register is never produced without collected
// evidence. An empty document reads as a period in which nothing changed, which is a claim, not an
// absence of one.
func TestRenderRegisterRefusesNilInput(t *testing.T) {
	t.Parallel()
	doc, err := RenderRegister(nil)
	if err == nil {
		t.Fatalf("RenderRegister(nil) produced %d bytes, want a refusal", len(doc))
	}
	if doc != nil {
		t.Error("RenderRegister(nil) returned a document beside its error")
	}
}

// TestDecisionOfReadsOnlyACommittedDecision pins which chain entries the register credits as
// approvals and rejections.
//
// The HTTP attempt the middleware records looks almost identical to the committed decision, but it
// is written whether or not the decision took. Reading attempts meant a self-approval the separation
// gate refused still printed "Approved by" the requester, naming the wrong person in the one
// document built for an auditor, and a later failed re-approve overwrote the true approver's name.
//
//nolint:funlen // Test function.
func TestDecisionOfReadsOnlyACommittedDecision(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name says what the case proves.
		Name string
		// Entry is the chain entry to read.
		Entry *audit.Entry
		// WantID is the run the entry decided, empty when it decided none.
		WantID string
		// WantVerdict is the verdict, empty when the entry is ordinary activity.
		WantVerdict string
	}{
		{ // Test 0: The committed approval.
			Name:   "committed approval",
			Entry:  &audit.Entry{Method: audit.MethodDecision, Path: "/runs/run_1/decision/approved"},
			WantID: "run_1", WantVerdict: "Approved",
		},
		{ // Test 1: The committed rejection.
			Name:   "committed rejection",
			Entry:  &audit.Entry{Method: audit.MethodDecision, Path: "/runs/run_1/decision/rejected"},
			WantID: "run_1", WantVerdict: "Rejected",
		},
		{ // Test 2: The HTTP attempt is not a decision, whatever its path looks like.
			Name:  "approval attempt",
			Entry: &audit.Entry{Method: "POST", Path: "/runs/run_1/decision/approved"},
		},
		{ // Test 3: Nor is the route the interface calls.
			Name:  "approve route",
			Entry: &audit.Entry{Method: "POST", Path: "/v1/runs/run_1/approve"},
		},
		{ // Test 4: A verdict the register does not know is not read as either one, so an unknown
			// decision cannot be laundered into an approval.
			Name:  "unknown verdict",
			Entry: &audit.Entry{Method: audit.MethodDecision, Path: "/runs/run_1/decision/expired"},
		},
		{ // Test 5: A decision path naming no run decides nothing.
			Name:  "no run id",
			Entry: &audit.Entry{Method: audit.MethodDecision, Path: "/runs//decision/approved"},
		},
		{ // Test 6: A decision method whose path is not a decision path decides nothing.
			Name:  "not a decision path",
			Entry: &audit.Entry{Method: audit.MethodDecision, Path: "/runs/run_1"},
		},
		{ // Test 7: An empty entry decides nothing.
			Name:  "empty entry",
			Entry: &audit.Entry{},
		},
		{ // Test 8: The verdict is matched exactly, so a different case is not the same verdict.
			Name:  "wrong case verdict",
			Entry: &audit.Entry{Method: audit.MethodDecision, Path: "/runs/run_1/decision/Approved"},
		},
		{ // Test 9: A trailing segment past the verdict is not the verdict.
			Name:  "verdict with a suffix",
			Entry: &audit.Entry{Method: audit.MethodDecision, Path: "/runs/run_1/decision/approved/x"},
		},
		{ // Test 10: A run id carrying its own separator still resolves to the whole id, so the
			// decision is credited to the run it was actually made on.
			Name: "id with a slash",
			Entry: &audit.Entry{Method: audit.MethodDecision,
				Path: "/runs/run_1/child/decision/approved"},
			WantID: "run_1/child", WantVerdict: "Approved",
		},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			id, verdict := decisionOf(test.Entry)
			if id != test.WantID || verdict != test.WantVerdict {
				t.Errorf("decisionOf(%s) = (%q, %q), want (%q, %q)",
					test.Name, id, verdict, test.WantID, test.WantVerdict)
			}
		})
	}
}

// TestChangeOfDescribesARunWithoutPublishingItsSecrets pins the Change column, which is the one line
// an auditor reads to learn what a run did.
//
// A bash, python, powershell, or go run keeps its whole script here, and a script holds whatever it
// was written with: an inline password, a connection string, a token on a command line. The register
// is the document built to be mailed outside the company, and it printed the field verbatim in this
// column and copied it into the CSV export beside it. The redaction runs before the truncation, so a
// secret cannot survive as the fragment of a cut line.
//
//nolint:funlen // Test function.
func TestChangeOfDescribesARunWithoutPublishingItsSecrets(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name says what the case proves.
		Name string
		// Run is the change to describe.
		Run *run.Run
		// WantResult is the exact line, when it is short enough to state.
		WantResult string
		// WantAbsent is a secret the line must not carry.
		WantAbsent string
		// WantMaxLen bounds the line, zero when the exact result is stated instead.
		WantMaxLen int
	}{
		{ // Test 0: An Ansible run names its playbook.
			Name: "playbook", Run: &run.Run{Tool: run.ToolAnsible, Playbook: "site.yml"},
			WantResult: "ansible site.yml",
		},
		{ // Test 1: A run with no recorded tool is Ansible, which is what the product defaults to.
			Name: "no tool", Run: &run.Run{Playbook: "site.yml"}, WantResult: "ansible site.yml",
		},
		{ // Test 2: A Terraform run names its working directory.
			Name: "terraform", Run: &run.Run{Tool: run.ToolTerraform, Command: "infra/network"},
			WantResult: "terraform infra/network",
		},
		{ // Test 3: A run with neither is just the tool, rather than a line with a dangling space.
			Name: "nothing to name", Run: &run.Run{Tool: run.ToolBash}, WantResult: "bash",
		},
		{ // Test 4: A run with an entirely empty record still names the default tool.
			Name: "empty run", Run: &run.Run{}, WantResult: "ansible",
		},
		{ // Test 5: A playbook wins over a command, so a script is not shown for a run that names one.
			Name:       "playbook beside a command",
			Run:        &run.Run{Tool: run.ToolAnsible, Playbook: "site.yml", Command: "PASSWORD=hunter2"},
			WantResult: "ansible site.yml", WantAbsent: "hunter2",
		},
		{ // Test 6: An inline secret in a script is replaced before anything else happens to the line.
			Name: "inline secret",
			Run: &run.Run{Tool: run.ToolBash,
				Command: "export DB_PASSWORD=hunter2secret && psql -c 'select 1'"},
			WantAbsent: "hunter2secret", WantMaxLen: 80,
		},
		{ // Test 7: A secret past the truncation point is still redacted, because the redaction runs
			// first. Truncating first would have cut the line before the redactor ever saw it, and a
			// long enough preamble would then have published the secret in the visible remainder.
			Name: "secret past the cut",
			Run: &run.Run{Tool: run.ToolBash,
				Command: strings.Repeat("echo padding; ", 20) + "API_TOKEN=sk-live-9f3a"},
			WantAbsent: "sk-live-9f3a", WantMaxLen: 80,
		},
		{ // Test 8: A long line is cut to the column's width with an ellipsis, so the table stays
			// readable rather than one row running off the page.
			Name:       "long command",
			Run:        &run.Run{Tool: run.ToolBash, Command: strings.Repeat("a", 200)},
			WantResult: "bash " + strings.Repeat("a", 77) + "...",
		},
		{ // Test 9: A command exactly at the width is not cut, so the ellipsis means something.
			Name:       "exactly at the width",
			Run:        &run.Run{Tool: run.ToolBash, Command: strings.Repeat("a", 80)},
			WantResult: "bash " + strings.Repeat("a", 80),
		},
		{ // Test 10: One character past is cut.
			Name:       "one past the width",
			Run:        &run.Run{Tool: run.ToolBash, Command: strings.Repeat("a", 81)},
			WantResult: "bash " + strings.Repeat("a", 77) + "...",
		},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got := changeOf(test.Run)
			if test.WantResult != "" && got != test.WantResult {
				t.Errorf("changeOf(%s) = %q, want %q", test.Name, got, test.WantResult)
			}
			if test.WantAbsent != "" && strings.Contains(got, test.WantAbsent) {
				t.Errorf("changeOf(%s) published %q: %q", test.Name, test.WantAbsent, got)
			}
			if test.WantMaxLen > 0 && len(got) > test.WantMaxLen+len(test.Run.Tool)+1 {
				t.Errorf("changeOf(%s) is %d bytes, above the column's width: %q",
					test.Name, len(got), got)
			}
		})
	}
}

// TestChangeOfKeepsTheLineValidText demonstrates a defect. The 80-character cut is spent in bytes,
// so a script written in any non-ASCII alphabet is cut mid character and the Change column, and the
// CSV export built from it, carries bytes that are not valid UTF-8.
func TestChangeOfKeepsTheLineValidText(t *testing.T) {
	t.Parallel()
	got := changeOf(&run.Run{Tool: run.ToolBash, Command: strings.Repeat("な", 100)})
	if !utf8.ValidString(got) {
		t.Errorf("changeOf produced invalid UTF-8: %q", got)
	}
}

// failingList is a run store whose page read fails, which is a store fault rather than a period in
// which nothing changed.
type failingList struct {
	run.Store
}

// ListPage always fails.
func (failingList) ListPage(context.Context, run.ListFilter, int, int) ([]*run.Run, error) {
	return nil, errors.New("run store is unavailable")
}

// TestCollectRegisterFailsRatherThanReportingAQuietPeriod pins the direction every store fault errs
// in. A register that came back empty over a failed read is the most dangerous artifact this package
// can produce: it reads as a period in which nothing changed, so an auditor sampling it concludes
// there was nothing to sample.
func TestCollectRegisterFailsRatherThanReportingAQuietPeriod(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		// Name says what the case proves.
		Name string
		// Runs wraps the run store with a fault, or leaves it alone.
		Runs func(run.Store) run.Store
		// Audits wraps the audit store with a fault, or leaves it alone.
		Audits func(audit.Store) audit.Store
	}{
		{ // Test 0: A failed run listing is not a period with no changes.
			Name: "list fails",
			Runs: func(s run.Store) run.Store { return failingList{Store: s} },
		},
		{ // Test 1: A failed anchor read is not an unanchored install.
			Name:   "anchor read fails",
			Audits: func(s audit.Store) audit.Store { return failingAnchors{Store: s} },
		},
		{ // Test 2: A failed chain walk is not a chain that verified.
			Name:   "chain walk fails",
			Audits: func(s audit.Store) audit.Store { return failingScan{Store: s} },
		},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			runs, audits, _ := seedRegister(t)
			if test.Runs != nil {
				runs = test.Runs(runs)
			}
			if test.Audits != nil {
				audits = test.Audits(audits)
			}
			in, err := CollectRegister(context.Background(), runs, audits, "",
				base.Add(-time.Hour), base.Add(7*24*time.Hour), base, 0)
			if err == nil {
				t.Fatalf("CollectRegister over %s returned a register, want a refusal", test.Name)
			}
			if in != nil {
				t.Errorf("CollectRegister over %s returned a register beside its error", test.Name)
			}
		})
	}
}

// TestCollectRegisterBoundsEveryQueryItMakes pins that no caller can ask for an unbounded page. The
// register is a single HTML document held whole in memory and handed to a browser, so a period
// nobody bounded is a document nobody can open and a request that can take the process down with it.
func TestCollectRegisterBoundsEveryQueryItMakes(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		// Name says what the case proves.
		Name string
		// Limit is the cap the caller named.
		Limit int
		// WantLimits are the page sizes the store must be asked for.
		WantLimits []int
	}{
		{Name: "zero", Limit: 0, WantLimits: []int{MaxRegisterRuns + 1}},      // Test 0: The default.
		{Name: "negative", Limit: -1, WantLimits: []int{MaxRegisterRuns + 1}}, // Test 1: Also the default.
		{Name: "one", Limit: 1, WantLimits: []int{2}},                         // Test 2: One row plus the
		// probe that tells a period ending on the cap from one running past it.
		{Name: "above the default", Limit: MaxRegisterRuns * 2, // Test 3: A caller may ask for more,
			// since the caller writing an evidence pack knows what it can hold.
			WantLimits: []int{MaxRegisterRuns*2 + 1}},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			runs := seedOffsets(t, base, []time.Duration{0, time.Hour})
			in, err := CollectRegister(context.Background(), runs, audit.NewMemStore(), "",
				base.Add(-time.Hour), base.Add(24*time.Hour), base, test.Limit)
			if err != nil {
				t.Fatalf("CollectRegister() error = %v", err)
			}
			if diff := cmp.Diff(test.WantLimits, runs.Limits(), cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("page sizes asked of the store (-want +got):\n%s", diff)
			}
			// The document says what bound it was read under, so a reader is never left to assume
			// there was none.
			wantLimit := test.Limit
			if wantLimit <= 0 {
				wantLimit = MaxRegisterRuns
			}
			if in.Limit != wantLimit {
				t.Errorf("recorded limit = %d, want %d", in.Limit, wantLimit)
			}
		})
	}
}

// TestRegisterPeriodBoundsAreHalfOpen pins which changes fall inside a period. The bounds are half
// open, From inclusive and To exclusive, so consecutive registers tile the timeline: a change on the
// boundary appears in exactly one of them, never in both and never in neither.
func TestRegisterPeriodBoundsAreHalfOpen(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	runs := run.NewMemStore()
	for i, at := range []time.Time{
		base.Add(-time.Nanosecond), base, base.Add(time.Hour),
		base.Add(24 * time.Hour), base.Add(24*time.Hour + time.Nanosecond),
	} {
		if err := runs.Save(ctx, &run.Run{ID: fmt.Sprintf("run_%d", i), Playbook: "site.yml",
			Status: run.StatusSucceeded, CreatedAt: at}); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
	}
	in, err := CollectRegister(ctx, runs, audit.NewMemStore(), "", base, base.Add(24*time.Hour),
		base.Add(48*time.Hour), 0)
	if err != nil {
		t.Fatalf("CollectRegister() error = %v", err)
	}
	var got []string
	for _, r := range in.Runs {
		got = append(got, r.ID)
	}
	// run_0 is before From, run_3 is exactly at To and so belongs to the next period, and run_4 is
	// past it.
	want := []string{"run_1", "run_2"}
	if diff := cmp.Diff(want, got, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("changes in the period (-want +got):\n%s", diff)
	}
}

// TestRegisterOverAnEmptyPeriodSaysSoWithoutClaimingMore pins the quiet case. A period in which
// nothing changed is a legitimate answer, and the document has to render it without a decision
// tally, a failure tally, or a truncation notice that would imply otherwise.
func TestRegisterOverAnEmptyPeriodSaysSoWithoutClaimingMore(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	in, err := CollectRegister(context.Background(), run.NewMemStore(), audit.NewMemStore(), "",
		base, base.Add(24*time.Hour), base, 0)
	if err != nil {
		t.Fatalf("CollectRegister() error = %v", err)
	}
	if len(in.Runs) != 0 || in.Truncated {
		t.Fatalf("runs = %d, truncated = %v, want an empty untruncated period", len(in.Runs),
			in.Truncated)
	}
	doc, err := RenderRegister(in)
	if err != nil {
		t.Fatalf("RenderRegister() error = %v", err)
	}
	html := string(doc)
	for _, want := range []string{
		`<p class="k">Changes</p><p class="v">0</p>`,
		`<p class="k">Approved</p><p class="v">0</p>`,
		`<p class="k">Rejected</p><p class="v">0</p>`,
		`<p class="k">Failed</p><p class="v">0</p>`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("the empty register is missing %q", want)
		}
	}
	if strings.Contains(html, "This register is truncated") {
		t.Error("an empty register carries a truncation notice")
	}
}

// TestRegisterBannerFollowsTheChainVerdict pins the headline against every state the collected chain
// can be in, and the order the states are decided in. The banner is what a reader takes away, so one
// that says verified over a chain its own anchors disown launders the finding the document exists
// to surface.
func TestRegisterBannerFollowsTheChainVerdict(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name says what the case proves.
		Name string
		// In is the collected register.
		In *RegisterInput
		// WantStatus is the machine verdict class.
		WantStatus string
		// WantText is a phrase the banner must carry.
		WantText string
	}{
		{ // Test 0: A broken hash walk leads, whatever the anchors say.
			Name:       "broken chain",
			In:         &RegisterInput{ChainOK: false, ChainBrokeAt: 7, Anchored: 3},
			WantStatus: "broken", WantText: "broken at entry 7",
		},
		{ // Test 1: A chain its own anchors disown is a rewrite, even though the hash walk passed,
			// because a wholesale rewrite is self-consistent.
			Name: "anchors disown the chain",
			In: &RegisterInput{ChainOK: true, Anchored: 2,
				AnchorProblems: []string{"Anchor 4 no longer holds.", "Anchor 9 no longer holds."}},
			WantStatus: "broken", WantText: "no longer satisfies 2 of its own anchors",
		},
		{ // Test 2: An unanchored chain names the one-command remedy rather than only the problem.
			Name: "unanchored", In: &RegisterInput{ChainOK: true},
			WantStatus: "unanchored", WantText: "switchtender audit anchor",
		},
		{ // Test 3: Anchors holding is the verified case, and the count is the holding anchors.
			Name: "verified", In: &RegisterInput{ChainOK: true, Anchored: 4},
			WantStatus: "verified", WantText: "carries 4 anchor(s)",
		},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			doc, err := RenderRegister(test.In)
			if err != nil {
				t.Fatalf("RenderRegister(%s) error = %v", test.Name, err)
			}
			html := string(doc)
			if !strings.Contains(html, `class="banner `+test.WantStatus+`"`) {
				t.Errorf("%s: banner class is not %q", test.Name, test.WantStatus)
			}
			if !strings.Contains(html, test.WantText) {
				t.Errorf("%s: banner does not carry %q", test.Name, test.WantText)
			}
			if test.WantStatus != "verified" && strings.Contains(html, "The chain verifies and carries") {
				t.Errorf("%s: the register calls a chain it just found fault with verified", test.Name)
			}
		})
	}
}

// TestRegisterCountsOnlyTheDecisionsOverItsOwnRows pins the summary cards. The tallies are what a
// reviewer reads before the table, so a decision counted for a run the period does not carry, or a
// row counted twice, makes the headline disagree with the evidence under it.
func TestRegisterCountsOnlyTheDecisionsOverItsOwnRows(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	in := &RegisterInput{
		From: base, To: base.Add(24 * time.Hour), GeneratedAt: base, ChainOK: true, Anchored: 1,
		Runs: []*run.Run{
			{ID: "run_a", Playbook: "site.yml", Status: run.StatusSucceeded, CreatedAt: base},
			{ID: "run_b", Playbook: "site.yml", Status: run.StatusFailed, CreatedAt: base},
			{ID: "run_c", Playbook: "site.yml", Status: run.StatusFailed, CreatedAt: base},
			{ID: "run_d", Playbook: "site.yml", Status: run.StatusPendingApproval, CreatedAt: base},
		},
		Decisions: map[string]Decision{
			"run_a": {Verdict: "Approved", Actor: "root", At: base, Seq: 4},
			"run_b": {Verdict: "Rejected", Actor: "root", At: base, Seq: 5},
			// A decision over a run outside this period must not be counted here.
			"run_elsewhere": {Verdict: "Approved", Actor: "root", At: base, Seq: 6},
			// Nor may a verdict the register does not recognize land in either tally.
			"run_c": {Verdict: "Withdrawn", Actor: "root", At: base, Seq: 7},
		},
	}
	doc, err := RenderRegister(in)
	if err != nil {
		t.Fatalf("RenderRegister() error = %v", err)
	}
	html := string(doc)
	for _, want := range []string{
		`<p class="k">Changes</p><p class="v">4</p>`,
		`<p class="k">Approved</p><p class="v">1</p>`,
		`<p class="k">Rejected</p><p class="v">1</p>`,
		`<p class="k">Failed</p><p class="v">2</p>`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("register tallies are missing %q", want)
		}
	}
	if strings.Contains(html, "run_elsewhere") {
		t.Error("the register carries a decision over a run outside the period it covers")
	}
	// The unrecognized verdict is still shown on its row, because hiding it would report the run as
	// undecided, but it is not counted as an approval or a rejection.
	if !strings.Contains(html, "Withdrawn by root") {
		t.Error("an unrecognized verdict is dropped from the row rather than shown as it stands")
	}
}

// TestRegisterEscapesEveryValueThatCameFromOutside pins that untrusted text cannot become markup.
// The register is opened in a browser by whoever it was mailed to, and it carries a script the run
// itself supplied, so a change's own fields must not be able to rewrite the evidence a reader sees.
func TestRegisterEscapesEveryValueThatCameFromOutside(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	const payload = `</td></tr><script>alert(1)</script>`
	in := &RegisterInput{
		From: base, To: base.Add(24 * time.Hour), GeneratedAt: base, ChainOK: true, Anchored: 1,
		Runs: []*run.Run{{
			ID: "run_" + payload, Tool: run.ToolBash, Command: payload,
			Status: run.StatusSucceeded, Actor: payload, Source: payload, SourceID: payload,
			HeldByPolicy: payload, CreatedAt: base,
		}},
		Decisions:      map[string]Decision{"run_" + payload: {Verdict: payload, Actor: payload}},
		AnchorProblems: []string{payload},
	}
	doc, err := RenderRegister(in)
	if err != nil {
		t.Fatalf("RenderRegister() error = %v", err)
	}
	html := string(doc)
	if strings.Contains(html, "<script>alert(1)</script>") {
		t.Error("the register renders a change's own field as live markup")
	}
	if !strings.Contains(html, "alert(1)") {
		t.Errorf("the escaped value is gone from the document entirely:\n%s", html)
	}
}

// TestRegisterRowCarriesTheControlsAReviewerNeeds pins the columns of one change. A reviewer reads
// this row to answer who asked, what it did, how wide it reached, what held it, who released it, and
// how it ended, and a column left blank is a question the document does not answer.
func TestRegisterRowCarriesTheControlsAReviewerNeeds(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 7, 1, 12, 30, 0, 0, time.UTC)
	in := &RegisterInput{
		From: base, To: base.Add(24 * time.Hour), GeneratedAt: base, ChainOK: true, Anchored: 1,
		Runs: []*run.Run{{
			ID: "run_1", Tool: run.ToolAnsible, Playbook: "site.yml", Limit: "all",
			Status: run.StatusSucceeded, Actor: "deploy-bot", Source: "template", SourceID: "tpl_web",
			HeldByPolicy: "prod holds", CreatedAt: base,
		}},
		Decisions: map[string]Decision{"run_1": {Verdict: "Approved", Actor: "root", At: base, Seq: 12}},
	}
	doc, err := RenderRegister(in)
	if err != nil {
		t.Fatalf("RenderRegister() error = %v", err)
	}
	html := string(doc)
	for _, want := range []string{
		"2026-07-01 12:30", "run_1", "ansible site.yml", "deploy-bot", "template tpl_web",
		"prod holds", "Approved by root",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("the change row is missing %q", want)
		}
	}
	// A whole-inventory run is graded wide, and the register has to show the grade an approval
	// policy keyed on rather than leaving a reader to recompute it.
	if !strings.Contains(html, "<td>"+run.RiskMedium+"</td>") {
		t.Errorf("the row does not carry the risk grade for a run reaching every host:\n%s", html)
	}

	// A rehearsal is marked on its own row, because a change register that shows a preview and a
	// real change identically overstates what the period actually changed.
	in.Runs[0].DryRun = true
	doc, err = RenderRegister(in)
	if err != nil {
		t.Fatalf("RenderRegister() error = %v", err)
	}
	if !strings.Contains(string(doc), "dry run") {
		t.Error("a no-change run is shown as an ordinary change")
	}
}

// TestRegisterAnchorsAreFoldedTheSameWayADossierFoldsThem pins that a disowned anchor is reported in
// both documents. The two describe the same install to the same reader, so a rule that holds in one
// and is forgotten in the other means an auditor's verdict depends on which export they were sent.
func TestRegisterAnchorsAreFoldedTheSameWayADossierFoldsThem(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	runs, audits, _ := seedRegister(t)
	chain, err := audits.Chain(ctx)
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	anchors := audits.(audit.AnchorStore)
	// One anchor that holds and one the chain no longer satisfies.
	if err := anchors.SaveAnchor(ctx, &audit.Anchor{
		ID: "anc_ok", Type: "rfc3161", Shape: audit.AnchorShapeLinear, Seq: 1, Link: chain[0].Hash,
		At: base, Ref: "https://tsa",
	}); err != nil {
		t.Fatalf("SaveAnchor() error = %v", err)
	}
	if err := anchors.SaveAnchor(ctx, &audit.Anchor{
		ID: "anc_bad", Type: "rfc3161", Shape: audit.AnchorShapeLinear, Seq: 2,
		Link: "the-link-that-was-anchored", At: base, Ref: "https://tsa",
	}); err != nil {
		t.Fatalf("SaveAnchor() error = %v", err)
	}
	in, err := CollectRegister(ctx, runs, audits, "", base.Add(-time.Hour),
		base.Add(7*24*time.Hour), base, 0)
	if err != nil {
		t.Fatalf("CollectRegister() error = %v", err)
	}
	if in.Anchored != 1 {
		t.Errorf("Anchored = %d, want only the anchor that still holds", in.Anchored)
	}
	if len(in.AnchorProblems) != 1 {
		t.Fatalf("anchor problems = %v, want the one the chain no longer satisfies", in.AnchorProblems)
	}
	doc, err := RenderRegister(in)
	if err != nil {
		t.Fatalf("RenderRegister() error = %v", err)
	}
	html := string(doc)
	if strings.Contains(html, "The chain verifies and carries") {
		t.Error("the register calls a chain its own anchors disown verified")
	}
	if !strings.Contains(html, "history was rewritten or lost") {
		t.Error("the register does not lead with the anchor disagreement")
	}
}
