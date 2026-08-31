package evidence

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/run"
)

// seedPeriod stores one run inside the period and one long before it.
func seedPeriod(t *testing.T) (run.Store, audit.Store, time.Time) {
	t.Helper()
	ctx := context.Background()
	runs := run.NewMemStore()
	audits := audit.NewMemStore()
	base := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	for _, r := range []*run.Run{
		{ID: "run_in", Playbook: "site.yml", Status: run.StatusSucceeded, Actor: "deploy-bot",
			CreatedAt: base, HeldByPolicy: "prod terraform destroy"},
		{ID: "run_out", Playbook: "old.yml", Status: run.StatusSucceeded,
			CreatedAt: base.Add(-60 * 24 * time.Hour)},
	} {
		if err := runs.Save(ctx, r); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
	}
	if err := audits.Append(ctx, &audit.Entry{
		ID: audit.NewID(), At: base, Actor: "root", Method: "POST", Path: "/v1/runs/run_in/approve",
	}); err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	return runs, audits, base
}

func TestEmitWritesThePeriodPack(t *testing.T) {
	t.Parallel()
	runs, audits, base := seedPeriod(t)
	dir := t.TempDir()
	var gotPath string
	e := NewEmitter(runs, audits, "", dir, time.Hour, nil,
		WithNotify(func(p string, _, _ time.Time) { gotPath = p }))
	defer e.Close()

	from, to := base.Add(-time.Hour), base.Add(24*time.Hour)
	if err := e.Emit(context.Background(), from, to); err != nil {
		t.Fatalf("Emit() error = %v", err)
	}
	want := filepath.Join(dir, packName(from, to))
	if gotPath != want {
		t.Errorf("notified path = %q, want %q", gotPath, want)
	}
	body, err := os.ReadFile(want)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	html := string(body)
	if !strings.Contains(html, "run_in") {
		t.Error("the pack omits a change inside the period")
	}
	if strings.Contains(html, "run_out") {
		t.Error("the pack carries a change from outside the period")
	}
	if !strings.Contains(html, "prod terraform destroy") {
		t.Error("the pack does not name the rule that held the change")
	}
}

func TestTwoPeriodsInOneDayDoNotShareAFile(t *testing.T) {
	t.Parallel()
	runs, audits, base := seedPeriod(t)
	dir := t.TempDir()
	e := NewEmitter(runs, audits, "", dir, time.Hour, nil)
	defer e.Close()

	// A cadence shorter than a day is allowed, so a name at day granularity would make the second
	// period silently replace the first and leave an archive that looks complete and is not.
	for _, p := range [][2]time.Time{
		{base, base.Add(6 * time.Hour)},
		{base.Add(6 * time.Hour), base.Add(12 * time.Hour)},
		{base.Add(12 * time.Hour), base.Add(18 * time.Hour)},
	} {
		if err := e.Emit(context.Background(), p[0], p[1]); err != nil {
			t.Fatalf("Emit() error = %v", err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir() error = %v", err)
	}
	if len(entries) != 3 {
		t.Errorf("archive holds %d packs, want 3: a period overwrote another", len(entries))
	}
}

func TestProgressResumesFromTheArchiveNotFromMemory(t *testing.T) {
	t.Parallel()
	runs, audits, base := seedPeriod(t)
	dir := t.TempDir()
	e := NewEmitter(runs, audits, "", dir, time.Hour, nil)
	defer e.Close()

	// A pack already in the archive is where the next period starts. Keeping this only in memory
	// meant a server restarted more often than its cadence never emitted at all.
	from, to := base, base.Add(time.Hour)
	if err := e.Emit(context.Background(), from, to); err != nil {
		t.Fatalf("Emit() error = %v", err)
	}
	resumed, err := e.resume(base.Add(48 * time.Hour))
	if err != nil {
		t.Fatalf("resume() error = %v", err)
	}
	if !resumed.Equal(to.UTC()) {
		t.Errorf("resumed at %s, want the end of the newest pack %s", resumed, to.UTC())
	}

	// An empty archive starts the first period now rather than at the epoch.
	fresh := NewEmitter(runs, audits, "", t.TempDir(), time.Hour, nil)
	defer fresh.Close()
	now := base.Add(72 * time.Hour)
	if got, err := fresh.resume(now); err != nil || !got.Equal(now) {
		t.Errorf("resume(empty) = %s, %v, want %s", got, err, now)
	}
}

func TestAFailedPeriodIsCoveredByTheNextAttempt(t *testing.T) {
	t.Parallel()
	runs, audits, base := seedPeriod(t)
	dir := t.TempDir()
	e := NewEmitter(runs, audits, "", dir, time.Hour, nil)
	defer e.Close()

	// One pack lands, then two cadences pass with nothing written. Because progress lives in the
	// archive, the next attempt covers the whole unwritten span rather than only the last cadence.
	if err := e.Emit(context.Background(), base, base.Add(time.Hour)); err != nil {
		t.Fatalf("Emit() error = %v", err)
	}
	clock := base.Add(4 * time.Hour)
	last, err := e.resume(clock)
	if err != nil {
		t.Fatalf("resume() error = %v", err)
	}
	if clock.Sub(last) != 3*time.Hour {
		t.Errorf("next period spans %s, want the 3h since the last pack so nothing is skipped",
			clock.Sub(last))
	}
}

func TestEmitNamesTheDirectoryItCouldNotUse(t *testing.T) {
	t.Parallel()
	runs, audits, base := seedPeriod(t)
	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	e := NewEmitter(runs, audits, "", filepath.Join(file, "packs"), time.Hour, nil)
	defer e.Close()
	err := e.Emit(context.Background(), base, base.Add(time.Hour))
	if err == nil {
		t.Fatal("Emit() reported success while writing nothing")
	}
	if !strings.Contains(err.Error(), "evidence directory") {
		t.Errorf("error = %q, want it to name the directory that could not be made", err)
	}
}

func TestStartRefusesAnUnusableDirectoryImmediately(t *testing.T) {
	t.Parallel()
	runs, audits, _ := seedPeriod(t)
	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	// Checked at start rather than at the first tick, which with a quarterly cadence is three
	// months after the misconfiguration, with the startup log claiming an archive was accruing.
	e := NewEmitter(runs, audits, "", filepath.Join(file, "packs"), time.Hour, nil)
	defer e.Close()
	if err := e.Start(); err == nil {
		t.Fatal("Start() accepted a directory it cannot write to")
	}
}

func TestEmitOutlivesClose(t *testing.T) {
	t.Parallel()
	runs, audits, base := seedPeriod(t)
	e := NewEmitter(runs, audits, "", t.TempDir(), time.Hour, nil)
	e.Close()
	// A pack generated by hand is not the loop's, so a shutdown must not abort it.
	if err := e.Emit(context.Background(), base, base.Add(time.Hour)); err != nil {
		t.Errorf("Emit() after Close error = %v, want a pack to still be writable", err)
	}
}

func TestNewEmitterRefusesConfigurationThatCannotWork(t *testing.T) {
	t.Parallel()
	runs, audits, _ := seedPeriod(t)
	for _, tc := range []struct {
		Name    string
		Dir     string
		Cadence time.Duration
	}{
		{"cadence too short to cover a period", t.TempDir(), time.Minute},
		{"no directory to write into", "", time.Hour},
	} {
		t.Run(tc.Name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Errorf("NewEmitter accepted %s", tc.Name)
				}
			}()
			NewEmitter(runs, audits, "", tc.Dir, tc.Cadence, nil)
		})
	}
}

