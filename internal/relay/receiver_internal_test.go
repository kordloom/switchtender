package relay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/handoff"
	"github.com/kordloom/switchtender/internal/run"
)

// cannedClaims is a stand-in control node that answers each claim with the next scripted answer, so
// a test can put any delivery, genuine, replayed, or misdirected, in front of a worker's transport.
type cannedClaims struct {
	// mu guards answers.
	mu sync.Mutex
	// answers are the claim answers still to give, in order.
	answers []cannedAnswer
}

// cannedAnswer is one scripted claim answer.
type cannedAnswer struct {
	// lease is the per-claim capability header the answer carries.
	lease string
	// body is the claim answer's body.
	body []byte
}

// handler answers claims from the script and every save with 204.
func (c *cannedClaims) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /relay/v1/claim", func(w http.ResponseWriter, _ *http.Request) {
		c.mu.Lock()
		if len(c.answers) == 0 {
			c.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next := c.answers[0]
		c.answers = c.answers[1:]
		c.mu.Unlock()
		w.Header().Set(leaseHeader, next.lease)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(next.body)
	})
	mux.HandleFunc("POST /relay/v1/runs/{id}/save", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}

// cannedTransport serves answers and returns a worker transport dialing them.
func cannedTransport(t *testing.T, answers ...cannedAnswer) *httpTransport {
	t.Helper()
	ts := httptest.NewServer((&cannedClaims{answers: answers}).handler())
	t.Cleanup(ts.Close)
	tr, ok := NewHTTPTransport(ts.URL, deliveryToken, ts.Client()).(*httpTransport)
	if !ok {
		t.Fatal("NewHTTPTransport() did not return an *httpTransport")
	}
	return tr
}

// sealedFor returns a claim answer for runID under lease, carrying a delivery sealed with key for
// the claim sealRun, sealLease, and sealOwner. A genuine answer seals for the claim it answers. A
// test that seals for another claim stands in for a replay or a misdirected delivery.
func sealedFor(t *testing.T, key *handoff.PrivateKey, runID, lease, sealRun, sealLease, sealOwner string) cannedAnswer {
	t.Helper()
	env, err := handoff.Seal(key.Public(), "dmz", handoff.Binding{RunID: sealRun, Lease: sealLease,
		Owner: sealOwner}, deliveredPayload(sealRun))
	if err != nil {
		t.Fatalf("Seal() error = %v", err)
	}
	return answerWith(t, runID, lease, &claimDelivery{Sealed: env})
}

// answerWith returns a claim answer for runID under lease carrying d.
func answerWith(t *testing.T, runID, lease string, d *claimDelivery) cannedAnswer {
	t.Helper()
	body, err := json.Marshal(claimResponse{Run: &run.Run{ID: runID, Playbook: "site.yml",
		Status: run.StatusPending, Queue: "dmz", CredentialIDs: []string{"cred_fleet"}}, Delivery: d})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	return cannedAnswer{lease: lease, body: body}
}

// claimAndReceive claims once over tr and opens what the claim delivered for its run.
func claimAndReceive(t *testing.T, tr *httpTransport, rc *Receiver) (*handoff.Payload, error) {
	t.Helper()
	leased, err := tr.Claim(context.Background(), "relay-a", []string{"dmz"})
	if err != nil {
		t.Fatalf("Claim() error = %v", err)
	}
	return rc.Receive(context.Background(), leased)
}

// keyRing builds a ring from keys.
func keyRing(t *testing.T, keys ...*handoff.PrivateKey) *handoff.KeyRing {
	t.Helper()
	r, err := handoff.NewKeyRing(keys...)
	if err != nil {
		t.Fatalf("NewKeyRing() error = %v", err)
	}
	return r
}

