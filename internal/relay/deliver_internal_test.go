package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/handoff"
	"github.com/kordloom/switchtender/internal/run"
)

const (
	// deliveryToken is the bearer token of the pool the delivery tests claim as.
	deliveryToken = "delivery-pool-token"
	// deliveryKeyMaterial is the SSH key value the scripted opener delivers, looked for wherever it
	// must not appear.
	deliveryKeyMaterial = "-----BEGIN OPENSSH PRIVATE KEY-----\ndelivered-key-body-7f3a\n" +
		"-----END OPENSSH PRIVATE KEY-----\n"
	// deliveryAnswer is the secret survey answer the scripted opener delivers.
	deliveryAnswer = "delivered-survey-answer-91c2"
)

// scriptedOpener is a SecretOpener a test drives: whether a run needs secrets, what opening them
// yields, and every release the relay runs.
type scriptedOpener struct {
	// mu guards opened.
	mu sync.Mutex
	// needs is what NeedsSecrets answers.
	needs bool
	// err, when set, is what OpenSecrets fails with.
	err error
	// opened counts OpenSecrets calls.
	opened int
	// released receives the run id of every release the relay runs.
	released chan string
}

// newScriptedOpener returns an opener that reports secrets needed and opens them.
func newScriptedOpener() *scriptedOpener {
	return &scriptedOpener{needs: true, released: make(chan string, 16)}
}

// NeedsSecrets answers the scripted value.
func (o *scriptedOpener) NeedsSecrets(context.Context, *run.Run) bool { return o.needs }

// OpenSecrets returns a payload carrying an SSH key and a secret answer, with a release that
// reports itself.
func (o *scriptedOpener) OpenSecrets(_ context.Context, r *run.Run) (*handoff.Payload, func(), error) {
	o.mu.Lock()
	o.opened++
	o.mu.Unlock()
	if o.err != nil {
		return nil, nil, o.err
	}
	return deliveredPayload(r.ID), func() { o.released <- r.ID }, nil
}

// openCount reports how many times the relay opened a run's secrets.
func (o *scriptedOpener) openCount() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.opened
}

// deliveredPayload is what the scripted opener opens for a run.
func deliveredPayload(runID string) *handoff.Payload {
	return &handoff.Payload{
		RunID: runID, CredentialIDs: []string{"cred_fleet"},
		Credentials: []*handoff.Credential{handoff.NewCredential(&credential.Credential{
			ID: "cred_fleet", Name: "fleet key", Kind: credential.KindSSHKey,
		}, deliveryKeyMaterial)},
		Answers: map[string]string{"db_password": deliveryAnswer},
	}
}

// failingAppends is an audit store whose appends fail, standing in for a trail that cannot record.
type failingAppends struct {
	// Store serves the reads.
	audit.Store
}

// Append refuses.
func (failingAppends) Append(context.Context, *audit.Entry) error {
	return errors.New("the audit store is unavailable")
}

// deliveryFixture is a control node serving one pool, with a run waiting in the pool's queue.
type deliveryFixture struct {
	// url is the relay's base URL.
	url string
	// store is the run store behind it.
	store run.Store
}

// newDeliveryFixture serves pool from a relay with opener and audits, seeding one pending run in
// the dmz queue.
func newDeliveryFixture(t *testing.T, pool Pool, opener SecretOpener, audits audit.Store) *deliveryFixture {
	t.Helper()
	store := run.NewMemStore()
	if err := store.Save(context.Background(), &run.Run{
		ID: "run_delivery", Playbook: "site.yml", Status: run.StatusPending, Queue: "dmz",
		CredentialIDs: []string{"cred_fleet"}, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed Save() error = %v", err)
	}
	var opts []HandlerOption
	if opener != nil {
		opts = append(opts, WithSecretOpener(opener))
	}
	ts := httptest.NewServer(NewHandler(store, &Pools{pools: []Pool{pool}}, nil, nil, audits, opts...))
	t.Cleanup(ts.Close)
	return &deliveryFixture{url: ts.URL, store: store}
}

// keyedPool returns the dmz pool with key registered.
func keyedPool(key *handoff.PrivateKey) Pool {
	p := Pool{Name: "dmz", TokenSHA256: HashToken(deliveryToken), Queues: []string{"dmz"}}
	if key != nil {
		p.DeliveryKey, p.key = key.Public().String(), key.Public()
	}
	return p
}

// claimAnswer is one claim's answer as the wire carried it.
type claimAnswer struct {
	// Status is the HTTP status.
	Status int
	// Lease is the per-claim capability header.
	Lease string
	// Body is the raw body.
	Body []byte
	// Response is the body decoded.
	Response claimResponse
}

// claimOver claims from the dmz queue as relay-a.
func claimOver(t *testing.T, base string) claimAnswer {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		base+"/relay/v1/claim", strings.NewReader(`{"owner":"relay-a","queues":["dmz"]}`))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+deliveryToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}
	out := claimAnswer{Status: resp.StatusCode, Lease: resp.Header.Get(leaseHeader), Body: body,
		Response: claimResponse{Run: &run.Run{}}}
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(body, &out.Response); err != nil {
			t.Fatalf("decode the claim answer: %v", err)
		}
	}
	return out
}