func TestPackNameRoundTripsItsPeriod(t *testing.T) {
	t.Parallel()
	from := time.Date(2026, 7, 1, 6, 30, 0, 0, time.UTC)
	to := from.Add(6 * time.Hour)
	gotFrom, gotTo, ok := parsePeriod(packName(from, to))
	if !ok || !gotFrom.Equal(from) || !gotTo.Equal(to) {
		t.Errorf("parsePeriod(packName(...)) = %s, %s, %v, want %s, %s, true",
			gotFrom, gotTo, ok, from, to)
	}
	if _, _, ok := parsePeriod("notes.txt"); ok {
		t.Error("parsePeriod accepted a file that is not a pack")
	}
}

// TestEmitterWritesItsFirstPackFromAnEmptyArchive pins the bootstrap, which nothing exercised.
//
// Every other test in this file calls Emit directly, which seeds the archive before the scheduler is
// ever consulted, so none of them crossed the path a real install takes. From a clean directory
// resume answered with the caller's own clock and emitDue compared that instant against itself, so
// the elapsed time was zero on every tick and the first pack was never due. The feature was inert for
// the life of the install while the server logged that periodic change registers were enabled: no
// error, no warning, and an empty evidence directory a year later, by which time retention may have
// trimmed the runs the packs would have covered.
func TestEmitterWritesItsFirstPackFromAnEmptyArchive(t *testing.T) {
	t.Parallel()
	runs, audits, base := seedPeriod(t)
	dir := t.TempDir()

	// The emitter's own loop reads the clock and reports packs from its goroutine while this test
	// drives emitDue from another, so both are guarded rather than raced.
	var mu sync.Mutex
	clock := base
	var written []string
	now := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return clock
	}
	advance := func(d time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		clock = base.Add(d)
	}
	count := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(written)
	}
	// Packs on disk, which is the emitter's own bookkeeping and the only race-free measure here.
	// Start runs an immediate check in its goroutine, so under load that check can land between
	// this test's advance and its emitDue: the goroutine writes the pack file, this test's
	// emitDue correctly declines because the archive already covers the period, and the
	// goroutine has not reached its notify callback yet. Counting callbacks then reads zero
	// packs for a period that was written. Emit renames a temp file into place, so a pack that
	// is visible is complete.
	packs := func() int {
		t.Helper()
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("ReadDir() error = %v", err)
		}
		n := 0
		for _, entry := range entries {
			if !entry.IsDir() && !strings.HasSuffix(entry.Name(), ".tmp") {
				n++
			}
		}
		return n
	}

	e := NewEmitter(runs, audits, "", dir, time.Hour, nil,
		WithClock(now),
		WithNotify(func(p string, _, _ time.Time) {
			mu.Lock()
			defer mu.Unlock()
			written = append(written, p)
		}))
	defer e.Close()

	if err := e.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	// Start stamps the period's origin and runs one immediate check, which must not emit: no cadence
	// has passed yet.
	if n := packs(); n != 0 {
		t.Fatalf("a pack was written before a cadence elapsed: %d", n)
	}

	// Walk past one cadence and ask again, the way the loop's ticker does.
	advance(90 * time.Minute)
	e.emitDue()
	if n := packs(); n < 1 {
		t.Fatalf("packs after one elapsed cadence = %d, want at least 1: an empty archive never "+
			"becomes due, so the archive stays empty for the life of the install", n)
	}

	// And it keeps going rather than emitting once and stalling.
	advance(3 * time.Hour)
	e.emitDue()
	if n := packs(); n < 2 {
		t.Errorf("packs after a second cadence = %d, want at least 2", n)
	}
	// The notify callback still has to fire for what landed, once the emitter's goroutine has
	// caught up. Nothing here depends on when it does.
	deadline := time.Now().Add(5 * time.Second)
	for count() < 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if count() < 1 {
		t.Error("packs landed in the archive but the notify callback never fired for any of them")
	}
}