// TestAWorkerOpensOnlyWhatWasSealedForItsClaim pins the worker's half of the binding, through the
// real transport. A genuine delivery opens. Everything else is refused: the same delivery presented
// a second time, a delivery from an earlier claim of the run replayed into a later one, another
// run's delivery handed to this run, a delivery sealed for another worker, another pool's delivery,
// a delivery reaching a worker with no key, and a delivery the control node refused.
func TestAWorkerOpensOnlyWhatWasSealedForItsClaim(t *testing.T) {
	t.Parallel()
	keyA, err := handoff.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	keyB, err := handoff.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	genuine := sealedFor(t, keyA, "run_a", "lease-1", "run_a", "lease-1", "relay-a")
	tests := []struct {
		Name    string
		Answers []cannedAnswer
		Ring    *handoff.KeyRing
		Want    error
		Reason  string
	}{{ // Test 0: The delivery sealed for this claim opens.
		Name: "genuine", Answers: []cannedAnswer{genuine}, Ring: keyRing(t, keyA), Want: nil,
	}, { // Test 1: The same claim answer replayed is refused the second time.
		Name: "replayed", Answers: []cannedAnswer{genuine, genuine}, Ring: keyRing(t, keyA),
		Want: ErrDeliveryReplay,
	}, { // Test 2: An earlier claim's delivery replayed into a later claim does not open.
		Name: "earlier claim",
		Answers: []cannedAnswer{
			sealedFor(t, keyA, "run_a", "lease-2", "run_a", "lease-1", "relay-a")},
		Ring: keyRing(t, keyA), Want: handoff.ErrBinding,
	}, { // Test 3: Another run's delivery handed to this run does not open.
		Name: "another run",
		Answers: []cannedAnswer{
			sealedFor(t, keyA, "run_b", "lease-1", "run_a", "lease-1", "relay-a")},
		Ring: keyRing(t, keyA), Want: handoff.ErrBinding,
	}, { // Test 4: A delivery sealed for another worker of the pool does not open here.
		Name: "another worker",
		Answers: []cannedAnswer{
			sealedFor(t, keyA, "run_a", "lease-1", "run_a", "lease-1", "relay-b")},
		Ring: keyRing(t, keyA), Want: handoff.ErrBinding,
	}, { // Test 5: A worker of another pool holds another key.
		Name: "another pool", Answers: []cannedAnswer{genuine}, Ring: keyRing(t, keyB),
		Want: handoff.ErrUnknownKey,
	}, { // Test 6: A worker started with no key says so.
		Name: "no key", Answers: []cannedAnswer{genuine}, Ring: nil, Want: ErrNoDeliveryKey,
	}, { // Test 7: The control node's refusal reaches the run with its reason.
		Name: "refused",
		Answers: []cannedAnswer{answerWith(t, "run_a", "lease-1",
			&claimDelivery{Refused: `worker pool "dmz" has registered no delivery key`})},
		Ring: keyRing(t, keyA), Want: ErrDeliveryRefused,
		Reason: `worker pool "dmz" has registered no delivery key`,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			tr := cannedTransport(t, test.Answers...)
			rc := NewReceiver(tr, test.Ring)
			var got *handoff.Payload
			var err error
			for range test.Answers {
				got, err = claimAndReceive(t, tr, rc)
			}
			if !errors.Is(err, test.Want) {
				t.Fatalf("Receive() error = %v, want %v", err, test.Want)
			}
			if test.Reason != "" && !strings.Contains(err.Error(), test.Reason) {
				t.Errorf("Receive() error = %q, want it to carry %q", err, test.Reason)
			}
			if err != nil {
				if got != nil {
					t.Errorf("Receive() returned a payload beside its error")
				}
				return
			}
			if diff := cmp.Diff(deliveredPayload("run_a"), got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("payload mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestADeliveryNobodyOpensIsWipedWhenTheRunEnds pins that a held delivery's ciphertext does not
// outlive its claim on any path: the executor discarding it, the run's terminal save, a new claim
// of the same run, and the executor opening it. Only ciphertext is ever held, and it is zeroed.
func TestADeliveryNobodyOpensIsWipedWhenTheRunEnds(t *testing.T) {
	t.Parallel()
	key, err := handoff.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	tests := []struct {
		Name string
		End  func(t *testing.T, tr *httpTransport, rc *Receiver, leased *run.Run)
	}{{ // Test 0: The executor discards it, as it does on every end of a run.
		Name: "discarded",
		End: func(_ *testing.T, _ *httpTransport, rc *Receiver, leased *run.Run) {
			rc.Discard(leased.ID)
		},
	}, { // Test 1: The run's terminal save closes its record.
		Name: "terminal save",
		End: func(t *testing.T, tr *httpTransport, _ *Receiver, leased *run.Run) {
			done := *leased
			done.Status = run.StatusFailed
			if err := tr.Save(context.Background(), &done); err != nil {
				t.Fatalf("Save() error = %v", err)
			}
		},
	}, { // Test 2: The same run is claimed again, so the first claim's delivery is stale.
		Name: "claimed again",
		End: func(t *testing.T, tr *httpTransport, _ *Receiver, _ *run.Run) {
			if _, err := tr.Claim(context.Background(), "relay-a", []string{"dmz"}); err != nil {
				t.Fatalf("Claim() error = %v", err)
			}
		},
	}, { // Test 3: The executor opens it.
		Name: "opened",
		End: func(t *testing.T, _ *httpTransport, rc *Receiver, leased *run.Run) {
			p, err := rc.Receive(context.Background(), leased)
			if err != nil {
				t.Fatalf("Receive() error = %v", err)
			}
			p.Wipe()
		},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			first := sealedFor(t, key, "run_a", "lease-1", "run_a", "lease-1", "relay-a")
			second := sealedFor(t, key, "run_a", "lease-2", "run_a", "lease-2", "relay-a")
			tr := cannedTransport(t, first, second)
			rc := NewReceiver(tr, keyRing(t, key))
			leased, err := tr.Claim(context.Background(), "relay-a", []string{"dmz"})
			if err != nil {
				t.Fatalf("Claim() error = %v", err)
			}
			tr.mu.Lock()
			held := tr.deliveries["run_a"]
			tr.mu.Unlock()
			if held.delivery == nil || held.delivery.Sealed == nil {
				t.Fatalf("the transport did not hold the claim's delivery")
			}
			ciphertext := held.delivery.Sealed.Ciphertext
			test.End(t, tr, rc, leased)
			for i, b := range ciphertext {
				if b != 0 {
					t.Fatalf("byte %d of the first claim's ciphertext survived the run's end", i)
				}
			}
			tr.mu.Lock()
			still, ok := tr.deliveries["run_a"]
			tr.mu.Unlock()
			if ok && still.lease == "lease-1" {
				t.Errorf("the transport still holds the first claim's delivery")
			}
		})
	}
}

// TestAReceiverForgetsOpenedDeliveriesOnlyWhenItMust pins the bound on what a long-lived worker
// remembers. A delivery inside the window is refused a second time. Past the window it is
// forgotten, which is safe because the lease it was bound to is long gone, and at the size cap the
// oldest goes first.
func TestAReceiverForgetsOpenedDeliveriesOnlyWhenItMust(t *testing.T) {
	t.Parallel()
	now := time.Now()
	rc := &Receiver{opened: map[string]time.Time{}, now: func() time.Time { return now }}
	if !rc.markOpened("d1") || rc.markOpened("d1") {
		t.Fatalf("a delivery was not refused the second time")
	}
	now = now.Add(openedWindow + time.Minute)
	if !rc.markOpened("d1") {
		t.Errorf("a delivery past the window was still remembered")
	}
	for i := range maxOpened {
		rc.opened[fmt.Sprintf("fill-%d", i)] = now.Add(time.Duration(i) * time.Nanosecond)
	}
	rc.opened["oldest"] = now.Add(-time.Hour)
	if !rc.markOpened("newest") {
		t.Fatalf("a new delivery was refused at the cap")
	}
	if _, kept := rc.opened["oldest"]; kept {
		t.Errorf("the oldest remembered delivery was not the one forgotten at the cap")
	}
	if len(rc.opened) > maxOpened {
		t.Errorf("the receiver remembers %d deliveries, past the cap of %d", len(rc.opened), maxOpened)
	}
}

// TestNewReceiverRefusesATransportThatKeepsNoDeliveries pins the wiring check: a receiver built on
// a transport that never holds deliveries would report every run as delivered nothing.
func TestNewReceiverRefusesATransportThatKeepsNoDeliveries(t *testing.T) {
	t.Parallel()
	if got := wantPanic(t, func() { NewReceiver(Loopback(run.NewMemStore()), nil) }); got == nil {
		t.Error("NewReceiver() accepted a transport that keeps no deliveries")
	}
}
