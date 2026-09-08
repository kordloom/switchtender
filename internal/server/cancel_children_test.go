package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/run"
)

// splitParentWithChildren seeds a store with an unclaimed parent of the given kind and children in
// the given statuses, which is the shape a split or a pipeline sits in before anything claims it.
func splitParentWithChildren(t *testing.T, kind string, parentStatus run.Status,
	children ...run.Status) run.Store {
	t.Helper()
	store := run.NewMemStore()
	ctx := context.Background()
	if err := store.Save(ctx, &run.Run{
		ID: "run_parent", Kind: kind, Status: parentStatus, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	parent := "run_parent"
	for i, status := range children {
		idx := i
		if err := store.Save(ctx, &run.Run{
			ID:         "run_child_" + string(rune('a'+i)),
			ParentID:   &parent,
			ShardIndex: &idx,
			Status:     status,
			CreatedAt:  time.Now(),
		}); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
	}
	return store
}

// TestCancelingAParentSettlesItsChildren covers the cascade that had no test at all.
//
// A split or pipeline parent stores its children beside it. Canceling the parent while it is still
// unclaimed terminalizes the parent in one statement, and nothing else would ever look at the
// children: orphan resolution only fires for an interrupted parent, and a canceled one is already
// terminal. Without the cascade the children sit in pending_approval forever, and approving one runs
// it on real hosts under a parent the operator canceled, with no parent left to roll it up.
//
// This is the "I canceled it and it ran anyway" failure, and it is only visible from the children's
// stored status, never from the cancel response, which returns 202 either way.
func TestCancelingAParentSettlesItsChildren(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name     string
		Kind     string
		Children []run.Status
		// WantCanceled lists the children that must end terminal, by index.
		WantCanceled []int
	}{
		{
			Name: "a split whose shards await approval", Kind: run.KindSplit,
			Children:     []run.Status{run.StatusPendingApproval, run.StatusPendingApproval},
			WantCanceled: []int{0, 1},
		},
		{
			Name: "a pipeline whose steps are queued", Kind: run.KindPipeline,
			Children:     []run.Status{run.StatusPending, run.StatusPending},
			WantCanceled: []int{0, 1},
		},
		{
			Name: "a split with a child that already finished", Kind: run.KindSplit,
			Children:     []run.Status{run.StatusSucceeded, run.StatusPending},
			WantCanceled: []int{1},
		},
	}
	for _, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			store := splitParentWithChildren(t, test.Kind, run.StatusPendingApproval, test.Children...)
			handler := New(store, &fakeSubmitter{}, zap.NewNop()).Handler()
			rec := httptest.NewRecorder()

			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/runs/run_parent/cancel", nil))

			if rec.Code != http.StatusAccepted {
				t.Fatalf("status = %d, want 202: %s", rec.Code, rec.Body.String())
			}
			ctx := context.Background()
			parent, err := store.Get(ctx, "run_parent")
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			if parent.Status != run.StatusCanceled {
				t.Errorf("parent status = %q, want canceled", parent.Status)
			}
			children, err := store.Shards(ctx, "run_parent")
			if err != nil {
				t.Fatalf("Shards() error = %v", err)
			}
			if len(children) != len(test.Children) {
				t.Fatalf("children = %d, want %d", len(children), len(test.Children))
			}
			want := make(map[int]bool, len(test.WantCanceled))
			for _, i := range test.WantCanceled {
				want[i] = true
			}
			for i, c := range children {
				switch {
				case want[i] && c.Status != run.StatusCanceled:
					t.Errorf("child %d status = %q, want canceled: it is still claimable and an "+
						"approval would run it under a canceled parent", i, c.Status)
				case !want[i] && c.Status != test.Children[i]:
					t.Errorf("child %d status = %q, want it left at %q", i, c.Status, test.Children[i])
				}
			}
		})
	}
}

// TestCancelingAParentRequestsCancelOnAClaimedChild covers the other half of the cascade. A child an
// executor already holds cannot be terminalized behind that executor's back, so it has to be asked
// to stop instead. Without the flag the shard keeps running on real hosts after the cancel returned.
func TestCancelingAParentRequestsCancelOnAClaimedChild(t *testing.T) {
	t.Parallel()
	store := run.NewMemStore()
	ctx := context.Background()
	parent := "run_parent"
	if err := store.Save(ctx, &run.Run{
		ID: parent, Kind: run.KindSplit, Status: run.StatusPendingApproval, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	idx := 0
	if err := store.Save(ctx, &run.Run{
		ID: "run_child_a", ParentID: &parent, ShardIndex: &idx, Status: run.StatusRunning,
		ClaimedBy: "worker-1", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	handler := New(store, &fakeSubmitter{}, zap.NewNop()).Handler()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/runs/run_parent/cancel", nil))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", rec.Code, rec.Body.String())
	}
	child, err := store.Get(ctx, "run_child_a")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if !child.CancelRequested {
		t.Error("the claimed child was not asked to stop, so it keeps running after the cancel")
	}
	if child.Status == run.StatusCanceled {
		t.Error("the claimed child was terminalized behind its executor, which loses its result")
	}
}

// TestCancelingAPlainRunTouchesNoChildren checks the cascade is scoped to parents. A plain run is
// not a parent, so the handler must not go looking for children, and a store that would fail the
// lookup must not turn a good cancel into an error.
func TestCancelingAPlainRunTouchesNoChildren(t *testing.T) {
	t.Parallel()
	store := &shardCountingStore{Store: run.NewMemStore()}
	if err := store.Save(context.Background(), &run.Run{
		ID: "run_plain", Status: run.StatusPending, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	handler := New(store, &fakeSubmitter{}, zap.NewNop()).Handler()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/runs/run_plain/cancel", nil))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", rec.Code, rec.Body.String())
	}
	if store.shardCalls != 0 {
		t.Errorf("Shards() called %d times for a plain run, want 0", store.shardCalls)
	}
}

// TestCancelingAParentSurvivesAChildThatWillNotSettle checks a store failure on the children does not
// fail the cancel. The parent is what the caller asked to stop and it is already stopped, so a child
// that cannot be settled is logged rather than turned into a 500 that suggests nothing happened.
func TestCancelingAParentSurvivesAChildThatWillNotSettle(t *testing.T) {
	t.Parallel()
	store := &shardCountingStore{Store: run.NewMemStore(), shardErr: errors.New("store is down")}
	ctx := context.Background()
	if err := store.Save(ctx, &run.Run{
		ID: "run_parent", Kind: run.KindSplit, Status: run.StatusPending, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	handler := New(store, &fakeSubmitter{}, zap.NewNop()).Handler()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/runs/run_parent/cancel", nil))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 even when the children cannot be listed: %s",
			rec.Code, rec.Body.String())
	}
	parent, err := store.Get(ctx, "run_parent")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if parent.Status != run.StatusCanceled {
		t.Errorf("parent status = %q, want canceled", parent.Status)
	}
}

// shardCountingStore wraps a run store to count Shards calls and optionally fail them.
type shardCountingStore struct {
	// Store is the wrapped store every other call passes through to.
	run.Store
	// shardCalls counts how many times Shards was asked for.
	shardCalls int
	// shardErr is returned from Shards when set.
	shardErr error
}

// Shards records the call and returns the configured error, or the wrapped store's answer.
func (s *shardCountingStore) Shards(ctx context.Context, parentID string) ([]*run.Run, error) {
	s.shardCalls++
	if s.shardErr != nil {
		return nil, s.shardErr
	}
	return s.Store.Shards(ctx, parentID)
}
