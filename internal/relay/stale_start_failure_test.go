package relay

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/handoff"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/sqlitestore"
)

// rlyStaleFixture is a control node on a real SQLite store serving the keyed dmz pool, with one
// run waiting in the dmz queue that needs secrets.
type rlyStaleFixture struct {
	// url is the relay's base URL.
	url string
	// store is the control node's run store.
	store run.Store
	// opener records every release the relay runs.
	opener *scriptedOpener
	// ring opens what the control node seals to the pool.
	ring *handoff.KeyRing
}

// rlyNewStaleFixture starts the control node and seeds run_stale.
func rlyNewStaleFixture(t *testing.T) *rlyStaleFixture {
	t.Helper()
	db, err := sqlitestore.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Runs().Save(context.Background(), &run.Run{
		ID: "run_stale", Playbook: "site.yml", Status: run.StatusPending, Queue: "dmz",
		CredentialIDs: []string{"cred_fleet"}, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed Save() error = %v", err)
	}
	key, err := handoff.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	opener := newScriptedOpener()
	ts := httptest.NewServer(NewHandler(db.Runs(), &Pools{pools: []Pool{keyedPool(key)}}, nil, nil,
		db.Audits(), WithSecretOpener(opener)))
	t.Cleanup(ts.Close)
	return &rlyStaleFixture{url: ts.URL, store: db.Runs(), opener: opener, ring: keyRing(t, key)}
}

// transport returns a worker transport dialing the fixture's control node.
func (f *rlyStaleFixture) transport(t *testing.T) *httpTransport {
	t.Helper()
	tr, ok := NewHTTPTransport(f.url, deliveryToken, nil).(*httpTransport)
	if !ok {
		t.Fatal("NewHTTPTransport() did not return an *httpTransport")
	}
	return tr
}

// TestRelayStaleClaimantCannotStartARequeuedRun is a relay worker that claimed a run, received
// its secrets sealed to the claim, and then stalled past its lease before its fenced start: a
// paused virtual machine, a stopped process, a long hang on the network. The control node's
// janitor requeued the run, which clears the claim and its capability. The worker then wakes and
// asks to start the run under the lease it no longer holds.
//
// The relay documentation promises that a worker opens a delivery only after the control node
// has accepted its fenced start under the same lease. The start endpoint treats a run with no
// stored capability as one claimed before capabilities existed and accepts any caller, and the
// store's fence compares only the status, so the stale claimant's start is accepted, the worker
// decrypts a delivery whose claim ended, and the run executes under no capability at all. The
// control node's held-release sweep then sees the run's claim gone and revokes the dynamic
// secrets that delivery minted, while the run is executing with them, and any worker of the pool
// can now finish the run with a report that carries no lease.
func TestRelayStaleClaimantCannotStartARequeuedRun(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fx := rlyNewStaleFixture(t)
	worker := fx.transport(t)
	receiver := NewReceiver(worker, fx.ring)

	leased, err := worker.Claim(ctx, "relay-a", []string{"dmz"})
	if err != nil {
		t.Fatalf("Claim() error = %v", err)
	}
	if worker.leaseFor(leased.ID) == "" {
		t.Fatal("the claim carried no capability, so the scenario cannot be set up")
	}
	// The janitor sweeps the lease once it is older than the lease lifetime. A negative lifetime
	// is that sweep without waiting thirty seconds of wall clock.
	if n, err := fx.store.ReclaimStale(ctx, -time.Minute); err != nil || n != 1 {
		t.Fatalf("ReclaimStale() = %d, %v, want the stale claim requeued", n, err)
	}
	requeued, err := fx.store.Get(ctx, leased.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if requeued.Status != run.StatusPending || requeued.ClaimedBy != "" {
		t.Fatalf("after the sweep the run is %s held by %q, want pending and unclaimed",
			requeued.Status, requeued.ClaimedBy)
	}

	moved, err := worker.Start(ctx, leased.ID, "relay-a", time.Now())
	if err != nil || !moved {
		// Refused, as a lease the control node took back must be.
		return
	}
	t.Errorf("the control node accepted a fenced start under a lease its janitor had already " +
		"taken back, so a stalled worker started a run it no longer held")

	payload, rerr := receiver.Receive(ctx, leased)
	if rerr == nil && payload != nil {
		t.Errorf("the stalled worker then opened the secrets sealed to the claim that ended: %d "+
			"credentials and %d answers in the clear", len(payload.Credentials), len(payload.Answers))
		payload.Wipe()
	}

	// Any later claim at the control node runs its held-release sweep. Another worker of the pool
	// polling for work is enough.
	if _, err := fx.transport(t).Claim(ctx, "relay-b", []string{"dmz"}); !errors.Is(err,
		run.ErrNonePending) {
		t.Fatalf("second worker Claim() error = %v, want nothing pending", err)
	}
	select {
	case id := <-fx.opener.released:
		current, gerr := fx.store.Get(ctx, id)
		if gerr != nil {
			t.Fatalf("Get() error = %v", gerr)
		}
		if current.Status == run.StatusRunning {
			t.Errorf("the control node revoked what %s's delivery minted while %s was still "+
				"executing it under that delivery", id, current.ClaimedBy)
		}
	case <-time.After(5 * time.Second):
	}

	// The run now executes with no capability on its row, so the proof every report must carry is
	// gone for it: another worker of the pool that never held the run reports it finished, naming
	// the holder it read from the run, and presents no lease at all.
	if err := fx.transport(t).Save(ctx, &run.Run{ID: leased.ID, Status: run.StatusSucceeded,
		ClaimedBy: "relay-a"}); err == nil {
		t.Errorf("a worker that never held the run reported it succeeded with no lease, and the " +
			"control node recorded it while relay-a was still executing it")
	}
}

// TestRelayStartRefusesAWorkerThatNeverClaimedTheRun is the same fence asked by a worker of the
// pool that never claimed the run at all. A pending run nobody has claimed carries no capability,
// and the start endpoint reads that as a run claimed before capabilities existed, so any holder of
// the pool token moves it to running under a name of its choosing, with no claim record in the
// audit chain, no delivery, and no capability guarding any report made for it afterward.
func TestRelayStartRefusesAWorkerThatNeverClaimedTheRun(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fx := rlyNewStaleFixture(t)
	tests := []struct {
		Name  string
		Owner string
	}{{ // Test 0: A worker of the pool that saw the run's id and never claimed it.
		Name: "never claimed", Owner: "relay-x",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			moved, err := fx.transport(t).Start(ctx, "run_stale", test.Owner, time.Now())
			if err != nil || !moved {
				return
			}
			got, gerr := fx.store.Get(ctx, "run_stale")
			if gerr != nil {
				t.Fatalf("Get() error = %v", gerr)
			}
			t.Errorf("a start from %s, which never claimed the run, moved it to %s held by %q",
				test.Owner, got.Status, got.ClaimedBy)
		})
	}
}
