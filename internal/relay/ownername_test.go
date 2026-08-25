package relay_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/relay"
	"github.com/kordloom/switchtender/internal/run"
)

// recordedActorFor leases a run as owner and returns the audit actor the relay recorded for it.
func recordedActorFor(t *testing.T, owner string) string {
	t.Helper()
	audits := audit.NewMemStore()
	backing := run.NewMemStore()
	ts := httptest.NewServer(relay.NewHandler(backing,
		relay.SinglePool(testWorkerToken), nil, nil, audits))
	t.Cleanup(ts.Close)
	if err := backing.Save(context.Background(), &run.Run{
		ID: "run_x", Playbook: "site.yml", Status: run.StatusPending, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	body, err := json.Marshal(map[string]any{"owner": owner, "queues": []string{""}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/relay/v1/claim", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+testWorkerToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("claim status = %d, want 200", resp.StatusCode)
	}
	entries, err := audits.List(context.Background(), 10)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("audit entries = %d, want 1", len(entries))
	}
	return entries[0].Actor
}

// TestAWorkerCannotWriteAnUnboundedActorIntoTheChain pins that the lease name a worker asserts is
// bounded before it becomes part of a chain link.
//
// The name arrives in the request body and was used as given. One claim wrote a two hundred thousand
// character actor into the audit chain, which is hashed into a link and then carried in every bundle
// exported afterwards, so a worker could grow the product's own evidence without limit. The token
// this needs is the one the design treats as the least trusted thing in the estate.
func TestAWorkerCannotWriteAnUnboundedActorIntoTheChain(t *testing.T) {
	t.Parallel()
	actor := recordedActorFor(t, strings.Repeat("A", 200000))
	if len(actor) > 256 {
		t.Errorf("recorded actor is %d characters, want it bounded", len(actor))
	}
}

// TestAWorkerCannotAssertASecondIdentity pins that the asserted name cannot hold the separators the
// actor string is built from, so it cannot read as a pool and worker the token never proved.
func TestAWorkerCannotAssertASecondIdentity(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name  string
		Owner string
	}{ // Test 0: A second pool and worker pair appended to a plausible name.
		{"second identity", "w1 pool:production worker:release-admin"},
		// Test 1: The separators alone.
		{"separators", "pool:admin"},
		// Test 2: A newline, which would split the field for a reader consuming lines.
		{"newline", "w1\npool:production worker:root"},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			actor := recordedActorFor(t, test.Owner)
			prefix := "pool:default worker:"
			if !strings.HasPrefix(actor, prefix) {
				t.Fatalf("actor %q does not start with the proven pool", actor)
			}
			if rest := strings.TrimPrefix(actor, prefix); strings.ContainsAny(rest, " :\n\t") {
				t.Errorf("the asserted name kept a separator: %q", actor)
			}
		})
	}
}

// TestAnOrdinaryLeaseNameSurvives pins that the constraint did not take real worker names with it.
func TestAnOrdinaryLeaseNameSurvives(t *testing.T) {
	t.Parallel()
	for _, owner := range []string{"worker-a", "runner_01", "host.example.com", "w1"} {
		if actor := recordedActorFor(t, owner); actor != "pool:default worker:"+owner {
			t.Errorf("owner %q recorded as %q", owner, actor)
		}
	}
}
