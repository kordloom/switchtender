package relay

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/handoff"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/sqlitestore"
)

// rlyReleaseWait is how long a test gives a control node to hand back what a delivery minted once
// the delivery's claim has ended: a full held-sweep interval and then some, which is the most a
// sweep that ran on its own schedule would need.
const rlyReleaseWait = heldSweepInterval + 15*time.Second

// rlyReplica is one control node replica's relay handler over a shared store.
type rlyReplica struct {
	// url is the replica's base URL.
	url string
	// opener records the releases this replica runs.
	opener *scriptedOpener
}

// rlyNewReplica serves the keyed dmz pool from store with an opener of its own, as each replica of
// an install holds its own memory of what it minted.
func rlyNewReplica(t *testing.T, store run.Store, audits audit.Store, pool Pool) *rlyReplica {
	t.Helper()
	opener := newScriptedOpener()
	ts := httptest.NewServer(NewHandler(store, &Pools{pools: []Pool{pool}}, nil, nil, audits,
		WithSecretOpener(opener)))
	t.Cleanup(ts.Close)
	return &rlyReplica{url: ts.URL, opener: opener}
}

// rlyTerminalSave reports runID succeeded to the replica at base under lease, as the worker's
// terminal save does.
func rlyTerminalSave(t *testing.T, base, runID, lease string) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		base+"/relay/v1/runs/"+runID+"/save",
		strings.NewReader(`{"status":"succeeded","claimed_by":"relay-a"}`))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+deliveryToken)
	req.Header.Set(leaseHeader, lease)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("terminal save answered %d", resp.StatusCode)
	}
}

// TestRelayDeliveryReleaseDoesNotWaitForAnotherClaim pins when a control node hands back what
// opening a relayed run's secrets minted, such as a Vault dynamic secret, once the claim it was
// delivered for is over. The documentation promises it is revoked when the worker reports the
// run finished, or when the control node next finds the claim gone.
//
// The only thing that ever looks for a claim that is gone is a later claim request arriving at the
// same control node, and only the node that minted the secret holds the means to revoke it. A pool
// whose only worker dies mid-run leaves its run interrupted by the janitor, and with no worker left
// to poll nothing ever claims again, so the minted secret is never revoked while the control node
// runs. With two replicas on one database, a worker that claimed through one and reported the
// finish through the other has its finish handled by a replica holding nothing, and the replica
// that minted the secret revokes it only if some later claim happens to reach it. A replica drained
// out of the load balancer never sees one.
func TestRelayDeliveryReleaseDoesNotWaitForAnotherClaim(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name string
		// FinishElsewhere reports the finish through a second replica instead of letting the
		// worker die.
		FinishElsewhere bool
	}{{ // Test 0: The pool's only worker dies mid-run and the janitor interrupts the run.
		Name: "worker died",
	}, { // Test 1: The finish lands on another replica of the same install.
		Name: "finished at another replica", FinishElsewhere: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			db, err := sqlitestore.Open(filepath.Join(t.TempDir(), "control.db"))
			if err != nil {
				t.Fatalf("Open() error = %v", err)
			}
			t.Cleanup(func() { _ = db.Close() })
			if err := db.Runs().Save(ctx, &run.Run{
				ID: "run_held", Playbook: "site.yml", Status: run.StatusPending, Queue: "dmz",
				CredentialIDs: []string{"cred_fleet"}, CreatedAt: time.Now(),
			}); err != nil {
				t.Fatalf("seed Save() error = %v", err)
			}
			key, err := handoff.GenerateKey()
			if err != nil {
				t.Fatalf("GenerateKey() error = %v", err)
			}
			minter := rlyNewReplica(t, db.Runs(), db.Audits(), keyedPool(key))
			worker, ok := NewHTTPTransport(minter.url, deliveryToken, nil).(*httpTransport)
			if !ok {
				t.Fatal("NewHTTPTransport() did not return an *httpTransport")
			}
			leased, err := worker.Claim(ctx, "relay-a", []string{"dmz"})
			if err != nil {
				t.Fatalf("Claim() error = %v", err)
			}
			if held, ok := worker.takeDelivery(leased.ID); !ok || held.delivery == nil ||
				held.delivery.Sealed == nil {
				t.Fatalf("the claim carried no sealed delivery, so nothing was minted to hand back")
			}
			if moved, err := worker.Start(ctx, leased.ID, "relay-a", time.Now()); err != nil ||
				!moved {
				t.Fatalf("Start() = %v, %v, want the run started", moved, err)
			}

			if test.FinishElsewhere {
				other := rlyNewReplica(t, db.Runs(), db.Audits(), keyedPool(key))
				rlyTerminalSave(t, other.url, leased.ID, worker.leaseFor(leased.ID))
			} else if _, err := db.Runs().ReclaimStale(ctx, -time.Minute); err != nil {
				t.Fatalf("ReclaimStale() error = %v", err)
			}
			ended, err := db.Runs().Get(ctx, leased.ID)
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			if !ended.Status.Terminal() {
				t.Fatalf("the run is %s, want it terminal before waiting for the release",
					ended.Status)
			}

			select {
			case <-minter.opener.released:
			case <-time.After(rlyReleaseWait):
				t.Errorf("the run ended %s and %s later the control node that minted its "+
					"delivery's secrets had still not handed them back, because no later claim "+
					"reached it", ended.Status, rlyReleaseWait)
			}
		})
	}
}
