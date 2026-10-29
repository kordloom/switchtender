// Package dossiertest holds what evidence document tests share across packages: a keyed install
// holding one sealed approval, and audit stores that rewrite a decider or fail a read the way a
// tampered or faulty chain does.
package dossiertest

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/binding"
	"github.com/kordloom/switchtender/internal/run"
)

// SealedInstall is a keyed install holding one run an approver released, sealed to the DECISION
// entry recording the approval the way the dispatcher seals it.
type SealedInstall struct {
	// Runs is the run store.
	Runs run.Store
	// Audits is the chain as appended.
	Audits audit.Store
	// Key is the binding key the run was sealed under.
	Key *binding.Key
	// Run is the approved run as stored.
	Run *run.Run
	// DecisionID is the approval's chain entry.
	DecisionID string
}

// NewSealedInstall stores a run held for approval at at, records alice's approval of it, settles it
// with that decision, and seals it under the install's key.
func NewSealedInstall(t *testing.T, runID string, at time.Time) *SealedInstall {
	t.Helper()
	ctx := context.Background()
	key, err := binding.DeriveKey(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatalf("DeriveKey() error = %v", err)
	}
	in := &SealedInstall{Runs: run.NewMemStore(), Audits: audit.NewMemStore(), Key: key,
		DecisionID: audit.NewID()}
	ended := at.Add(time.Minute)
	if err := in.Runs.Save(ctx, &run.Run{
		ID: runID, Playbook: "site.yml", Inventory: "hosts.ini", Status: run.StatusPendingApproval,
		CreatedAt: at, EndedAt: &ended, Actor: "deploy-bot", ApprovalRequested: true,
		HeldByPolicy: "prod changes",
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if err := in.Audits.Append(ctx, &audit.Entry{ID: in.DecisionID, At: at, Actor: "alice",
		ActorType: "session", Method: audit.MethodDecision,
		Path: "/runs/" + runID + "/decision/approved", ContentDigest: "sha256:" +
			strings.Repeat("ab", 32)}); err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	if ok, err := in.Runs.ClaimDecision(ctx, runID, in.DecisionID, "claim"); err != nil || !ok {
		t.Fatalf("ClaimDecision() = %v, %v", ok, err)
	}
	settle := run.DecisionSettle{Status: run.StatusSucceeded, EndedAt: ended}
	if ok, err := in.Runs.SettleDecision(ctx, runID, in.DecisionID, settle); err != nil || !ok {
		t.Fatalf("SettleDecision() = %v, %v", ok, err)
	}
	r, err := in.Runs.Get(ctx, runID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	entry, err := in.Audits.Entry(ctx, in.DecisionID)
	if err != nil {
		t.Fatalf("Entry() error = %v", err)
	}
	rec, err := binding.RunRecord(r, binding.RootOf(entry))
	if err != nil {
		t.Fatalf("RunRecord() error = %v", err)
	}
	r.SpecSeal = key.Seal(rec)
	if err := in.Runs.Save(ctx, r); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	in.Run = r
	return in
}

// Verifier returns the checks a keyed process holds rows to, over audits.
func (in *SealedInstall) Verifier(audits audit.Store) *binding.Verifier {
	return &binding.Verifier{Runs: in.Runs, Audits: audits, Key: in.Key,
		SpecSeal: func(r *run.Run) string { return r.SpecSeal },
		Entry: func(ctx context.Context, id string) (*audit.Entry, error) {
			e, err := audits.Entry(ctx, id)
			if errors.Is(err, audit.ErrEntryNotFound) {
				return nil, nil
			}
			return e, err
		}}
}

// ActorRewritten is a chain on which a writer renamed the decider of one entry, read by id and
// streamed alike, and linked every entry from it on again so the walk still verifies.
type ActorRewritten struct {
	audit.Store
	// ID is the entry rewritten.
	ID string
	// Actor is the name the writer put on it.
	Actor string
}

// Entry reads an entry by id as the writer left it.
func (c *ActorRewritten) Entry(ctx context.Context, id string) (*audit.Entry, error) {
	e, err := c.Store.Entry(ctx, id)
	if err == nil && e != nil && e.ID == c.ID {
		e.Actor = c.Actor
	}
	return e, err
}

// ChainScan streams the rewritten chain above afterSeq, linked again from the rewrite on.
func (c *ActorRewritten) ChainScan(ctx context.Context, afterSeq int64,
	fn func(*audit.Entry) error) error {
	prev, relinking := "", false
	return c.Store.ChainScan(ctx, 0, func(e *audit.Entry) error {
		cp := *e
		if cp.ID == c.ID {
			cp.Actor = c.Actor
			relinking = true
		}
		if relinking {
			cp.PrevHash = prev
			cp.Hash = audit.EntryHash(&cp)
		}
		prev = cp.Hash
		if cp.Seq <= afterSeq {
			return nil
		}
		return fn(&cp)
	})
}

// FailingAnchors is an audit store whose anchor read fails, which is a store fault rather than an
// install with no anchors.
type FailingAnchors struct {
	audit.Store
}

// Anchors always fails.
func (FailingAnchors) Anchors(context.Context, int64) ([]*audit.Anchor, error) {
	return nil, errors.New("anchor store is unavailable")
}

// SaveAnchor is never called; it exists so this store satisfies audit.AnchorStore and the anchor
// read is actually attempted.
func (FailingAnchors) SaveAnchor(context.Context, *audit.Anchor) error { return nil }

// DeleteAnchor is never called; it exists for the same reason as SaveAnchor.
func (FailingAnchors) DeleteAnchor(context.Context, string) error { return nil }

// FailingScan is an audit store whose chain walk fails partway, the way a truncated or unreadable
// chain reads.
type FailingScan struct {
	audit.Store
}

// ChainScan always fails.
func (FailingScan) ChainScan(context.Context, int64, func(*audit.Entry) error) error {
	return errors.New("chain is unreadable")
}
