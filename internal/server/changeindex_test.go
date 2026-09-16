package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/run"
)

// TestTheChangeIndexListsWhatItCanSeeAndSaysWhatItCannot covers the browsing surface and, more
// importantly, its own honesty about its limits.
//
// No store can return distinct label values, so this is built by scanning recent runs. That means a
// change whose every run predates the scan cap is simply not listed. An index that quietly omits
// old changes while presenting itself as the list of changes is worse than no index, so the answer
// carries how many runs it read and whether it hit the cap.
func TestTheChangeIndexListsWhatItCanSeeAndSaysWhatItCannot(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := run.NewMemStore()
	base := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

	seed := []struct {
		id, change string
		status     run.Status
		mins       int
	}{
		{"run_a", "OPS-1", run.StatusFailed, 0},
		{"run_b", "OPS-1", run.StatusSucceeded, 1},
		{"run_c", "OPS-2", run.StatusSucceeded, 2},
		{"run_d", "", run.StatusSucceeded, 3},
	}
	for _, s := range seed {
		r := &run.Run{ID: s.id, Playbook: "site.yml", Status: s.status,
			CreatedAt: base.Add(time.Duration(s.mins) * time.Minute)}
		if s.change != "" {
			r.Labels = map[string]string{run.ChangeLabel: s.change}
		}
		ended := r.CreatedAt.Add(time.Minute)
		r.EndedAt = &ended
		if err := store.Save(ctx, r); err != nil {
			t.Fatalf("save %s: %v", s.id, err)
		}
	}

	rec := httptest.NewRecorder()
	changesHandler(store, nil, zap.NewNop()).ServeHTTP(rec,
		httptest.NewRequest(http.MethodGet, "/v1/changes", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got changeListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if got.Total != 2 {
		t.Fatalf("index holds %d changes, want 2. A run carrying no change label must not become "+
			"one: %+v", got.Total, got.Changes)
	}
	byName := map[string]changeResponse{}
	for _, c := range got.Changes {
		byName[c.Change] = c
	}
	// The mixed outcome survives into the index, which is the shape somebody is scanning for.
	if byName["OPS-1"].Outcome != changeMixed {
		t.Errorf("OPS-1 outcome = %q, want %q", byName["OPS-1"].Outcome, changeMixed)
	}
	if byName["OPS-1"].Total != 2 {
		t.Errorf("OPS-1 holds %d runs, want 2", byName["OPS-1"].Total)
	}
	// The members are not carried, or an index of a year's changes is the whole history.
	for _, c := range got.Changes {
		if len(c.Runs) != 0 {
			t.Errorf("change %s carries its member runs in the index", c.Change)
		}
	}
	// It says what it read, so a reader can tell a complete answer from a partial one.
	if got.Scanned == 0 {
		t.Error("the index does not report how many runs it read, so its completeness is invisible")
	}
	if got.Partial {
		t.Error("a four-run store reported a capped scan")
	}
}
