package relay_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	ts := httptest.NewServer(relay.NewHandler(store, pools, nil, nil))
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
	ts := httptest.NewServer(relay.NewHandler(store, relay.SinglePool(testWorkerToken), nil, nil))
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
