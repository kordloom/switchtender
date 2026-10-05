package relay

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/run"
)

// presenceCall is one call the relay made to record a worker.
type presenceCall struct {
	// Kind is note or touch.
	Kind string
	// Owner is the worker.
	Owner string
	// Queues are the queues a note recorded.
	Queues []string
	// Slots is the pool size a note recorded.
	Slots int
}

// presenceSpy records every call the relay makes to record its workers.
type presenceSpy struct {
	// mu guards calls.
	mu sync.Mutex
	// calls are the calls, in order.
	calls []presenceCall
}

// NoteWorker records a note.
func (p *presenceSpy) NoteWorker(_ context.Context, owner string, queues []string, slots int) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, presenceCall{Kind: "note", Owner: owner, Queues: queues, Slots: slots})
	return nil
}

// TouchWorker records a touch.
func (p *presenceSpy) TouchWorker(_ context.Context, owner string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, presenceCall{Kind: "touch", Owner: owner})
	return nil
}

// recorded returns the calls so far.
func (p *presenceSpy) recorded() []presenceCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]presenceCall(nil), p.calls...)
}

// TestRelayRecordsItsWorkers pins that the control node records each relay worker it hears from: a
// claim records the queues it serves and the slots it sent, whether or not anything was pending, a
// heartbeat keeps it fresh while it is too busy to claim, and a claim its pool may not make records
// nothing, since that worker serves none of what it asked for.
func TestRelayRecordsItsWorkers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	poolFile := filepath.Join(dir, "pools.yml")
	doc := fmt.Sprintf("workers:\n  - name: dmz\n    token_sha256: %s\n    queues: [dmz]\n",
		HashToken("dmz-token"))
	if err := os.WriteFile(poolFile, []byte(doc), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	pools, err := LoadPools(poolFile)
	if err != nil {
		t.Fatalf("LoadPools() error = %v", err)
	}
	backing := run.NewMemStore()
	if err := backing.Save(ctx, &run.Run{ID: "run_dmz", Playbook: "site.yml", Queue: "dmz",
		Status: run.StatusPending, CreatedAt: time.Now()}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	spy := &presenceSpy{}
	ts := httptest.NewServer(NewHandler(backing, pools, nil, nil, nil, WithPresence(spy),
		presenceEvery(0)))
	t.Cleanup(ts.Close)
	c := NewClient(NewHTTPTransport(ts.URL, "dmz-token", ts.Client()))
	c.SetClaimSlots(5)

	if _, err := c.Claim(ctx, "relay-dmz", []string{"prod"}); err == nil {
		t.Fatal("a claim on a queue the pool does not serve succeeded")
	}
	claimed, err := c.Claim(ctx, "relay-dmz", []string{"dmz"})
	if err != nil {
		t.Fatalf("Claim() error = %v", err)
	}
	if err := c.Heartbeat(ctx, claimed.ID, "relay-dmz"); err != nil {
		t.Fatalf("Heartbeat() error = %v", err)
	}
	if _, err := c.Claim(ctx, "relay-dmz", []string{"dmz"}); !errors.Is(err, run.ErrNonePending) {
		t.Fatalf("Claim() with nothing pending error = %v, want ErrNonePending", err)
	}
	want := []presenceCall{
		{Kind: "note", Owner: "relay-dmz", Queues: []string{"dmz"}, Slots: 5},
		{Kind: "touch", Owner: "relay-dmz"},
		{Kind: "note", Owner: "relay-dmz", Queues: []string{"dmz"}, Slots: 5},
	}
	if diff := cmp.Diff(want, spy.recorded(), cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("recorded calls mismatch (-want +got):\n%s", diff)
	}
}

// TestRelayPresenceIsThrottled pins that a worker polling faster than the interval is recorded once
// per interval, since its claim loop polls several times a second when idle.
func TestRelayPresenceIsThrottled(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tests := []struct {
		Every     time.Duration
		WantCalls int
	}{{ // Test 0: Within the interval, one report is recorded however many arrive.
		Every: time.Hour, WantCalls: 1,
	}, { // Test 1: With no interval, every report is recorded.
		Every: 0, WantCalls: 3,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			spy := &presenceSpy{}
			p := &presenceNotes{rec: spy, last: map[string]time.Time{}, every: test.Every}
			for range 3 {
				p.note(ctx, "relay-a", []string{""}, 2)
			}
			if diff := cmp.Diff(test.WantCalls, len(spy.recorded())); diff != "" {
				t.Errorf("recorded calls mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestRelayPresenceIsOptional pins that a relay with nothing to record presence in serves claims as
// it always did.
func TestRelayPresenceIsOptional(t *testing.T) {
	t.Parallel()
	var p *presenceNotes
	p.note(context.Background(), "relay-a", nil, 1)
	p.touch(context.Background(), "relay-a")
}
