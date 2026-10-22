package cmd

import (
	"context"
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
)

// TestAFreshKeyedInstallCannotBeAdoptedOverARewrite pins the adoption fence on an install keyed from
// its first start, which never has anything to adopt. The gate used to let that start through and
// record nothing, so a writer who rewrote an approved held run, stamps included, and cleared its
// seal got the next start refused with a message sending the operator to adopt, and adopt sealed
// the rewrite over the genuine DECISION entry, after which a keyed server ran it. The first start
// now records an empty adoption. After the same rewrite the start goes ahead, adopt refuses as
// already recorded, and the keyed executor refuses the row as carrying no seal and runs nothing.
//
//nolint:funlen // Test function.
func TestAFreshKeyedInstallCannotBeAdoptedOverARewrite(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := tempDB(t)
	id, err := installIdentity(db)
	if err != nil {
		t.Fatalf("installIdentity() error = %v", err)
	}
	key, err := bindingKeyOf(&id)
	if err != nil {
		t.Fatalf("bindingKeyOf() error = %v", err)
	}
	var executed atomic.Int32
	runner := roundhouse.RunnerFunc(func(context.Context, roundhouse.Spec, io.Writer) (
		roundhouse.Result, error) {
		executed.Add(1)
		return roundhouse.Result{}, nil
	})
	bundle, err := openBundle(db)
	if err != nil {
		t.Fatalf("openBundle() error = %v", err)
	}
	bundle.Audits().(audit.InstallBinder).BindInstall(id.InstallID)
	// The install's first start finds nothing to adopt and records that it adopted nothing.
	if err := adoptionGate(ctx, bundle.Runs(), bundle.Audits(), key, nil, db); err != nil {
		t.Fatalf("adoptionGate() on the new install = %v, want the start allowed", err)
	}
	deps := dispatch.AdoptionDeps{Runs: bundle.Runs(), Audits: bundle.Audits(), Key: key}
	if recorded, err := dispatch.Recorded(ctx, deps); err != nil || !recorded {
		t.Fatalf("Recorded() after the first keyed start = %t, %v, want an adoption record",
			recorded, err)
	}
	keyed := dispatch.New(bundle.Runs(), runner, nil, dispatch.WithNoJanitor(),
		dispatch.WithAudits(bundle.Audits()), dispatch.WithBindingKey(key),
		dispatch.WithClaimGate(func() error { return errors.New("this replica claims nothing") }))
	held, err := keyed.Submit(ctx, "", "", run.WithTool(run.ToolBash), run.WithCommand("deploy"),
		run.WithActor("requester"), run.WithRequireApproval(true))
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	if _, err := keyed.Approve(ctx, held.ID, outcome.Decider{Name: "approver", Type: "human"}); err != nil {
		t.Fatalf("Approve() error = %v", err)
	}
	keyed.Close()
	r, err := bundle.Runs().Get(ctx, held.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	_ = bundle.Close()

	// The writer rewrites the approved row in one statement, stamps included, and clears its seal.
	rewritten := r.Clone()
	rewritten.Command = "deploy --everywhere"
	digest, err := outcome.SpecDigest(rewritten)
	if err != nil {
		t.Fatalf("SpecDigest() error = %v", err)
	}
	bind, err := outcome.SpecBinding(rewritten)
	if err != nil {
		t.Fatalf("SpecBinding() error = %v", err)
	}
	editChain(t, db, fmt.Sprintf("UPDATE runs SET command='deploy --everywhere', "+
		"approved_spec_digest='%s', approved_spec_binding='%s', spec_seal='' WHERE id='%s'",
		digest, bind, held.ID))

	bundle, err = openBundle(db)
	if err != nil {
		t.Fatalf("openBundle() error = %v", err)
	}
	defer func() { _ = bundle.Close() }()
	if err := adoptionGate(ctx, bundle.Runs(), bundle.Audits(), key, nil, db); err != nil {
		t.Fatalf("adoptionGate() after the rewrite = %v, want the start allowed: the record is "+
			"there, so the row is refused when it is reached", err)
	}
	err = adopt(ctx, adoptRequest{DB: db, Yes: true, Out: io.Discard, Prompt: io.Discard,
		In: strings.NewReader("")})
	if !errors.Is(err, binding.ErrDrift) || !strings.Contains(err.Error(), "already recorded") {
		t.Fatalf("adopt() after the rewrite = %v, want it refused as already recorded", err)
	}
	serve := dispatch.New(bundle.Runs(), runner, nil, dispatch.WithNoJanitor(),
		dispatch.WithAudits(bundle.Audits()), dispatch.WithBindingKey(key),
		dispatch.WithClaimInterval(5*time.Millisecond))
	defer serve.Close()
	deadline := time.Now().Add(time.Minute)
	for {
		cur, err := bundle.Runs().Get(ctx, held.ID)
		if err == nil && cur.Status.Terminal() {
			if cur.Status != run.StatusFailed || !strings.Contains(cur.Error, "carries no seal") {
				t.Errorf("the rewrite ended %s (%s), want failed as carrying no seal", cur.Status,
					cur.Error)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the rewritten run never reached a terminal state")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if n := executed.Load(); n != 0 {
		t.Errorf("the rewritten command executed %d times, want 0", n)
	}
}