// deliveryEntries returns the chain entries recording deliveries.
func deliveryEntries(t *testing.T, audits audit.Store) []*audit.Entry {
	t.Helper()
	all, err := audits.List(context.Background(), 1000)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	var out []*audit.Entry
	for _, e := range all {
		if strings.HasPrefix(e.Path, "/relay/delivered/") {
			out = append(out, e)
		}
	}
	return out
}

// TestAKeyedPoolReceivesItsRunsSecretsSealed pins the delivery itself: a worker of a pool that
// registered a key claims a run and receives exactly that run's secrets, sealed to the pool's key
// and bound to the claim, with the chain recording which credentials and answers went to which
// worker, and no value anywhere outside the seal.
func TestAKeyedPoolReceivesItsRunsSecretsSealed(t *testing.T) {
	t.Parallel()
	key, err := handoff.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	audits := audit.NewMemStore()
	fx := newDeliveryFixture(t, keyedPool(key), newScriptedOpener(), audits)

	got := claimOver(t, fx.url)
	if got.Status != http.StatusOK || got.Response.Delivery == nil ||
		got.Response.Delivery.Sealed == nil {
		t.Fatalf("claim = %d %s, want the run with its secrets sealed", got.Status, got.Body)
	}
	for _, secret := range []string{deliveryKeyMaterial, "delivered-key-body", deliveryAnswer,
		got.Lease} {
		if bytes.Contains(got.Body, []byte(secret)) {
			t.Errorf("the claim answer carries %q outside the seal", secret)
		}
	}
	ring, err := handoff.NewKeyRing(key)
	if err != nil {
		t.Fatalf("NewKeyRing() error = %v", err)
	}
	opened, err := handoff.Open(ring, handoff.Binding{RunID: "run_delivery", Lease: got.Lease,
		Owner: "relay-a"}, got.Response.Delivery.Sealed)
	if err != nil {
		t.Fatalf("the worker could not open its delivery: %v", err)
	}
	if diff := cmp.Diff(deliveredPayload("run_delivery"), opened, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("delivered payload mismatch (-want +got):\n%s", diff)
	}

	entries := deliveryEntries(t, audits)
	want := []string{"pool:dmz worker:relay-a " + DeliveryPath("run_delivery", key.ID(),
		[]string{"cred_fleet"}, []string{"db_password"})}
	var gotEntries []string
	for _, e := range entries {
		gotEntries = append(gotEntries, e.Actor+" "+e.Path)
	}
	if diff := cmp.Diff(want, gotEntries); diff != "" {
		t.Errorf("delivery record mismatch (-want +got):\n%s", diff)
	}
	all, err := audits.List(context.Background(), 1000)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	chain, err := json.Marshal(all)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	for _, secret := range []string{"delivered-key-body", deliveryAnswer, got.Lease} {
		if bytes.Contains(chain, []byte(secret)) {
			t.Errorf("the chain carries %q", secret)
		}
	}
}

