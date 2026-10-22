package cmd

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/binding"
	"github.com/kordloom/switchtender/internal/dispatch"
	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/sqlutil"
)

// deciderInstall is a keyed SQLite install whose first start recorded the empty adoption, with a
// runner that counts what it executes.
type deciderInstall struct {
	// DB is the database path.
	DB string
	// Key is the binding key.
	Key *binding.Key
	// ID is the producer identity.
	ID audit.Identity
	// Executed counts runner invocations.
	Executed atomic.Int32
}

// newDeciderInstall builds the install and runs its first keyed start gate.
func newDeciderInstall(t *testing.T) *deciderInstall {
	t.Helper()
	in := &deciderInstall{DB: tempDB(t)}
	id, err := installIdentity(in.DB)
	if err != nil {
		t.Fatalf("installIdentity() error = %v", err)
	}
	in.ID = id
	if in.Key, err = bindingKeyOf(&id); err != nil {
		t.Fatalf("bindingKeyOf() error = %v", err)
	}
	in.with(t, func(bundle storeBundle) {
		err := adoptionGate(context.Background(), bundle.Runs(), bundle.Audits(), in.Key, nil, in.DB)
		if err != nil {
			t.Fatalf("adoptionGate() on the new install = %v", err)
		}
	})
	return in
}

// with opens the install's stores bound to its chain, hands them to use, and closes them.
func (in *deciderInstall) with(t *testing.T, use func(bundle storeBundle)) {
	t.Helper()
	bundle, err := openBundle(in.DB)
	if err != nil {
		t.Fatalf("openBundle() error = %v", err)
	}
	defer func() { _ = bundle.Close() }()
	bundle.Audits().(audit.InstallBinder).BindInstall(in.ID.InstallID)
	use(bundle)
}

// runner returns the counting runner.
func (in *deciderInstall) runner() roundhouse.Runner {
	return roundhouse.RunnerFunc(func(context.Context, roundhouse.Spec, io.Writer) (
		roundhouse.Result, error) {
		in.Executed.Add(1)
		return roundhouse.Result{}, nil
	})
}

// decide submits a run that needs approval and approves it as by, through a keyed dispatcher that
// claims nothing, and returns the run as stored.
func (in *deciderInstall) decide(t *testing.T, by outcome.Decider) *run.Run {
	t.Helper()
	ctx := context.Background()
	var got *run.Run
	in.with(t, func(bundle storeBundle) {
		keyed := dispatch.New(bundle.Runs(), in.runner(), nil, dispatch.WithNoJanitor(),
			dispatch.WithAudits(bundle.Audits()), dispatch.WithBindingKey(in.Key),
			dispatch.WithClaimGate(func() error { return errors.New("this replica claims nothing") }))
		defer keyed.Close()
		held, err := keyed.Submit(ctx, "", "", run.WithTool(run.ToolBash), run.WithCommand("deploy"),
			run.WithActor("requester"), run.WithRequireApproval(true))
		if err != nil {
			t.Fatalf("Submit() error = %v", err)
		}
		if _, err := keyed.Approve(ctx, held.ID, by); err != nil {
			t.Fatalf("Approve() error = %v", err)
		}
		if got, err = bundle.Runs().Get(ctx, held.ID); err != nil {
			t.Fatalf("Get() error = %v", err)
		}
	})
	return got
}

// serveUntilTerminal starts a keyed claiming dispatcher and waits for id to end.
func (in *deciderInstall) serveUntilTerminal(t *testing.T, id string) *run.Run {
	t.Helper()
	ctx := context.Background()
	var got *run.Run
	in.with(t, func(bundle storeBundle) {
		serve := dispatch.New(bundle.Runs(), in.runner(), nil, dispatch.WithNoJanitor(),
			dispatch.WithAudits(bundle.Audits()), dispatch.WithBindingKey(in.Key),
			dispatch.WithClaimInterval(5*time.Millisecond))
		defer serve.Close()
		deadline := time.Now().Add(20 * time.Second)
		for {
			cur, err := bundle.Runs().Get(ctx, id)
			if err == nil && cur.Status.Terminal() {
				got = cur
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("run %s never reached a terminal state, last seen %+v", id, cur)
			}
			time.Sleep(5 * time.Millisecond)
		}
	})
	return got
}

