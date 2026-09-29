package relay_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/relay"
	"github.com/kordloom/switchtender/internal/run"
)

// claimAs asks the relay to lease from the named queues with the given token, returning the status.
func claimAs(t *testing.T, base, token string, queues []string) int {
	t.Helper()
	body := `{"owner":"worker-1","queues":["` + strings.Join(queues, `","`) + `"]}`
	if len(queues) == 0 {
		body = `{"owner":"worker-1"}`
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		base+"/relay/v1/claim", strings.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode
}

// TestWorkerTokenIsConfinedToItsQueues checks that a queue is a boundary rather than a routing hint.
//
// The relay exists to put workers in segments the control node cannot reach, which means the least
// trusted machine in the estate holds a worker token. With one token for every worker, that machine
// could name any queue it liked and lease from it: a compromised host in the DMZ took a production
// run and executed it with production credentials. Binding queues to the token is what makes the
// separation real.
func TestWorkerTokenIsConfinedToItsQueues(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "workers.yaml")
	const dmzToken, prodToken = "tok-dmz-secret", "tok-prod-secret"
	doc := "workers:\n" +
		"  - name: dmz\n    token_sha256: " + relay.HashToken(dmzToken) + "\n    queues: [dmz]\n" +
		"  - name: prod\n    token_sha256: " + relay.HashToken(prodToken) + "\n    queues: [prod, dmz]\n"
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	pools, err := relay.LoadPools(path)
	if err != nil {
		t.Fatalf("LoadPools() error = %v", err)
	}

	store := run.NewMemStore()
	if err := store.Save(context.Background(), &run.Run{
		ID: "run_prod", Playbook: "prod.yml", Status: run.StatusPending,
		Queue: "prod", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	ts := httptest.NewServer(relay.NewHandler(store, pools, nil, nil, nil))
	t.Cleanup(ts.Close)

	// The DMZ token cannot name the production queue.
	if code := claimAs(t, ts.URL, dmzToken, []string{"prod"}); code != http.StatusForbidden {
		t.Errorf("a dmz worker claiming the prod queue answered %d, so a compromised host in the "+
			"least trusted segment can take a production run", code)
	}
	// Nor can it reach it by naming both.
	if code := claimAs(t, ts.URL, dmzToken, []string{"dmz", "prod"}); code != http.StatusForbidden {
		t.Errorf("naming its own queue alongside prod answered %d", code)
	}
	// Nor by naming none, which means the default queue.
	if code := claimAs(t, ts.URL, dmzToken, nil); code != http.StatusForbidden {
		t.Errorf("naming no queue answered %d; a confined pool must not get the default queue "+
			"by omission", code)
	}
	// Its own queue is served, so the confinement did not break the worker.
	if code := claimAs(t, ts.URL, dmzToken, []string{"dmz"}); code >= 400 {
		t.Errorf("a dmz worker claiming its own queue answered %d", code)
	}
	// The production pool may take the production run.
	if code := claimAs(t, ts.URL, prodToken, []string{"prod"}); code >= 400 {
		t.Errorf("the prod pool claiming its own queue answered %d", code)
	}
	// An unknown token gets nothing.
	if code := claimAs(t, ts.URL, "tok-not-a-pool", []string{"dmz"}); code != http.StatusUnauthorized {
		t.Errorf("an unknown token answered %d, want 401", code)
	}
}

// TestSinglePoolServesEveryQueue checks that an install with one token and no pool file keeps
// working, and that its lack of confinement is a stated choice rather than an accident.
func TestSinglePoolServesEveryQueue(t *testing.T) {
	t.Parallel()
	store := run.NewMemStore()
	ts := httptest.NewServer(relay.NewHandler(store, relay.SinglePool(testWorkerToken), nil, nil, nil))
	t.Cleanup(ts.Close)
	for _, q := range [][]string{{"prod"}, {"dmz"}, nil} {
		if code := claimAs(t, ts.URL, testWorkerToken, q); code >= 400 {
			t.Errorf("single-pool claim of %v answered %d", q, code)
		}
	}
}

// TestLoadPoolsRefusesAFileItCannotTrust checks that a malformed pool file stops the server rather
// than degrading to no confinement.
func TestLoadPoolsRefusesAFileItCannotTrust(t *testing.T) {
	t.Parallel()
	dup := relay.HashToken("same")
	bad := map[string]string{
		"no workers":      "workers: []\n",
		"no name":         "workers:\n  - token_sha256: " + dup + "\n",
		"no token":        "workers:\n  - name: a\n",
		"plaintext token": "workers:\n  - name: a\n    token_sha256: hunter2\n",
		"shared token": "workers:\n  - name: a\n    token_sha256: " + dup +
			"\n  - name: b\n    token_sha256: " + dup + "\n",
	}
	for name, doc := range bad {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "workers.yaml")
			if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}
			if _, err := relay.LoadPools(path); err == nil {
				t.Error("a pool file that cannot be trusted was accepted, so an install starts " +
					"believing it is confined when it is not")
			}
		})
	}
}