// TestADeliveryIsRefusedWhereItCannotBeMadeSafely pins every refusal. Each one sends nothing secret
// and tells the worker why, so the run fails with a reason instead of executing without its
// credentials. A pool with no key, or one not bound to explicit queues, is refused before anything
// is opened at all. A delivery the chain cannot record is not sent, and what opening it minted is
// handed back.
func TestADeliveryIsRefusedWhereItCannotBeMadeSafely(t *testing.T) {
	t.Parallel()
	key, err := handoff.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	unbound := keyedPool(key)
	unbound.Queues = nil
	tests := []struct {
		Name        string
		Pool        Pool
		Audits      audit.Store
		OpenErr     error
		WantReason  string
		WantOpened  int
		WantRelease bool
	}{{ // Test 0: A pool that registered no key keeps today's behavior and is told why.
		Name: "no key", Pool: keyedPool(nil), Audits: audit.NewMemStore(),
		WantReason: `worker pool "dmz" has registered no delivery key`, WantOpened: 0,
	}, { // Test 1: A pool with a key but no queues could claim anything, so it gets nothing.
		Name: "not bound to queues", Pool: unbound, Audits: audit.NewMemStore(),
		WantReason: `worker pool "dmz" is not bound to explicit queues`, WantOpened: 0,
	}, { // Test 2: An install keeping no trail cannot record the delivery, so it sends none.
		Name: "no audit trail", Pool: keyedPool(key), Audits: nil,
		WantReason: "keeps no audit trail", WantOpened: 0,
	}, { // Test 3: A trail that refuses the record stops the delivery after opening.
		Name: "record refused", Pool: keyedPool(key), Audits: failingAppends{audit.NewMemStore()},
		WantReason: "could not be recorded in the audit trail", WantOpened: 1, WantRelease: true,
	}, { // Test 4: A secret that does not open on the control node is reported, not skipped.
		Name: "open failed", Pool: keyedPool(key), Audits: audit.NewMemStore(),
		OpenErr:    errors.New(`credential "fleet key" does not open`),
		WantReason: `could not open this run's secrets: credential "fleet key" does not open`,
		WantOpened: 1,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			opener := newScriptedOpener()
			opener.err = test.OpenErr
			fx := newDeliveryFixture(t, test.Pool, opener, test.Audits)
			got := claimOver(t, fx.url)
			if got.Status != http.StatusOK {
				t.Fatalf("claim answered %d (%s); a refused delivery must not refuse the claim, or the "+
					"run would wait with nobody to fail it", got.Status, got.Body)
			}
			d := got.Response.Delivery
			if d == nil || d.Sealed != nil || !strings.Contains(d.Refused, test.WantReason) {
				t.Fatalf("delivery = %+v, want a refusal containing %q and nothing sealed", d,
					test.WantReason)
			}
			if bytes.Contains(got.Body, []byte(deliveryAnswer)) ||
				bytes.Contains(got.Body, []byte("delivered-key-body")) {
				t.Errorf("a refused delivery carried a secret: %s", got.Body)
			}
			if n := opener.openCount(); n != test.WantOpened {
				t.Errorf("secrets opened %d times, want %d", n, test.WantOpened)
			}
			if test.Audits != nil {
				if entries := deliveryEntries(t, test.Audits); len(entries) != 0 {
					t.Errorf("a refused delivery was recorded as delivered: %v", entries[0].Path)
				}
			}
			if test.WantRelease {
				select {
				case id := <-opener.released:
					if id != "run_delivery" {
						t.Errorf("released %s, want run_delivery", id)
					}
				case <-time.After(5 * time.Second):
					t.Errorf("what the opening minted was never handed back after the refusal")
				}
			}
		})
	}
}

// TestARunNeedingNoSecretAnswersWithTheRunAlone pins that delivery is additive on the wire. A run
// that needs no secret is answered with the run and nothing else, and an answer that does carry a
// delivery still decodes as the run for a worker that predates delivery, so a mixed fleet keeps
// claiming during an upgrade.
func TestARunNeedingNoSecretAnswersWithTheRunAlone(t *testing.T) {
	t.Parallel()
	key, err := handoff.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	quiet := newScriptedOpener()
	quiet.needs = false
	fx := newDeliveryFixture(t, keyedPool(key), quiet, audit.NewMemStore())
	got := claimOver(t, fx.url)
	if got.Status != http.StatusOK {
		t.Fatalf("claim = %d %s", got.Status, got.Body)
	}
	if bytes.Contains(got.Body, []byte("relay_secret_delivery")) || got.Response.Delivery != nil {
		t.Errorf("a run needing no secret carried a delivery: %s", got.Body)
	}
	if n := quiet.openCount(); n != 0 {
		t.Errorf("secrets opened %d times for a run that needs none", n)
	}

	withSecrets := newDeliveryFixture(t, keyedPool(key), newScriptedOpener(), audit.NewMemStore())
	answer := claimOver(t, withSecrets.url)
	var asOldWorker run.Run
	if err := json.Unmarshal(answer.Body, &asOldWorker); err != nil {
		t.Fatalf("an older worker cannot decode a claim answer carrying a delivery: %v", err)
	}
	if asOldWorker.ID != "run_delivery" || asOldWorker.Queue != "dmz" {
		t.Errorf("an older worker decoded run %q in queue %q, want run_delivery in dmz",
			asOldWorker.ID, asOldWorker.Queue)
	}
}

