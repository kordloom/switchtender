package dossier

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/event"
	"github.com/kordloom/switchtender/internal/run"
)

// seedEvidence stores one approved run and the chain entries recording it, returning the stores.
func seedEvidence(t *testing.T) (run.Store, audit.Store, string) {
	t.Helper()
	ctx := context.Background()
	runs := run.NewMemStore()
	audits := audit.NewMemStore()

	id := "run_dossier1"
	started := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	ended := started.Add(90 * time.Second)
	exit := 0
	if err := runs.Save(ctx, &run.Run{
		ID: id, Playbook: "site.yml", Inventory: "hosts.ini", Status: run.StatusSucceeded,
		CreatedAt: started.Add(-time.Minute), StartedAt: &started, EndedAt: &ended, ExitCode: &exit,
		Actor: "deploy-bot", Source: "template", SourceID: "tpl_web",
		Labels: map[string]string{"env": "prod"},
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	for _, e := range []struct {
		Actor, Method, Path string
	}{
		// The real recorded path for a launch. The auth middleware records the request path
		// before the handler runs, so it names the template, never the run it goes on to create.
		{"deploy-bot", "POST", "/v1/templates/tpl_web/launch"},
		{"root", "POST", "/v1/runs/" + id + "/approve"},
		{"root", "POST", "/v1/projects"},
	} {
		if err := audits.Append(ctx, &audit.Entry{
			ID: audit.NewID(), At: started, Actor: e.Actor, Method: e.Method, Path: e.Path,
		}); err != nil {
			t.Fatalf("Append() error = %v", err)
		}
	}
	return runs, audits, id
}

func TestDossierCollectsDecisionsAndReceipts(t *testing.T) {
	t.Parallel()
	runs, audits, id := seedEvidence(t)
	in, err := Collect(context.Background(), runs, audits, "", id, time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	if !in.ChainOK || in.ChainCount != 3 {
		t.Errorf("chain ok=%v count=%d, want intact with 3 entries", in.ChainOK, in.ChainCount)
	}
	if len(in.Entries) != 1 {
		t.Fatalf("run entries = %d, want the approval; a launch is recorded at the request path, "+
			"which never names the run it creates", len(in.Entries))
	}
	if in.Head == nil || in.Head.Seq != 3 {
		t.Errorf("head = %+v, want the chain head at seq 3", in.Head)
	}

	doc, err := Render(in)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	html := string(doc)
	for _, want := range []string{
		id, "Approved", "root", "deploy-bot",
		"2:" + in.Entries[0].Hash, "3:" + in.Head.Hash,
		"no anchor covers this run",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("dossier is missing %q", want)
		}
	}
}

func TestDossierReportsABrokenChain(t *testing.T) {
	t.Parallel()
	runs, audits, id := seedEvidence(t)
	// A rewritten entry breaks verification; the dossier must lead with that, not bury it.
	broken := &brokenChain{Store: audits}
	in, err := Collect(context.Background(), runs, broken, "", id, time.Now())
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	if in.ChainOK {
		t.Fatal("Collect() reported an intact chain over a rewritten entry")
	}
	doc, err := Render(in)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	if !strings.Contains(string(doc), "The audit chain is broken at entry 2") {
		t.Error("dossier does not lead with the chain break")
	}
}

func TestDossierAnchorsCoverTheRun(t *testing.T) {
	t.Parallel()
	runs, audits, id := seedEvidence(t)
	ctx := context.Background()
	chain, err := audits.Chain(ctx)
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	anchorStore := audits.(audit.AnchorStore)
	// One anchor before the run's last entry proves nothing about it; one at the head covers it.
	if err := anchorStore.SaveAnchor(ctx, &audit.Anchor{
		ID: "anc_early", Type: "https", Shape: audit.AnchorShapeLinear, Seq: 1, Link: chain[0].Hash,
		At: time.Now(), Ref: "https://x/1",
	}); err != nil {
		t.Fatalf("SaveAnchor() error = %v", err)
	}
	if err := anchorStore.SaveAnchor(ctx, &audit.Anchor{
		ID: "anc_head", Type: "rfc3161", Shape: audit.AnchorShapeLinear, Seq: 3, Link: chain[2].Hash,
		At: time.Now(), Ref: "https://tsa", Proof: "cHJvb2Y=",
	}); err != nil {
		t.Fatalf("SaveAnchor() error = %v", err)
	}
	in, err := Collect(ctx, runs, audits, "", id, time.Now())
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	if len(in.Covering) != 1 || in.Covering[0].ID != "anc_head" {
		t.Fatalf("covering anchors = %+v, want only the head anchor", in.Covering)
	}
	doc, err := Render(in)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	html := string(doc)
	if !strings.Contains(html, "1 anchor(s) fix history") {
		t.Error("dossier does not report the covering anchor in its banner")
	}
	// The proof on this fixture anchor is a placeholder, not a timestamp token, and the dossier now
	// reads it rather than describing it. Saying "verifies offline" about bytes nobody parsed is the
	// claim that used to be made here, and an auditor acting on it would have been acting on nothing.
	if !strings.Contains(html, "timestamp token REFUSED") {
		t.Errorf("dossier does not report that the anchor's proof failed to verify:\n%s", html)
	}
	if strings.Contains(html, "it fixes this link") {
		t.Error("dossier claims a placeholder proof fixes the link")
	}
}

func TestDossierMissingRun(t *testing.T) {
	t.Parallel()
	runs, audits, _ := seedEvidence(t)
	_, err := Collect(context.Background(), runs, audits, "", "run_ghost", time.Now())
	if !errors.Is(err, run.ErrNotFound) {
		t.Errorf("Collect(ghost) error = %v, want run.ErrNotFound", err)
	}
}

// brokenChain rewrites the second entry's path mid-scan, so its stored hash no longer matches its
// content, the way a tampered database reads.
type brokenChain struct {
	audit.Store
}

// ChainScan streams the wrapped chain with entry two rewritten.
func (b *brokenChain) ChainScan(ctx context.Context, afterSeq int64, fn func(*audit.Entry) error) error {
	return b.Store.ChainScan(ctx, afterSeq, func(e *audit.Entry) error {
		if e.Seq == 2 {
			e.Path = "/v1/rewritten"
		}
		return fn(e)
	})
}

func TestDossierRefusesARewrittenChainItsAnchorsDisown(t *testing.T) {
	t.Parallel()
	runs, audits, id := seedEvidence(t)
	ctx := context.Background()
	chain, err := audits.Chain(ctx)
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	// An anchor recorded over the history as it was. The chain then comes back internally
	// consistent but with different links, which is what a wholesale rewrite looks like: the hash
	// walk passes and only the anchor disagrees.
	anchorStore := audits.(audit.AnchorStore)
	if err := anchorStore.SaveAnchor(ctx, &audit.Anchor{
		ID: "anc_1", Type: "rfc3161", Shape: audit.AnchorShapeLinear, Seq: 3,
		Link: "the-link-that-was-anchored",
		At:   time.Now(), Ref: "https://tsa", Proof: "cHJvb2Y=",
	}); err != nil {
		t.Fatalf("SaveAnchor() error = %v", err)
	}
	_ = chain

	in, err := Collect(ctx, runs, audits, "", id, time.Now())
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	if !in.ChainOK {
		t.Fatal("the rewritten chain must still pass the hash walk, or this proves nothing")
	}
	if len(in.AnchorProblems) != 1 {
		t.Fatalf("anchor problems = %v, want the one anchor the chain no longer satisfies", in.AnchorProblems)
	}
	if len(in.Covering) != 0 {
		t.Errorf("covering anchors = %v, want none: an anchor that does not hold covers nothing", in.Covering)
	}
	doc, err := Render(in)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	html := string(doc)
	if strings.Contains(html, "The chain verifies and") {
		t.Error("the dossier calls a chain its own anchors disown verified")
	}
	if !strings.Contains(html, "history was rewritten or lost") {
		t.Error("the dossier does not lead with the anchor disagreement")
	}
}

func TestDossierCoversARunWithNoEntriesNamingIt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	runs := run.NewMemStore()
	audits := audit.NewMemStore()
	base := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	ended := base.Add(time.Minute)
	// An ordinary run that needed no approval: its creation is recorded at the collection path,
	// which never carries the run id, so nothing in the chain names it.
	if err := runs.Save(ctx, &run.Run{
		ID: "run_plain", Playbook: "site.yml", Status: run.StatusSucceeded,
		CreatedAt: base, EndedAt: &ended,
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if err := audits.Append(ctx, &audit.Entry{
		ID: audit.NewID(), At: base, Actor: "deploy-bot", Method: "POST", Path: "/v1/runs",
	}); err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	chain, err := audits.Chain(ctx)
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	if err := audits.(audit.AnchorStore).SaveAnchor(ctx, &audit.Anchor{
		ID: "anc_1", Type: "rfc3161", Shape: audit.AnchorShapeLinear, Seq: 1, Link: chain[0].Hash,
		At: ended, Ref: "https://tsa", Proof: "cHJvb2Y=",
	}); err != nil {
		t.Fatalf("SaveAnchor() error = %v", err)
	}

	in, err := Collect(ctx, runs, audits, "", "run_plain", time.Now())
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	if len(in.Entries) != 0 {
		t.Fatalf("entries naming the run = %d, want none, or this proves nothing", len(in.Entries))
	}
	if len(in.Covering) != 1 {
		t.Fatalf("covering anchors = %d, want the anchor over the position the run was recorded by", len(in.Covering))
	}
	doc, err := Render(in)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	if strings.Contains(string(doc), "no anchor covers this run") {
		t.Error("an anchored install's ordinary run is reported as unanchored")
	}
}

func TestDossierFailsLoudlyWhenAStoreRead2Fails(t *testing.T) {
	t.Parallel()
	runs, audits, id := seedEvidence(t)
	_, err := Collect(context.Background(), &eventsFail{Store: runs}, audits, "", id, time.Now())
	if err == nil {
		t.Fatal("Collect() rendered evidence over a failed event read, asserting by omission that nothing ran")
	}
}

// eventsFail is a run store whose event reads fail.
type eventsFail struct {
	run.Store
}

// Events always fails.
func (e *eventsFail) Events(context.Context, string) ([]event.Event, error) {
	return nil, errors.New("event store is unavailable")
}

func TestDossierRedeemsTheRunReceiptForWhoAsked(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	runs := run.NewMemStore()
	audits := audit.NewMemStore()
	base := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)

	// The launch is recorded before the run exists, at a path naming the template. The receipt is
	// what ties the two together.
	launch := &audit.Entry{
		ID: audit.NewID(), At: base, Actor: "deploy-bot", Method: "POST",
		Path: "/v1/templates/tpl_web/launch",
	}
	if err := audits.Append(ctx, launch); err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	ended := base.Add(time.Minute)
	if err := runs.Save(ctx, &run.Run{
		ID: "run_receipted", Playbook: "site.yml", Status: run.StatusSucceeded,
		CreatedAt: base, EndedAt: &ended, Actor: "deploy-bot",
		AuditReceipt: fmt.Sprintf("%d:%s", launch.Seq, launch.Hash),
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	in, err := Collect(ctx, runs, audits, "", "run_receipted", time.Now())
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	if in.Launch == nil {
		t.Fatal("the run's receipt did not resolve to the entry that recorded its creation")
	}
	if in.Launch.Actor != "deploy-bot" || in.ReceiptMissing {
		t.Errorf("launch = %+v, missing = %v, want the recorded launch", in.Launch, in.ReceiptMissing)
	}
	doc, err := Render(in)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	html := string(doc)
	if !strings.Contains(html, "Launched") {
		t.Error("the dossier does not show who asked for the run")
	}
	if !strings.Contains(html, fmt.Sprintf("%d:%s", launch.Seq, launch.Hash)) {
		t.Error("the launch row carries no redeemable receipt")
	}
}

func TestDossierReportsACreationEntryTheChainNoLongerHolds(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	runs := run.NewMemStore()
	audits := audit.NewMemStore()
	base := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	if err := audits.Append(ctx, &audit.Entry{
		ID: audit.NewID(), At: base, Actor: "root", Method: "POST", Path: "/v1/projects",
	}); err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	// The run holds a receipt for an entry the chain cannot produce, which is exactly what a
	// dropped creation entry looks like to the party holding the receipt.
	if err := runs.Save(ctx, &run.Run{
		ID: "run_orphan", Playbook: "site.yml", Status: run.StatusSucceeded, CreatedAt: base,
		AuditReceipt: "99:deadbeef",
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	in, err := Collect(ctx, runs, audits, "", "run_orphan", time.Now())
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	if !in.ReceiptMissing {
		t.Fatal("a receipt the chain cannot answer was not reported")
	}
	doc, err := Render(in)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	if !strings.Contains(string(doc), "record of who asked for this run is missing") {
		t.Error("the dossier does not lead with the missing creation entry")
	}
}

func TestDossierReportsAWipedChainForARunItRecordsNothingAbout(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	runs := run.NewMemStore()
	audits := audit.NewMemStore()
	base := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	ended := base.Add(time.Minute)
	// A run with no receipt and nothing in the chain naming it, which is what a scheduled run
	// looks like, and also what a wiped chain leaves behind.
	if err := runs.Save(ctx, &run.Run{
		ID: "run_scheduled", Playbook: "site.yml", Status: run.StatusSucceeded,
		CreatedAt: base, EndedAt: &ended,
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	// The chain holds only entries written after the run, and an anchor it no longer satisfies.
	if err := audits.Append(ctx, &audit.Entry{
		ID: audit.NewID(), At: ended.Add(time.Hour), Actor: "root", Method: "POST", Path: "/v1/projects",
	}); err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	if err := audits.(audit.AnchorStore).SaveAnchor(ctx, &audit.Anchor{
		ID: "anc_1", Type: "rfc3161", Shape: audit.AnchorShapeLinear, Seq: 1,
		Link: "the-link-that-was-anchored",
		At:   ended, Ref: "https://tsa",
	}); err != nil {
		t.Fatalf("SaveAnchor() error = %v", err)
	}

	in, err := Collect(ctx, runs, audits, "", "run_scheduled", time.Now())
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	if in.RecordedBy != 0 {
		t.Fatalf("RecordedBy = %d, want 0, or this proves nothing", in.RecordedBy)
	}
	if len(in.AnchorProblems) != 1 {
		t.Fatalf("anchor problems = %v, want the disowning anchor reported even with no coverage "+
			"position", in.AnchorProblems)
	}
	if len(in.Covering) != 0 {
		t.Errorf("covering = %v, want none for a run the chain records nothing about", in.Covering)
	}
	doc, err := Render(in)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	if !strings.Contains(string(doc), "history was rewritten or lost") {
		t.Error("a wiped chain reads as merely unanchored, hiding the tamper it exists to show")
	}
}

func TestDossierDoesNotClaimAnchorsCoverARunTheChainNeverNamed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	runs := run.NewMemStore()
	audits := audit.NewMemStore()
	base := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	ended := base.Add(time.Minute)
	// A scheduled run: no receipt, and no chain entry naming it. The chain holds entries from
	// around that time, and an anchor above them that genuinely holds.
	if err := runs.Save(ctx, &run.Run{
		ID: "run_sched", Playbook: "site.yml", Status: run.StatusSucceeded,
		CreatedAt: base, EndedAt: &ended, Source: "schedule", SourceID: "sch_1",
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if err := audits.Append(ctx, &audit.Entry{
		ID: audit.NewID(), At: base, Actor: "root", Method: "POST", Path: "/v1/projects",
	}); err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	chain, err := audits.Chain(ctx)
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	if err := audits.(audit.AnchorStore).SaveAnchor(ctx, &audit.Anchor{
		ID: "anc_1", Type: "rfc3161", Shape: audit.AnchorShapeLinear, Seq: 1, Link: chain[0].Hash,
		At: ended, Ref: "https://tsa",
	}); err != nil {
		t.Fatalf("SaveAnchor() error = %v", err)
	}

	in, err := Collect(ctx, runs, audits, "", "run_sched", time.Now())
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	if len(in.Covering) != 1 || in.Launch != nil || len(in.Entries) != 0 {
		t.Fatalf("setup wrong: covering=%d launch=%v entries=%d", len(in.Covering), in.Launch, len(in.Entries))
	}
	doc, err := Render(in)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	html := string(doc)
	if strings.Contains(html, "fix history containing this run") {
		t.Error("the dossier claims anchors fix history containing a run the chain never named")
	}
	if !strings.Contains(html, "holds no entry naming this run") {
		t.Error("the dossier does not say what the anchors actually fix")
	}
}

func TestDossierCoverageNeedsAPositionToMeasureFrom(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	runs := run.NewMemStore()
	audits := audit.NewMemStore()
	base := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	ended := base.Add(time.Minute)
	// A run that finished before the chain held anything: no receipt, no entry names it, and every
	// chain entry is later than the run, so there is no position to measure coverage from.
	if err := runs.Save(ctx, &run.Run{
		ID: "run_before", Playbook: "site.yml", Status: run.StatusSucceeded,
		CreatedAt: base, EndedAt: &ended,
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if err := audits.Append(ctx, &audit.Entry{
		ID: audit.NewID(), At: ended.Add(time.Hour), Actor: "root", Method: "POST", Path: "/v1/projects",
	}); err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	chain, err := audits.Chain(ctx)
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	// The anchor genuinely holds, so it takes the coverage branch rather than the problem branch.
	if err := audits.(audit.AnchorStore).SaveAnchor(ctx, &audit.Anchor{
		ID: "anc_ok", Type: "rfc3161", Shape: audit.AnchorShapeLinear, Seq: 1, Link: chain[0].Hash,
		At: ended.Add(2 * time.Hour), Ref: "https://tsa",
	}); err != nil {
		t.Fatalf("SaveAnchor() error = %v", err)
	}

	in, err := Collect(ctx, runs, audits, "", "run_before", time.Now())
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	if in.RecordedBy != 0 || len(in.AnchorProblems) != 0 {
		t.Fatalf("setup wrong: RecordedBy=%d problems=%v, this must exercise the coverage branch",
			in.RecordedBy, in.AnchorProblems)
	}
	if len(in.Covering) != 0 {
		t.Fatalf("covering = %d, want none: with no position to measure from, Seq >= 0 would "+
			"otherwise match every anchor in the install", len(in.Covering))
	}
	doc, err := Render(in)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	if strings.Contains(string(doc), "fix history containing this run") {
		t.Error("a run the chain records nothing about is reported as anchored")
	}
}

// TestDossierSurfacesTheCommittedOutcome checks a finished run's committed outcome shows as a
// decision-grade event in the dossier, not a bare chain row. This is what turns the document from a
// record of what was asked into a record of what happened.
func TestDossierSurfacesTheCommittedOutcome(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	runs, audits, id := seedEvidence(t)
	if err := audits.Append(ctx, &audit.Entry{
		ID: audit.NewID(), At: time.Date(2026, 8, 1, 10, 2, 0, 0, time.UTC),
		Actor: "system:dispatcher", ActorType: "system", OnBehalfOf: "deploy-bot",
		Method: audit.MethodRun, Path: "/runs/" + id + "/outcome/succeeded",
		ContentDigest: "sha256s:abc",
	}); err != nil {
		t.Fatalf("Append(outcome) error = %v", err)
	}

	in, err := Collect(ctx, runs, audits, "", id, time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	doc, err := Render(in)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	// The role label only appears when the outcome is recognized as a decision-grade event.
	if !strings.Contains(string(doc), "<td>Outcome</td>") {
		t.Errorf("dossier does not surface the committed outcome as a decision:\n%s", string(doc))
	}
}
