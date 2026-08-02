package server

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/live"
	"github.com/kordloom/switchtender/internal/run"
)

// countingStore counts reads so a spinning stream is measurable rather than inferred.
type countingStore struct {
	run.Store
	gets atomic.Int64
}

func (c *countingStore) Get(ctx context.Context, id string) (*run.Run, error) {
	c.gets.Add(1)
	return c.Store.Get(ctx, id)
}

// TestClosedHubDoesNotSpinTheStream pins that ending a run's hub topic ends its stream instead of
// making it query in a loop.
//
// Closing the subscriber channel made it always ready to receive, so a handler that received
// without checking stopped waiting and re-read the store as fast as it could: tens of thousands of
// statements a second per connected stream. The trigger is a run whose stream is closed while the
// run is not yet terminal, which happens when a store read fails during startup or finalize, so the
// spin arrived exactly when the store was already struggling and fed on itself.
func TestClosedHubDoesNotSpinTheStream(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := &countingStore{Store: run.NewMemStore()}
	// Left running on purpose: a terminal run ends the stream on its own and hides the defect.
	if err := store.Save(ctx, &run.Run{
		ID: "run_live", Playbook: "site.yml", Status: run.StatusRunning, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	hub := live.NewHub()
	srv := httptest.NewServer(New(store, &fakeSubmitter{}, zap.NewNop(),
		WithStreamer(hub)).Handler())
	t.Cleanup(srv.Close)

	res, err := http.Get(srv.URL + "/v1/runs/run_live/stream")
	if err != nil {
		t.Fatalf("GET stream: %v", err)
	}
	defer func() { _ = res.Body.Close() }()
	reader := bufio.NewReader(res.Body)

	// Let the stream settle into its idle poll.
	time.Sleep(200 * time.Millisecond)
	hub.CloseRun("run_live")

	// Read until the stream ends, bounded so a spin cannot hang the suite.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			if _, rerr := reader.ReadString('\n'); rerr != nil {
				return
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Error("the stream never ended after its hub topic closed")
	}

	before := store.gets.Load()
	time.Sleep(time.Second)
	spun := store.gets.Load() - before
	// A settled stream polls about once a second. Anything near a thousand is the spin.
	if spun > 20 {
		t.Errorf("the stream issued %d store reads in one second after its topic closed, so "+
			"closing a run's hub turns every connected stream into a loop against the store", spun)
	}
}
