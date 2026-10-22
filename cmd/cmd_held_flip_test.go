package cmd

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/binding"
	"github.com/kordloom/switchtender/internal/dispatch"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// heldFlipInstall is a keyed SQLite install whose first start recorded the empty adoption, with a
// runner that counts what it executes.
type heldFlipInstall struct {
	// DB is the database path.
	DB string
	// Key is the binding key.
	Key *binding.Key
	// ID is the producer identity.
	ID audit.Identity
	// Executed counts runner invocations.
	Executed atomic.Int32
}

// runner returns the counting runner.
func (in *heldFlipInstall) runner() roundhouse.Runner {
	return roundhouse.RunnerFunc(func(_ context.Context, _ roundhouse.Spec, _ io.Writer) (
		roundhouse.Result, error) {
		in.Executed.Add(1)
		return roundhouse.Result{}, nil
	})
}

// newHeldFlipInstall builds the install and runs its first keyed start gate.
func newHeldFlipInstall(t *testing.T) *heldFlipInstall {
	t.Helper()
	in := &heldFlipInstall{DB: tempDB(t)}
	id, err := installIdentity(in.DB)
	if err != nil {
		t.Fatalf("installIdentity() error = %v", err)
	}
	in.ID = id
	if in.Key, err = bindingKeyOf(&id); err != nil {
		t.Fatalf("bindingKeyOf() error = %v", err)
	}
	bundle := in.open(t)
	defer func() { _ = bundle.Close() }()
	if err := adoptionGate(context.Background(), bundle.Runs(), bundle.Audits(), in.Key, nil,
		in.DB); err != nil {
		t.Fatalf("adoptionGate() on the new install = %v", err)
	}
	return in
}

// open opens the install's stores with its chain bound to the install.
func (in *heldFlipInstall) open(t *testing.T) storeBundle {
	t.Helper()
	bundle, err := openBundle(in.DB)
	if err != nil {
		t.Fatalf("openBundle() error = %v", err)
	}
	bundle.Audits().(audit.InstallBinder).BindInstall(in.ID.InstallID)
	return bundle
}

// submitHeld submits a run that needs approval through a keyed dispatcher that claims nothing,
// and returns it as stored.
func (in *heldFlipInstall) submitHeld(t *testing.T, command string) *run.Run {
	t.Helper()
	ctx := context.Background()
	bundle := in.open(t)
	defer func() { _ = bundle.Close() }()
	keyed := dispatch.New(bundle.Runs(), in.runner(), nil, dispatch.WithNoJanitor(),
		dispatch.WithAudits(bundle.Audits()), dispatch.WithBindingKey(in.Key),
		dispatch.WithClaimGate(func() error { return errors.New("this replica claims nothing") }))
	held, err := keyed.Submit(ctx, "", "", run.WithTool(run.ToolBash), run.WithCommand(command),
		run.WithActor("requester"), run.WithRequireApproval(true))
	keyed.Close()
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	r, err := bundle.Runs().Get(ctx, held.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	return r
}

// serveUntilTerminal starts a keyed claiming dispatcher and waits for id to end.
func (in *heldFlipInstall) serveUntilTerminal(t *testing.T, id string) *run.Run {
	t.Helper()
	ctx := context.Background()
	bundle := in.open(t)
	defer func() { _ = bundle.Close() }()
	serve := dispatch.New(bundle.Runs(), in.runner(), nil, dispatch.WithNoJanitor(),
		dispatch.WithAudits(bundle.Audits()), dispatch.WithBindingKey(in.Key),
		dispatch.WithClaimInterval(5*time.Millisecond))
	defer serve.Close()
	deadline := time.Now().Add(20 * time.Second)
	for {
		cur, err := bundle.Runs().Get(ctx, id)
		if err == nil && cur.Status.Terminal() {
			return cur
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s never reached a terminal state, last seen %+v (error %v)", id, cur, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestAHeldRunFlippedToPendingDoesNotExecute pins that a run waiting for approval is sealed as
// held, so a database writer who moves its status from pending_approval to pending in one
// statement does not get it executed: the keyed executor that claims it refuses it, and nothing
// runs.
func TestAHeldRunFlippedToPendingDoesNotExecute(t *testing.T) {
	t.Parallel()
	in := newHeldFlipInstall(t)
	held := in.submitHeld(t, "deploy")
	if held.Status != run.StatusPendingApproval || held.HeldByPolicy == "" || held.SpecSeal == "" {
		t.Fatalf("held run status %s, held by %q, seal %q; want pending_approval, held, sealed",
			held.Status, held.HeldByPolicy, held.SpecSeal)
	}
	editChain(t, in.DB, "UPDATE runs SET status='pending' WHERE id='"+held.ID+"'")
	got := in.serveUntilTerminal(t, held.ID)
	if n := in.Executed.Load(); n != 0 || got.Status != run.StatusFailed {
		t.Fatalf("a held run flipped to pending executed %d times and ended %s, want nothing "+
			"executed and the run failed", n, got.Status)
	}
	want := "refused: run " + held.ID + ": its spec or its approval fields changed after it was " +
		"submitted"
	if got.Error != want {
		t.Errorf("the flipped run ended with %q, want %q", got.Error, want)
	}
}
