package dossier

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
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
		{"deploy-bot", "POST", "/v1/templates/tpl_web/launch?run=" + id},
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
	in, err := Collect(context.Background(), runs, audits, id, time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	if !in.ChainOK || in.ChainCount != 3 {
		t.Errorf("chain ok=%v count=%d, want intact with 3 entries", in.ChainOK, in.ChainCount)
	}
	if len(in.Entries) != 2 {
		t.Fatalf("run entries = %d, want the launch and the approval", len(in.Entries))
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
		"1:" + in.Entries[0].Hash, "3:" + in.Head.Hash,
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
	in, err := Collect(context.Background(), runs, broken, id, time.Now())
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
		ID: "anc_early", Type: "https", Seq: 1, Link: chain[0].Hash, At: time.Now(), Ref: "https://x/1",
	}); err != nil {
		t.Fatalf("SaveAnchor() error = %v", err)
	}
	if err := anchorStore.SaveAnchor(ctx, &audit.Anchor{
		ID: "anc_head", Type: "rfc3161", Seq: 3, Link: chain[2].Hash, At: time.Now(), Ref: "https://tsa", Proof: "cHJvb2Y=",
	}); err != nil {
		t.Fatalf("SaveAnchor() error = %v", err)
	}
	in, err := Collect(ctx, runs, audits, id, time.Now())
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
	if !strings.Contains(html, "embedded, verifies offline") {
		t.Error("dossier does not mark the rfc3161 anchor's embedded proof")
	}
}

func TestDossierMissingRun(t *testing.T) {
	t.Parallel()
	runs, audits, _ := seedEvidence(t)
	_, err := Collect(context.Background(), runs, audits, "run_ghost", time.Now())
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
func (b *brokenChain) ChainScan(ctx context.Context, fn func(*audit.Entry) error) error {
	return b.Store.ChainScan(ctx, func(e *audit.Entry) error {
		if e.Seq == 2 {
			e.Path = "/v1/rewritten"
		}
		return fn(e)
	})
}