// TestRelayRecordsWhatCrossesTheBoundary checks that work leaving for a segment the control node
// cannot reach, and the outcome coming back, both land in the audit chain.
//
// The relay is mounted outside the API's own gate, so nothing a worker reported reached the trail at
// all. For a product whose claim is that every change is provable, a run starting and finishing on a
// machine nobody here can see is exactly the change worth recording. What is recorded is the
// decision, not the stream: output, events, summaries, and heartbeats arrive several times a second
// and would drown the record they are meant to make readable.
func TestRelayRecordsWhatCrossesTheBoundary(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	audits := audit.NewMemStore()
	if err := store.Save(ctx, &run.Run{
		ID: "run_1", Playbook: "site.yml", Status: run.StatusPending, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	ts := httptest.NewServer(relay.NewHandler(store, relay.SinglePool(testWorkerToken), nil, nil, audits))
	t.Cleanup(ts.Close)
	c := relay.NewClient(relay.NewHTTPTransport(ts.URL, testWorkerToken, ts.Client()))

	leased, err := c.Claim(ctx, "worker-dmz", []string{""})
	if err != nil {
		t.Fatalf("Claim() error = %v", err)
	}
	// Output and heartbeats are the content of a run, not decisions about it.
	if err := c.AppendLog(ctx, leased.ID, []byte("ok: [web01]\n")); err != nil {
		t.Fatalf("AppendLog() error = %v", err)
	}
	if err := c.Heartbeat(ctx, leased.ID, "worker-dmz"); err != nil {
		t.Fatalf("Heartbeat() error = %v", err)
	}
	leased.Status = run.StatusSucceeded
	if err := c.Save(ctx, leased); err != nil {
		t.Fatalf("Save(succeeded) error = %v", err)
	}

	chain, err := audits.Chain(ctx)
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	var claimed, finished bool
	for _, e := range chain {
		switch {
		case strings.Contains(e.Path, "/relay/claim/run_1"):
			claimed = true
		case strings.Contains(e.Path, "/relay/finished/run_1/succeeded"):
			finished = true
		}
	}
	if !claimed {
		t.Error("a worker took a run onto a machine this node cannot reach and the trail says nothing")
	}
	if !finished {
		t.Error("a run finished on a worker and the outcome is not in the trail")
	}
	// The stream is not in the chain, so a run cannot flood it.
	if len(chain) > 4 {
		t.Errorf("the chain holds %d entries for one run, so output or heartbeats are being "+
			"recorded and will drown the record", len(chain))
	}
	if ok, at := audit.Verify(chain); !ok {
		t.Errorf("the chain does not verify at entry %d", at)
	}
}

// TestQueueConfinementCoversEveryEndpoint checks that a queue is a boundary on every relay call,
// not only on the one that leases work.
//
// Confining the claim alone was not confinement. A pool that could not lease a production run could
// still read it by id, append to its log, and save a terminal status over it, because seven of the
// eight endpoints never looked at the pool. The queue bounded which work a token could start and
// nothing else, so a worker in the least trusted segment read the playbook, inventory, credential
// ids, and extra vars of a production run and could cancel it. A boundary that holds on one call is
// not a boundary.
func TestQueueConfinementCoversEveryEndpoint(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "workers.yaml")
	const dmzToken, prodToken = "tok-dmz", "tok-prod"
	doc := "workers:\n" +
		"  - name: dmz\n    token_sha256: " + relay.HashToken(dmzToken) + "\n    queues: [dmz]\n" +
		"  - name: prod\n    token_sha256: " + relay.HashToken(prodToken) + "\n    queues: [prod]\n"
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	pools, err := relay.LoadPools(path)
	if err != nil {
		t.Fatalf("LoadPools() error = %v", err)
	}

	store := run.NewMemStore()
	held := time.Now()
	secret := &run.Run{
		ID: "run_prod", Playbook: "site.yml", Inventory: "prod.ini", Queue: "prod",
		Status: run.StatusRunning, CreatedAt: time.Now(),
		ClaimedBy: "prod-worker", ClaimedAt: &held,
		CredentialIDs: []string{"cred_prod_ssh"},
		ExtraVars:     map[string]any{"vault_password": "s3cr3t-prod"},
	}
	if err := store.Save(ctx, secret); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	ts := httptest.NewServer(relay.NewHandler(store, pools, nil, nil, nil))
	t.Cleanup(ts.Close)

	// Every endpoint, with the wrong pool's token.
	probes := []struct {
		Name   string
		Method string
		Path   string
		Body   string
	}{
		{"read the run", http.MethodGet, "/relay/v1/runs/run_prod", ""},
		{"append output", http.MethodPost, "/relay/v1/runs/run_prod/log", "PLAY RECAP ok=12\n"},
		{"append events", http.MethodPost, "/relay/v1/runs/run_prod/events", "[]"},
		{"host summary", http.MethodPost, "/relay/v1/runs/run_prod/host-summary", "[]"},
		{"host facts", http.MethodPost, "/relay/v1/runs/run_prod/host-facts", "[]"},
		{"task summary", http.MethodPost, "/relay/v1/runs/run_prod/task-summary", "[]"},
		{"kill the run", http.MethodPost, "/relay/v1/runs/run_prod/save",
			`{"status":"canceled","error":"killed by another pool"}`},
		{"renew the lease", http.MethodPost, "/relay/v1/heartbeat",
			`{"id":"run_prod","owner":"dmz-worker"}`},
	}
	for _, p := range probes {
		req, rerr := http.NewRequestWithContext(ctx, p.Method, ts.URL+p.Path, strings.NewReader(p.Body))
		if rerr != nil {
			t.Fatalf("NewRequest() error = %v", rerr)
		}
		req.Header.Set("Authorization", "Bearer "+dmzToken)
		resp, derr := http.DefaultClient.Do(req)
		if derr != nil {
			t.Fatalf("Do() error = %v", derr)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode < 400 {
			t.Errorf("a dmz token could %s on a prod-queue run: %d", p.Name, resp.StatusCode)
		}
		if strings.Contains(string(body), "s3cr3t-prod") || strings.Contains(string(body), "cred_prod_ssh") {
			t.Errorf("%s leaked the run's spec to the wrong pool: %s", p.Name, body)
		}
	}

	// Nothing the wrong pool sent was applied.
	after, err := store.Get(ctx, "run_prod")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if after.Status != run.StatusRunning {
		t.Errorf("the run is %q, so another pool terminated work it may not even see", after.Status)
	}
	if log, lerr := store.Log(ctx, "run_prod"); lerr != nil {
		t.Fatalf("Log() error = %v", lerr)
	} else if len(log) != 0 {
		t.Errorf("another pool wrote %q into the run's record", log)
	}

	// The owning pool is unaffected.
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/relay/v1/runs/run_prod", nil)
	req.Header.Set("Authorization", "Bearer "+prodToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("the owning pool was refused its own run: %d", resp.StatusCode)
	}
}
