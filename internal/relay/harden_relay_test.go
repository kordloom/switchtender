package relay_test

import (
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/event"
	"github.com/kordloom/switchtender/internal/relay"
	"github.com/kordloom/switchtender/internal/run"
)

// TestDecodeCappedStopsDuringDecode proves the element cap bounds the work, not only the result: a
// body over the cap is refused before its later elements are ever parsed. A post-decode cap would
// parse the whole array first and return a syntax error at the trailing garbage instead.
func TestDecodeCappedStopsDuringDecode(t *testing.T) {
	t.Parallel()
	n := relay.MaxRelayElementsForTest()
	tests := []struct {
		Name      string
		Body      string
		WantCount int
		Want      error
	}{
		{Name: "under cap", Body: "[{},{}]", WantCount: 2, Want: nil},
		{Name: "at cap", Body: "[" + strings.Repeat("{},", n-1) + "{}]", WantCount: n, Want: nil},
		{
			Name: "over cap stops before parsing the trailing garbage",
			Body: "[" + strings.Repeat("{},", n) + "%%%not-json%%%]",
			Want: relay.ErrTooManyElements,
		},
		{Name: "not an array", Body: `{"nope":1}`, Want: nil, WantCount: 0},
		{Name: "truncated array", Body: "[", Want: io.EOF},
	}
	for testNum, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			got, err := relay.DecodeCappedForTest[event.Event](strings.NewReader(test.Body), n)
			// "not an array" returns a plain error, not the sentinel; assert only the sentinel rows.
			if test.Name == "not an array" {
				if err == nil {
					t.Errorf("test %d: decoding a non-array succeeded, want an error", testNum)
				}
				return
			}
			if !errors.Is(err, test.Want) {
				t.Errorf("test %d: err = %v, want %v", testNum, err, test.Want)
			}
			if diff := cmp.Diff(test.WantCount, len(got), cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("test %d: count mismatch (-want +got):\n%s", testNum, diff)
			}
		})
	}
}

// TestRelayRefusesAnOversizeArray checks the handler maps an over-cap body to 413 rather than
// decoding it whole and then rejecting. A held, running run is seeded so the report passes the holder
// gate and the cap is the only thing left to answer.
func TestRelayRefusesAnOversizeArray(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	backing := run.NewMemStore()
	ts := httptest.NewServer(relay.NewHandler(backing, relay.SinglePool(testWorkerToken), nil, nil, nil))
	t.Cleanup(ts.Close)

	claimed := time.Now()
	held := &run.Run{
		ID: "run_cap", Playbook: "site.yml", Status: run.StatusRunning, CreatedAt: claimed,
		ClaimedBy: "worker-a", ClaimedAt: &claimed,
	}
	if err := backing.Save(ctx, held); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	n := relay.MaxRelayElementsForTest()
	// One element past the cap, with trailing garbage a post-decode path would choke on first.
	over := []byte("[" + strings.Repeat("{},", n) + "%%%]")
	if code := postAsWorker(t, ts.URL, "/relay/v1/runs/run_cap/events", over); code != 413 {
		t.Errorf("an over-cap events body answered %d, want 413", code)
	}
	// A legitimate small batch still lands.
	if code := postAsWorker(t, ts.URL, "/relay/v1/runs/run_cap/events",
		[]byte(`[{"type":"runner_ok","host":"web01","task":"ping"}]`)); code != 204 {
		t.Errorf("a small events batch answered %d, want 204", code)
	}
}

// TestRelayRecordsTheProvenPoolAndActorType checks a relay audit entry names the pool the token
// resolved to, not only the lease name the worker asserted, and classifies the entry as a service
// actor. Every worker in a pool shares one token, so the pool is the only identity the server can
// prove, and an empty actor type left relay entries indistinguishable from unattributed ones.
func TestRelayRecordsTheProvenPoolAndActorType(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	backing := run.NewMemStore()
	audits := audit.NewMemStore()
	ts := httptest.NewServer(relay.NewHandler(backing, relay.SinglePool(testWorkerToken), nil, nil, audits))
	t.Cleanup(ts.Close)

	if err := backing.Save(ctx, &run.Run{
		ID: "run_rec", Playbook: "site.yml", Status: run.StatusPending, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	// A worker asserts a self-chosen owner label. The pool is what the token proves.
	claim := []byte(`{"owner":"attacker-label","queues":[""]}`)
	if code := postAsWorker(t, ts.URL, "/relay/v1/claim", claim); code != 200 {
		t.Fatalf("claim answered %d, want 200", code)
	}

	entries, err := audits.List(ctx, 10)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	var claimEntry *audit.Entry
	for _, e := range entries {
		if strings.Contains(e.Path, "/relay/claim/") {
			claimEntry = e
			break
		}
	}
	if claimEntry == nil {
		t.Fatal("no relay claim entry was recorded")
	}
	if diff := cmp.Diff("pool:default worker:attacker-label", claimEntry.Actor); diff != "" {
		t.Errorf("recorded actor mismatch (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff("service", claimEntry.ActorType); diff != "" {
		t.Errorf("recorded actor type mismatch (-want +got):\n%s", diff)
	}
}
