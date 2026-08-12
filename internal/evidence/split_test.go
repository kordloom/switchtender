package evidence

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/run"
)

// pack is one written register, its period read back from the file name and its rows from the
// document, which is the only progress record the emitter keeps.
type pack struct {
	// From and To are the period the name claims the document covers.
	From, To time.Time
	// IDs are the run ids the document lists.
	IDs []string
	// Doc is the rendered document.
	Doc string
}

// readArchive returns every pack in dir, oldest period first.
func readArchive(t *testing.T, dir string) []pack {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir() error = %v", err)
	}
	var packs []pack
	for _, entry := range entries {
		from, to, ok := parsePeriod(entry.Name())
		if !ok {
			t.Fatalf("archive holds %q, which is not a pack", entry.Name())
		}
		body, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatalf("ReadFile() error = %v", err)
		}
		doc := string(body)
		p := pack{From: from, To: to, Doc: doc}
		for _, part := range strings.Split(doc, `<td class="mono">`)[1:] {
			id, _, cut := strings.Cut(part, "</td>")
			if cut && strings.HasPrefix(id, "run_") {
				p.IDs = append(p.IDs, id)
			}
		}
		packs = append(packs, p)
	}
	sort.Slice(packs, func(i, j int) bool { return packs[i].From.Before(packs[j].From) })
	return packs
}

// seedSpread stores one run per offset from base, named run_0 upward in the order given.
func seedSpread(t *testing.T, base time.Time, offsets ...time.Duration) (run.Store, audit.Store) {
	t.Helper()
	ctx := context.Background()
	runs := run.NewMemStore()
	for i, off := range offsets {
		r := &run.Run{ID: fmt.Sprintf("run_%d", i), Playbook: "site.yml",
			Status: run.StatusSucceeded, Actor: "deploy-bot", CreatedAt: base.Add(off)}
		if err := runs.Save(ctx, r); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
	}
	return runs, audit.NewMemStore()
}

func TestATruncatedPeriodIsSplitSoTheArchiveHasNoHole(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	hour := time.Hour
	// Five changes in the first period and three in the second, against a register that carries
	// two. Progress is read out of pack names, so a pack named for a whole period it only partly
	// covers moves the archive past the rest of that period forever.
	runs, audits := seedSpread(t, base, 0, hour, 2*hour, 3*hour, 4*hour, 5*hour, 6*hour, 7*hour)
	dir := t.TempDir()
	e := NewEmitter(runs, audits, dir, hour, nil, WithMaxChanges(2))
	defer e.Close()

	ctx := context.Background()
	first, second := base.Add(-hour), base.Add(5*hour)
	end := base.Add(11 * hour)
	if err := e.Emit(ctx, first, second); err != nil {
		t.Fatalf("Emit(period one) error = %v", err)
	}
	// The second period starts where the archive says the first ended, which is the emitter's own
	// resume path and the thing a truncated pack name would have corrupted.
	resumed, err := e.resume(end)
	if err != nil {
		t.Fatalf("resume() error = %v", err)
	}
	if !resumed.Equal(second.UTC()) {
		t.Fatalf("resumed at %s, want the end of the first period %s", resumed, second.UTC())
	}
	if err := e.Emit(ctx, resumed, end); err != nil {
		t.Fatalf("Emit(period two) error = %v", err)
	}

	packs := readArchive(t, dir)
	if len(packs) < 2 {
		t.Fatalf("archive holds %d packs, want the period split into several", len(packs))
	}
	// Every instant between the first period's start and the second's end is covered by exactly
	// one pack, with no gap between consecutive names.
	if !packs[0].From.Equal(first.UTC()) {
		t.Errorf("archive starts at %s, want %s", packs[0].From, first.UTC())
	}
	if got := packs[len(packs)-1].To; !got.Equal(end.UTC()) {
		t.Errorf("archive ends at %s, want %s", got, end.UTC())
	}
	for i := 1; i < len(packs); i++ {
		if !packs[i].From.Equal(packs[i-1].To) {
			t.Errorf("pack %d starts at %s but the one before it ended at %s, so the archive has "+
				"a hole", i, packs[i].From, packs[i-1].To)
		}
	}

	var got []string
	for _, p := range packs {
		got = append(got, p.IDs...)
	}
	want := []string{"run_0", "run_1", "run_2", "run_3", "run_4", "run_5", "run_6", "run_7"}
	if diff := cmp.Diff(want, got, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("changes across the archive, in order, once each (-want +got):\n%s", diff)
	}

	// The pack that was cut short says so, and names the instant the next pack picks up at, so a
	// reader holding one document knows it is a part and knows where the rest is.
	if !strings.Contains(packs[0].Doc, "This register is truncated") {
		t.Error("the first pack was cut short and does not say so")
	}
	if !strings.Contains(packs[0].Doc, packs[0].To.Format(time.RFC3339)) {
		t.Errorf("the first pack does not name %s, where the next pack starts", packs[0].To)
	}
}
