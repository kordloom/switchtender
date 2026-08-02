package run

import (
	"context"
	"sync"
	"testing"
	"time"
)

// Runs are left non-terminal on purpose: SaveHostSummary fences a terminal run and returns without
// writing, so seeding them as succeeded makes the writer a no-op and the test proves nothing.
//
// TestListPageDoesNotRaceWithSummaryWrites pins that reading a page while summaries are written is
// safe. The helper's comment claimed the caller held the read lock through List; List takes and
// releases it, so nothing was held, and a concurrent write made it a fatal concurrent map access.
func TestListPageDoesNotRaceWithSummaryWrites(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemStore()
	for i := range 20 {
		if err := store.Save(ctx, &Run{
			ID: "run_" + string(rune('a'+i)), Playbook: "site.yml",
			Status: StatusRunning, CreatedAt: time.Now(),
		}); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
	}
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			_ = store.SaveHostSummary(ctx, "run_a", []HostSummary{{Host: "web01", OK: i}})
		}
	}()
	for range 400 {
		if _, err := store.ListPage(ctx, ListFilter{Host: "web01"}, 10, 0); err != nil {
			t.Fatalf("ListPage() error = %v", err)
		}
	}
	close(stop)
	wg.Wait()
}