// TestAFinishedRunHandsBackWhatItsDeliveryMinted pins that the control node revokes, through the
// opener's release, whatever opening a delivered run's secrets minted, the moment the worker
// reports the run finished. A worker cannot revoke a dynamic secret the control node minted, so
// nobody would.
func TestAFinishedRunHandsBackWhatItsDeliveryMinted(t *testing.T) {
	t.Parallel()
	key, err := handoff.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	opener := newScriptedOpener()
	fx := newDeliveryFixture(t, keyedPool(key), opener, audit.NewMemStore())
	got := claimOver(t, fx.url)
	if got.Response.Delivery == nil || got.Response.Delivery.Sealed == nil {
		t.Fatalf("claim = %d %s, want a sealed delivery", got.Status, got.Body)
	}
	select {
	case id := <-opener.released:
		t.Fatalf("released %s before the run finished", id)
	default:
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		fx.url+"/relay/v1/runs/run_delivery/save",
		strings.NewReader(`{"status":"succeeded","claimed_by":"relay-a"}`))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+deliveryToken)
	req.Header.Set(leaseHeader, got.Lease)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("terminal save answered %d", resp.StatusCode)
	}
	select {
	case id := <-opener.released:
		if id != "run_delivery" {
			t.Errorf("released %s, want run_delivery", id)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("the finished run's delivery was never released")
	}
}

// TestHeldReleasesFollowTheClaim pins the sweep that releases what a delivery minted when its claim
// ends with no terminal report: the run was deleted, settled by the control node, or requeued and
// claimed again under another lease. A run still held by the claim it was delivered for keeps its
// release, and so does one the store cannot answer about.
func TestHeldReleasesFollowTheClaim(t *testing.T) {
	t.Parallel()
	now := time.Now()
	tests := []struct {
		Name        string
		Stored      *run.Run
		GetErr      error
		Since       time.Duration
		WantRelease bool
	}{{ // Test 0: The same claim still holds the run.
		Name:   "still held",
		Stored: &run.Run{ID: "run_h", Status: run.StatusRunning, ClaimSecret: "lease-one"},
		Since:  time.Hour, WantRelease: false,
	}, { // Test 1: The run was claimed again under another lease.
		Name:   "claimed again",
		Stored: &run.Run{ID: "run_h", Status: run.StatusRunning, ClaimSecret: "lease-two"},
		Since:  time.Hour, WantRelease: true,
	}, { // Test 2: The control node settled it, as the stale-lease sweep does.
		Name:   "settled",
		Stored: &run.Run{ID: "run_h", Status: run.StatusInterrupted, ClaimSecret: "lease-one"},
		Since:  time.Hour, WantRelease: true,
	}, { // Test 3: The run is gone.
		Name: "deleted", GetErr: run.ErrNotFound, Since: time.Hour, WantRelease: true,
	}, { // Test 4: A store that cannot answer is not a claim that ended.
		Name: "store down", GetErr: errors.New("unavailable"), Since: time.Hour, WantRelease: false,
	}, { // Test 5: Swept again too soon after the last sweep, nothing is checked.
		Name:   "too soon",
		Stored: &run.Run{ID: "run_h", Status: run.StatusInterrupted, ClaimSecret: "lease-one"},
		Since:  time.Second, WantRelease: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			released := make(chan struct{}, 1)
			h := &heldReleases{lastSweep: now}
			h.hold("run_h", "lease-one", func() { released <- struct{}{} })
			store := getOnlyStore{run: test.Stored, err: test.GetErr}
			h.sweep(context.Background(), store, now.Add(test.Since))
			select {
			case <-released:
				if !test.WantRelease {
					t.Errorf("released a delivery whose claim had not ended")
				}
			case <-time.After(200 * time.Millisecond):
				if test.WantRelease {
					t.Errorf("kept a delivery whose claim had ended")
				}
			}
		})
	}
}