// chainBreaksAt returns the sequence at which the install's chain stops linking, zero when every
// link holds.
func (in *deciderInstall) chainBreaksAt(t *testing.T) int {
	t.Helper()
	links := audit.NewChainScanner(true)
	in.with(t, func(bundle storeBundle) {
		err := bundle.Audits().ChainScan(context.Background(), 0, func(e *audit.Entry) error {
			links.Feed(e)
			return nil
		})
		if err != nil {
			t.Fatalf("ChainScan() error = %v", err)
		}
	})
	_, brokeAt, _ := links.Result()
	return brokeAt
}

// relinkChain recomputes every link of db's chain in sequence order with nothing but the public
// hash function and SQL, the way a writer who edited an entry in place hides the edit from a check
// of the links.
func relinkChain(t *testing.T, db string) {
	t.Helper()
	raw, err := sql.Open("sqlite", db)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	defer func() { _ = raw.Close() }()
	rows, err := raw.Query("SELECT id, at, actor, actor_type, on_behalf_of, method, path, " +
		"content_digest, seq, install_id FROM audit_entries ORDER BY seq ASC")
	if err != nil {
		t.Fatalf("Query() error = %v", err)
	}
	var entries []*audit.Entry
	for rows.Next() {
		var e audit.Entry
		var at string
		if err := rows.Scan(&e.ID, &at, &e.Actor, &e.ActorType, &e.OnBehalfOf, &e.Method, &e.Path,
			&e.ContentDigest, &e.Seq, &e.InstallID); err != nil {
			t.Fatalf("Scan() error = %v", err)
		}
		if e.At, err = sqlutil.ParseTime(at); err != nil {
			t.Fatalf("ParseTime() error = %v", err)
		}
		entries = append(entries, &e)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows error = %v", err)
	}
	_ = rows.Close()
	prev := ""
	for _, e := range entries {
		e.PrevHash = prev
		e.Hash = audit.EntryHash(e)
		if _, err := raw.Exec("UPDATE audit_entries SET prev_hash=?, hash=? WHERE id=?",
			e.PrevHash, e.Hash, e.ID); err != nil {
			t.Fatalf("relink error = %v", err)
		}
		prev = e.Hash
	}
}

// TestARewrittenApproverIsRefusedUnderTheSeal pins that a writer who renames the approver on the
// run's DECISION entry and re-links the chain buys nothing. The chain verifies after the re-link,
// since its links are public, but the run's seal covers the entry's actor, so the keyed executor
// refuses the row before it runs. The approver's name holds a percent sign, which the chain stores
// escaped, so the honest twin also pins that the seal is made over the entry as stored.
func TestARewrittenApproverIsRefusedUnderTheSeal(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Rewrite renames the approver on the DECISION entry and re-links the chain.
		Rewrite bool
		// WantStatus is how the run ends.
		WantStatus run.Status
		// WantError is what the run's error must say, empty for none.
		WantError string
		// WantExecuted is how often the runner executed.
		WantExecuted int32
	}{{ // Test 0: The approval left alone runs once.
		WantStatus: run.StatusSucceeded, WantExecuted: 1,
	}, { // Test 1: The approver renamed and the chain re-linked is refused.
		Rewrite: true, WantStatus: run.StatusFailed, WantExecuted: 0,
		WantError: "its decision changed after it was approved",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			in := newDeciderInstall(t)
			approved := in.decide(t, outcome.Decider{Name: "alice%ops", Type: "session"})
			if approved.DecisionID == "" || approved.SpecSeal == "" {
				t.Fatalf("approved run carries decision %q and seal %q, want both",
					approved.DecisionID, approved.SpecSeal)
			}
			if test.Rewrite {
				editChain(t, in.DB, "UPDATE audit_entries SET actor='mallory', actor_type='human' "+
					"WHERE id='"+approved.DecisionID+"'")
				relinkChain(t, in.DB)
			}
			if brokeAt := in.chainBreaksAt(t); brokeAt != 0 {
				t.Fatalf("the chain breaks at %d, want every link to hold", brokeAt)
			}
			got := in.serveUntilTerminal(t, approved.ID)
			if got.Status != test.WantStatus || in.Executed.Load() != test.WantExecuted {
				t.Errorf("run ended %s (%q) having executed %d times, want %s and %d", got.Status,
					got.Error, in.Executed.Load(), test.WantStatus, test.WantExecuted)
			}
			if test.WantError == "" && got.Error != "" {
				t.Errorf("run error = %q, want none", got.Error)
			}
			if !strings.Contains(got.Error, test.WantError) {
				t.Errorf("run error = %q, want it to say %q", got.Error, test.WantError)
			}
		})
	}
}
