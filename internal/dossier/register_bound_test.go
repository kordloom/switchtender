package dossier

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/run"
)

// spyRuns records the page size each register asks the store for and serves the wrapped store's
// answer. It exists because a cap spent on the rows after they arrive renders the same document as
// one spent in the query, and only the second bounds the work the store does.
type spyRuns struct {
	run.Store
	// mu guards limits.
	mu sync.Mutex
	// limits are the page sizes requested, in order.
	limits []int
}

// ListPage records the requested limit and delegates to the wrapped store.
func (s *spyRuns) ListPage(ctx context.Context, f run.ListFilter, limit, offset int) ([]*run.Run, error) {
	s.mu.Lock()
	s.limits = append(s.limits, limit)
	s.mu.Unlock()
	return s.Store.ListPage(ctx, f, limit, offset)
}

// Limits returns the page sizes requested so far.
func (s *spyRuns) Limits() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int(nil), s.limits...)
}

// seedOffsets stores one succeeded run per offset from base, named run_0 upward in the order given.
func seedOffsets(t *testing.T, base time.Time, offsets []time.Duration) *spyRuns {
	t.Helper()
	ctx := context.Background()
	store := run.NewMemStore()
	for i, off := range offsets {
		r := &run.Run{ID: fmt.Sprintf("run_%d", i), Playbook: "site.yml",
			Status: run.StatusSucceeded, Actor: "deploy-bot", CreatedAt: base.Add(off)}
		if err := store.Save(ctx, r); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
	}
	return &spyRuns{Store: store}
}

// rowIDs returns the run ids the rendered register lists, in order.
func rowIDs(html string) []string {
	var out []string
	for _, part := range strings.Split(html, `<td class="mono">`)[1:] {
		id, _, ok := strings.Cut(part, "</td>")
		if ok && strings.HasPrefix(id, "run_") {
			out = append(out, id)
		}
	}
	return out
}

func TestRegisterBoundsThePageInTheStoreQuery(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	hour := time.Hour
	tests := []struct {
		Name          string
		Seed          []time.Duration
		WantRows      []string
		WantLimits    []int
		Limit         int
		WantCovered   time.Duration
		WantTruncated bool
	}{{ // Test 0: The cap is spent in the query, one row past it, and the rest never arrives.
		Name: "over the cap", Seed: []time.Duration{0, hour, 2 * hour, 3 * hour, 4 * hour},
		Limit: 2, WantLimits: []int{3}, WantRows: []string{"run_0", "run_1"},
		WantTruncated: true, WantCovered: 2 * hour,
	}, { // Test 1: A period ending exactly on the cap is whole, and says nothing about truncation.
		Name: "exactly the cap", Seed: []time.Duration{0, hour, 2 * hour},
		Limit: 3, WantLimits: []int{4}, WantRows: []string{"run_0", "run_1", "run_2"},
	}, { // Test 2: No caller-named cap still bounds the query, at the package default.
		Name: "default cap", Seed: []time.Duration{0, hour},
		Limit: 0, WantLimits: []int{MaxRegisterRuns + 1}, WantRows: []string{"run_0", "run_1"},
	}, { // Test 3: The cut falls between changes sharing an instant, so the tail is left whole for
		// the register that resumes there rather than half shown here.
		Name: "cut through a shared instant", Seed: []time.Duration{0, hour, hour, 2 * hour},
		Limit: 2, WantLimits: []int{3}, WantRows: []string{"run_0"},
		WantTruncated: true, WantCovered: hour,
	}, { // Test 4: A whole page at one instant has no clean cut, so the page stays and the register
		// resuming at that instant repeats it. Repeating reconciles; dropping loses.
		Name: "a whole page at one instant", Seed: []time.Duration{0, 0, 0, hour},
		Limit: 2, WantLimits: []int{3}, WantRows: []string{"run_0", "run_1"},
		WantTruncated: true, WantCovered: 0,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			runs := seedOffsets(t, base, test.Seed)
			audits := audit.NewMemStore()
			from, to := base.Add(-time.Hour), base.Add(24*time.Hour)
			in, err := CollectRegister(context.Background(), runs, audits, "", from, to, to, test.Limit)
			if err != nil {
				t.Fatalf("CollectRegister() error = %v", err)
			}
			if diff := cmp.Diff(test.WantLimits, runs.Limits(), cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("page sizes asked of the store (-want +got):\n%s", diff)
			}
			var gotIDs []string
			for _, r := range in.Runs {
				gotIDs = append(gotIDs, r.ID)
			}
			if diff := cmp.Diff(test.WantRows, gotIDs, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("collected runs (-want +got):\n%s", diff)
			}
			if in.Truncated != test.WantTruncated {
				t.Errorf("Truncated = %v, want %v", in.Truncated, test.WantTruncated)
			}
			wantCovered := time.Time{}
			if test.WantTruncated {
				wantCovered = base.Add(test.WantCovered)
			}
			if !in.CoveredTo.Equal(wantCovered) {
				t.Errorf("CoveredTo = %v, want %v", in.CoveredTo, wantCovered)
			}

			doc, err := RenderRegister(in)
			if err != nil {
				t.Fatalf("RenderRegister() error = %v", err)
			}
			html := string(doc)
			if diff := cmp.Diff(test.WantRows, rowIDs(html), cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("rendered rows (-want +got):\n%s", diff)
			}
			notice := strings.Contains(html, "This register is truncated")
			if notice != test.WantTruncated {
				t.Fatalf("truncation notice in the document = %v, want %v", notice, test.WantTruncated)
			}
			if !test.WantTruncated {
				return
			}
			// The notice has to name the bound and the instant coverage stops at, because a reader
			// told only that something is missing cannot tell where to look for the rest.
			for _, want := range []string{
				fmt.Sprintf("at most %d changes", test.Limit),
				fmt.Sprintf("carries the earliest\n\t%d and stops at", len(test.WantRows)),
				wantCovered.UTC().Format(time.RFC3339),
				`<p class="k">Changes shown</p>`,
			} {
				if !strings.Contains(html, want) {
					t.Errorf("the truncation notice does not carry %q", want)
				}
			}
		})
	}
}