// TestHoldingASecondClaimReleasesTheFirst pins that a run delivered twice, once per claim, releases
// the first claim's minted secrets as soon as the second delivery is held.
func TestHoldingASecondClaimReleasesTheFirst(t *testing.T) {
	t.Parallel()
	first := make(chan struct{}, 1)
	h := &heldReleases{}
	h.hold("run_h", "lease-one", func() { first <- struct{}{} })
	h.hold("run_h", "lease-two", func() {})
	h.hold("run_h", "lease-three", nil)
	select {
	case <-first:
	case <-time.After(5 * time.Second):
		t.Fatalf("the first claim's release never ran")
	}
}

// getOnlyStore is a run.Store whose Get answers one scripted run or error. Nothing else is called.
type getOnlyStore struct {
	// Store supplies the method set without supplying an implementation.
	run.Store
	// run is what Get returns.
	run *run.Run
	// err is what Get fails with.
	err error
}

// Get returns the scripted answer.
func (g getOnlyStore) Get(context.Context, string) (*run.Run, error) {
	if g.err != nil {
		return nil, g.err
	}
	return g.run.Clone(), nil
}

// TestDeliveryPathEscapesEveryName pins the record's path: names are escaped, so a credential id or
// an answer name cannot add a segment that reads as another part of the record.
func TestDeliveryPathEscapesEveryName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Run, Key   string
		Creds      []string
		Answers    []string
		WantResult string
	}{{ // Test 0: Ordinary ids.
		Run: "run_1", Key: "k1", Creds: []string{"cred_a", "cred_b"}, Answers: []string{"pw"},
		WantResult: "/relay/delivered/run_1/key/k1/credentials/cred_a,cred_b/answers/pw",
	}, { // Test 1: Only answers.
		Run: "run_1", Key: "k1", Answers: []string{"pw"},
		WantResult: "/relay/delivered/run_1/key/k1/answers/pw",
	}, { // Test 2: A name carrying a slash cannot forge a segment.
		Run: "run_1", Key: "k1", Creds: []string{"cred_a/answers/forged"},
		WantResult: "/relay/delivered/run_1/key/k1/credentials/cred_a%2Fanswers%2Fforged",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got := DeliveryPath(test.Run, test.Key, test.Creds, test.Answers)
			if diff := cmp.Diff(test.WantResult, got); diff != "" {
				t.Errorf("DeliveryPath() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestEachPoolsRunsAreSealedToThatPoolsKeyAlone pins the pool boundary at the control node. Two
// pools each registered a key. A run in the second pool's queue, claimed by the second pool's
// worker, is sealed to the second pool's key, so the first pool's worker, holding only its own key,
// cannot open it even with the envelope in hand.
func TestEachPoolsRunsAreSealedToThatPoolsKeyAlone(t *testing.T) {
	t.Parallel()
	keyA, err := handoff.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	keyB, err := handoff.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	poolA := Pool{Name: "edge", TokenSHA256: HashToken("tok-edge"), Queues: []string{"edge"},
		DeliveryKey: keyA.Public().String(), key: keyA.Public()}
	poolB := keyedPool(keyB)
	store := run.NewMemStore()
	if err := store.Save(context.Background(), &run.Run{ID: "run_dmz", Playbook: "site.yml",
		Status: run.StatusPending, Queue: "dmz", CredentialIDs: []string{"cred_fleet"},
		CreatedAt: time.Now()}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	ts := httptest.NewServer(NewHandler(store, &Pools{pools: []Pool{poolA, poolB}}, nil, nil,
		audit.NewMemStore(), WithSecretOpener(newScriptedOpener())))
	t.Cleanup(ts.Close)

	got := claimOver(t, ts.URL)
	if got.Response.Delivery == nil || got.Response.Delivery.Sealed == nil {
		t.Fatalf("claim = %d %s, want a sealed delivery", got.Status, got.Body)
	}
	env := got.Response.Delivery.Sealed
	binding := handoff.Binding{RunID: "run_dmz", Lease: got.Lease, Owner: "relay-a"}
	if _, err := handoff.Open(keyRing(t, keyA), binding, env); err == nil {
		t.Fatalf("the edge pool's worker opened a delivery sealed for the dmz pool")
	}
	if _, err := handoff.Open(keyRing(t, keyB), binding, env); err != nil {
		t.Errorf("the dmz pool's own worker could not open its delivery: %v", err)
	}
}
