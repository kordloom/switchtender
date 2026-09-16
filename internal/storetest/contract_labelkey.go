package storetest

import (
	"context"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/run"
)

// testLabelKeyOnlyMatchesAnyValue holds every backend to matching on a label key alone.
//
// Filtering used to require both halves of a pair, which meant a caller had to already know a value
// to find anything. Nobody can open a list of changes if they must name a change first, so the
// browsing surface for them was impossible to build without this.
//
// The three backends do it three different ways: a json_each subquery, a jsonb function, and a map
// lookup. They have to agree, or the same install answers differently on Postgres than on SQLite.
func testLabelKeyOnlyMatchesAnyValue(t *testing.T, store run.Store) {
	t.Helper()
	ctx := context.Background()
	base := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

	seed := []struct {
		id     string
		labels map[string]string
	}{
		{"run_a", map[string]string{"change": "OPS-1", "env": "prod"}},
		{"run_b", map[string]string{"change": "OPS-2"}},
		{"run_c", map[string]string{"env": "prod"}},
		{"run_d", nil},
	}
	for i, s := range seed {
		if err := store.Save(ctx, &run.Run{ID: s.id, Playbook: "site.yml",
			Status: run.StatusSucceeded, CreatedAt: base.Add(time.Duration(i) * time.Minute),
			Labels: s.labels}); err != nil {
			t.Fatalf("save %s: %v", s.id, err)
		}
	}

	// The key alone: every run carrying it, whatever the value, and nothing else.
	got, err := store.ListPage(ctx, run.ListFilter{LabelKey: "change"}, 100, 0)
	if err != nil {
		t.Fatalf("ListPage(key only) error = %v", err)
	}
	ids := map[string]bool{}
	for _, r := range got {
		ids[r.ID] = true
	}
	if len(got) != 2 || !ids["run_a"] || !ids["run_b"] {
		t.Fatalf("key-only filter returned %d runs (%v), want run_a and run_b. A caller cannot "+
			"discover which values exist without this", len(got), ids)
	}

	// The pair still narrows to one, so adding a value did not stop meaning what it meant.
	got, err = store.ListPage(ctx, run.ListFilter{LabelKey: "change", LabelValue: "OPS-2"}, 100, 0)
	if err != nil {
		t.Fatalf("ListPage(pair) error = %v", err)
	}
	if len(got) != 1 || got[0].ID != "run_b" {
		t.Errorf("pair filter returned %d runs, want only run_b", len(got))
	}

	// A key nothing carries matches nothing rather than everything, which is the direction a
	// filter must fail in.
	got, err = store.ListPage(ctx, run.ListFilter{LabelKey: "nosuchkey"}, 100, 0)
	if err != nil {
		t.Fatalf("ListPage(absent key) error = %v", err)
	}
	if len(got) != 0 {
		t.Errorf("an unknown label key matched %d runs, want none", len(got))
	}
}
