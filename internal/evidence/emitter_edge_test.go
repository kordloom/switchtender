package evidence

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/dossier"
	"github.com/kordloom/switchtender/internal/run"
)

// errListRuns is the failure a run store reports when a test wants the collect step to fail.
var errListRuns = errors.New("the run store is unavailable")

// failingRuns is a run.Store whose paging fails, so a test can drive the collect error path without
// standing up a database. Everything else is delegated to the embedded store.
type failingRuns struct {
	run.Store
	// err is what ListPage reports.
	err error
}

// ListPage reports the configured failure rather than a page of runs.
func (f failingRuns) ListPage(_ context.Context, _ run.ListFilter, _, _ int) ([]*run.Run, error) {
	return nil, f.err
}

// packFiles returns the names of the finished packs in dir, ignoring temporary files.
func packFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir() error = %v", err)
	}
	var names []string
	for _, entry := range entries {
		if !entry.IsDir() && !strings.HasSuffix(entry.Name(), ".tmp") {
			names = append(names, entry.Name())
		}
	}
	return names
}

// TestNewEmitterRefusesEveryDependencyItCannotWorkWithout pins the constructor's own contract.
//
// The existing table covers the cadence and the directory and not the stores, so a constructor that
// dropped the nil check would still pass. A nil store is not a configuration problem an operator can
// fix; it is a wiring mistake, and it surfaces as a nil dereference inside a goroutine three months
// later at the first quarterly tick, in a background loop whose panic takes the server with it.
func TestNewEmitterRefusesEveryDependencyItCannotWorkWithout(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name     string
		Runs     run.Store
		Audits   audit.Store
		Dir      string
		Cadence  time.Duration
		WantWord string
	}{{ // Test 0: No run store, so there are no changes to write about.
		Name: "nil run store", Runs: nil, Audits: audit.NewMemStore(), Dir: "d",
		Cadence: time.Hour, WantWord: "stores required",
	}, { // Test 1: No audit store, so the chain the register is verified against is missing.
		Name: "nil audit store", Runs: run.NewMemStore(), Audits: nil, Dir: "d",
		Cadence: time.Hour, WantWord: "stores required",
	}, { // Test 2: Neither store.
		Name: "no stores", Runs: nil, Audits: nil, Dir: "d", Cadence: time.Hour,
		WantWord: "stores required",
	}, { // Test 3: No directory, so nothing could be written even if it were generated.
		Name: "no directory", Runs: run.NewMemStore(), Audits: audit.NewMemStore(), Dir: "",
		Cadence: time.Hour, WantWord: "directory required",
	}, { // Test 4: A cadence under an hour, which is not the artifact this exists to produce.
		Name: "cadence under an hour", Runs: run.NewMemStore(), Audits: audit.NewMemStore(),
		Dir: "d", Cadence: 59 * time.Minute, WantWord: "at least an hour",
	}, { // Test 5: A zero cadence would make every tick due forever.
		Name: "zero cadence", Runs: run.NewMemStore(), Audits: audit.NewMemStore(), Dir: "d",
		Cadence: 0, WantWord: "at least an hour",
	}, { // Test 6: A negative cadence is nonsense and must not be treated as "always due".
		Name: "negative cadence", Runs: run.NewMemStore(), Audits: audit.NewMemStore(), Dir: "d",
		Cadence: -time.Hour, WantWord: "at least an hour",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			defer func() {
				r := recover()
				if r == nil {
					t.Fatalf("NewEmitter accepted %s", test.Name)
				}
				msg, ok := r.(string)
				if !ok {
					t.Fatalf("panicked with %T, want a string explaining the mistake", r)
				}
				if !strings.Contains(msg, test.WantWord) {
					t.Errorf("panic = %q, want it to mention %q", msg, test.WantWord)
				}
			}()
			NewEmitter(test.Runs, test.Audits, "", test.Dir, test.Cadence, nil)
		})
	}
}

// TestNewEmitterAcceptsExactlyAnHour pins the inclusive side of the cadence bound, so the refusal is
// at "under an hour" rather than at "an hour or less".
func TestNewEmitterAcceptsExactlyAnHour(t *testing.T) {
	t.Parallel()
	runs, audits, _ := seedPeriod(t)
	e := NewEmitter(runs, audits, "", t.TempDir(), time.Hour, nil)
	defer e.Close()
	if e.cadence != time.Hour {
		t.Errorf("cadence = %s, want an hour", e.cadence)
	}
}

// TestNewEmitterInstallsTheOptionsItWasGiven pins the wiring between the constructor and the options.
//
// An option that is accepted and then not stored, or stored into the wrong field, passes every test
// aimed at the behavior it configures because those tests reach the behavior another way. The clock
// is the one that matters most: an emitter that kept time.Now while a test thought it had a fake
// clock would look correct and would never have exercised the scheduler at all.
func TestNewEmitterInstallsTheOptionsItWasGiven(t *testing.T) {
	t.Parallel()
	runs, audits, base := seedPeriod(t)
	var notified []string
	fixed := base.Add(99 * time.Hour)

	e := NewEmitter(runs, audits, "in_test", t.TempDir(), 2*time.Hour, nil,
		WithClock(func() time.Time { return fixed }),
		WithMaxChanges(7),
		WithNotify(func(p string, _, _ time.Time) { notified = append(notified, p) }))
	defer e.Close()

	if !e.now().Equal(fixed) {
		t.Errorf("clock = %s, want the option's %s: the emitter kept its own clock", e.now(), fixed)
	}
	if e.limit != 7 {
		t.Errorf("limit = %d, want the option's 7", e.limit)
	}
	if e.notify == nil {
		t.Fatal("the notify option was dropped, so no pack is ever announced")
	}
	e.notify("path", base, base)
	if diff := cmp.Diff([]string{"path"}, notified, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("the installed notify is not the one given (-want +got):\n%s", diff)
	}
	if e.installID != "in_test" {
		t.Errorf("installID = %q, want in_test", e.installID)
	}
	if e.cadence != 2*time.Hour {
		t.Errorf("cadence = %s, want 2h", e.cadence)
	}
	// A nil logger becomes a working no-op rather than a nil dereference on the first write.
	if e.log == nil {
		t.Fatal("a nil logger was stored as nil, so the first log call panics in the loop goroutine")
	}
	// A supplied logger is kept as given.
	log := zap.NewNop()
	withLog := NewEmitter(runs, audits, "", t.TempDir(), time.Hour, log)
	defer withLog.Close()
	if withLog.log != log {
		t.Error("the supplied logger was replaced")
	}
}

// TestMaxChangesFallsBackToTheDefaultRatherThanToZero pins that a non-positive cap restores the
// documented default.
//
// A cap of zero reaching the collector would be read as "no bound" by one layer and "carry nothing"
// by another. The option documents that a value which is not positive restores the default, and the
// order matters: the fallback has to run after the options rather than before, or an explicit zero
// would survive.
func TestMaxChangesFallsBackToTheDefaultRatherThanToZero(t *testing.T) {
	t.Parallel()
	runs, audits, _ := seedPeriod(t)
	tests := []struct {
		Name      string
		Limit     int
		WantLimit int
	}{{ // Test 0: Zero restores the default rather than capping at nothing.
		Name: "zero", Limit: 0, WantLimit: dossier.MaxRegisterRuns,
	}, { // Test 1: A negative cap likewise.
		Name: "negative", Limit: -5, WantLimit: dossier.MaxRegisterRuns,
	}, { // Test 2: A cap of one is the smallest meaningful bound and is kept.
		Name: "one", Limit: 1, WantLimit: 1,
	}, { // Test 3: An ordinary cap is kept as given.
		Name: "ordinary", Limit: 250, WantLimit: 250,
	}, { // Test 4: A cap above the default is kept, since the option overrides rather than clamps.
		Name: "above the default", Limit: dossier.MaxRegisterRuns * 2,
		WantLimit: dossier.MaxRegisterRuns * 2,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			e := NewEmitter(runs, audits, "", t.TempDir(), time.Hour, nil,
				WithMaxChanges(test.Limit))
			defer e.Close()
			if e.limit != test.WantLimit {
				t.Errorf("limit = %d, want %d", e.limit, test.WantLimit)
			}
		})
	}
	// With no option at all the default is in place, so the fallback is not the only thing setting it.
	plain := NewEmitter(runs, audits, "", t.TempDir(), time.Hour, nil)
	defer plain.Close()
	if plain.limit != dossier.MaxRegisterRuns {
		t.Errorf("limit with no option = %d, want the default %d", plain.limit,
			dossier.MaxRegisterRuns)
	}
}

// TestParsePeriodRefusesEveryNameThatIsNotAPack pins the archive's own name grammar.
//
// Progress is read back out of pack names, so this function decides what counts as recorded
// progress. A name it wrongly accepts moves the archive's resume point to whatever timestamp it
// managed to read, permanently skipping the period between there and the real end. A temporary file
// left by a crashed write is the case that matters most: reading one as progress would skip the very
// period whose write failed.
func TestParsePeriodRefusesEveryNameThatIsNotAPack(t *testing.T) {
	t.Parallel()
	good := packName(time.Date(2026, 7, 1, 6, 0, 0, 0, time.UTC),
		time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC))
	tests := []struct {
		Name string
		In   string
	}{{ // Test 0: The temporary file a write in progress leaves behind is not progress.
		Name: "temp file", In: good + ".tmp",
	}, { // Test 1: A wholly unrelated file an operator dropped in the directory.
		Name: "unrelated", In: "notes.txt",
	}, { // Test 2: An empty name.
		Name: "empty", In: "",
	}, { // Test 3: The prefix and suffix with nothing between them.
		Name: "no period", In: namePrefix + nameSuffix,
	}, { // Test 4: A period with only one side of the range.
		Name: "one timestamp", In: namePrefix + "20260701T060000Z" + nameSuffix,
	}, { // Test 5: Three timestamps, which no writer produces and no reader should trust.
		Name: "three timestamps",
		In:   namePrefix + "20260701T060000Z-to-20260701T120000Z-to-20260701T180000Z" + nameSuffix,
	}, { // Test 6: The right shape with an unparseable start.
		Name: "bad start", In: namePrefix + "notatime-to-20260701T120000Z" + nameSuffix,
	}, { // Test 7: The right shape with an unparseable end.
		Name: "bad end", In: namePrefix + "20260701T060000Z-to-notatime" + nameSuffix,
	}, { // Test 8: A month that does not exist is refused rather than rolled over into the next one.
		Name: "month 13", In: namePrefix + "20261301T060000Z-to-20260701T120000Z" + nameSuffix,
	}, { // Test 9: An hour of 25 likewise.
		Name: "hour 25", In: namePrefix + "20260701T250000Z-to-20260701T120000Z" + nameSuffix,
	}, { // Test 10: A stamp truncated to the minute, coarser than the layout.
		Name: "coarse stamp", In: namePrefix + "20260701T0600-to-20260701T1200" + nameSuffix,
	}, { // Test 11: The right name under a different extension.
		Name: "wrong extension", In: strings.TrimSuffix(good, nameSuffix) + ".json",
	}, { // Test 12: The right name under a different prefix.
		Name: "wrong prefix", In: "register-" + strings.TrimPrefix(good, namePrefix),
	}, { // Test 13: The right name with something appended after the extension.
		Name: "suffix then more", In: good + ".bak",
	}, { // Test 14: The right name with something prepended.
		Name: "prefix preceded", In: "old-" + good,
	}, { // Test 15: A backup an operator made by copying the pack.
		Name: "copy", In: strings.TrimSuffix(good, nameSuffix) + " copy" + nameSuffix,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			from, to, ok := parsePeriod(test.In)
			if ok {
				t.Errorf("parsePeriod(%q) accepted it as progress covering %s to %s", test.In, from, to)
			}
			if !from.IsZero() || !to.IsZero() {
				t.Errorf("a refused name returned times %s and %s rather than zero", from, to)
			}
		})
	}
	// The genuine article still parses, or the archive would look empty forever.
	if _, _, ok := parsePeriod(good); !ok {
		t.Errorf("parsePeriod refused the name packName produced: %q", good)
	}
}

// TestPackNameIsUniquePerPeriodAndAlwaysUTC pins that a name identifies exactly one period and does
// not depend on the writer's time zone.
//
// Two periods sharing a name means the second write silently replaces the first and the archive
// reads as complete while a period is missing. A name built from local time would make the same
// period produce different names on two control nodes, so the same period would be written twice
// under two names and a reader could not tell which was which.
func TestPackNameIsUniquePerPeriodAndAlwaysUTC(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	seen := make(map[string]bool)
	for i := range 200 {
		// One second apart, which is the finest the name distinguishes.
		from := base.Add(time.Duration(i) * time.Second)
		name := packName(from, from.Add(time.Hour))
		if seen[name] {
			t.Fatalf("two periods share the name %s, so one pack silently replaces the other", name)
		}
		seen[name] = true
	}
	// The same instant in another zone is the same period and must produce the same name.
	tokyo, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Skipf("time zone database unavailable: %v", err)
	}
	from, to := base, base.Add(6*time.Hour)
	if diff := cmp.Diff(packName(from, to), packName(from.In(tokyo), to.In(tokyo))); diff != "" {
		t.Errorf("the same period named differently in another zone (-want +got):\n%s", diff)
	}
	// And the name really round trips back to the instants, in UTC.
	gotFrom, gotTo, ok := parsePeriod(packName(from.In(tokyo), to.In(tokyo)))
	if !ok {
		t.Fatal("a name written from a non-UTC time did not parse")
	}
	if !gotFrom.Equal(from) || !gotTo.Equal(to) {
		t.Errorf("round trip = %s to %s, want %s to %s", gotFrom, gotTo, from, to)
	}
	// Sub-second detail is deliberately not in the name, so two periods within one second collide.
	// This is recorded rather than asserted as good: the cadence floor is an hour, so it cannot
	// happen through the scheduler, and a direct caller emitting twice within a second would.
	sub := packName(from, to.Add(500*time.Millisecond))
	if sub != packName(from, to) {
		t.Error("the name now carries sub-second detail, which is a widening worth noticing")
	}
}

// TestResumeReadsOnlyPacksAndAnswersWithTheNewest pins how the emitter recovers its own progress.
//
// The archive is the bookkeeping. Everything else in the directory has to be ignored, and the answer
// has to be the newest end rather than the first one read, because directory order is not sorted.
// Answering with an older end would re-emit periods already written; answering with a name that is
// not a pack would move progress to a timestamp nothing covers and open a permanent hole.
func TestResumeReadsOnlyPacksAndAnswersWithTheNewest(t *testing.T) {
	t.Parallel()
	runs, audits, base := seedPeriod(t)
	dir := t.TempDir()
	e := NewEmitter(runs, audits, "", dir, time.Hour, nil)
	defer e.Close()

	// Packs written out of order, so the answer cannot come from directory order.
	ends := []time.Time{base.Add(6 * time.Hour), base.Add(2 * time.Hour), base.Add(4 * time.Hour)}
	for _, end := range ends {
		name := packName(end.Add(-2*time.Hour), end)
		if err := os.WriteFile(filepath.Join(dir, name), []byte("<html></html>"), 0o600); err != nil {
			t.Fatalf("WriteFile() error = %v", err)
		}
	}
	// Everything a real directory also holds, none of which is progress.
	newest := packName(base.Add(8*time.Hour), base.Add(20*time.Hour))
	for _, name := range []string{
		newest + ".tmp", "README.txt", "change-register-broken.html", ".DS_Store",
		"change-register-20260701T060000Z-to-notatime.html",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatalf("WriteFile() error = %v", err)
		}
	}
	// A directory named exactly like a pack, which an archiving tool creates.
	if err := os.Mkdir(filepath.Join(dir, newest), 0o750); err != nil {
		t.Fatalf("Mkdir() error = %v", err)
	}

	got, err := e.resume(base.Add(100 * time.Hour))
	if err != nil {
		t.Fatalf("resume() error = %v", err)
	}
	want := base.Add(6 * time.Hour).UTC()
	if !got.Equal(want) {
		t.Errorf("resume() = %s, want the newest pack's end %s; a directory named like a pack or a "+
			"temporary file was read as progress", got, want)
	}
}

// TestResumeReportsAnUnreadableArchiveRatherThanGuessing pins that a directory the emitter cannot
// read is an error rather than an empty archive.
//
// An empty archive means "start the first period now". If an unreadable directory answered the same
// way, an install whose evidence directory was renamed or whose permissions changed would silently
// restart its period on every tick and never emit, while the log said the feature was on.
func TestResumeReportsAnUnreadableArchiveRatherThanGuessing(t *testing.T) {
	t.Parallel()
	runs, audits, base := seedPeriod(t)
	dir := t.TempDir()
	e := NewEmitter(runs, audits, "", filepath.Join(dir, "gone"), time.Hour, nil)
	defer e.Close()

	got, err := e.resume(base)
	if err == nil {
		t.Fatalf("resume() reported progress at %s for a directory that does not exist", got)
	}
	if !got.IsZero() {
		t.Errorf("a failed resume returned %s rather than the zero time", got)
	}
}

// TestEmitDueDoesNothingUntilAWholeCadenceHasPassed pins the scheduler's one decision.
//
// The loop ticks ten times per cadence, so most ticks must decline. Emitting early would produce
// packs covering less than a period, and since a pack's name is the archive's progress, a short pack
// silently redefines where the next period starts. The boundary is inclusive at exactly the cadence,
// which is what makes consecutive periods meet rather than overlap.
func TestEmitDueDoesNothingUntilAWholeCadenceHasPassed(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name      string
		Elapsed   time.Duration
		WantPacks int
	}{{ // Test 0: No time at all has passed.
		Name: "nothing elapsed", Elapsed: 0, WantPacks: 0,
	}, { // Test 1: A second short of the cadence still declines.
		Name: "one second short", Elapsed: time.Hour - time.Second, WantPacks: 0,
	}, { // Test 2: Exactly the cadence is due, so the bound is inclusive.
		Name: "exactly the cadence", Elapsed: time.Hour, WantPacks: 1,
	}, { // Test 3: Well past the cadence emits the whole span as one pack rather than several.
		Name: "several cadences", Elapsed: 5 * time.Hour, WantPacks: 1,
	}, { // Test 4: A clock that went backwards must not emit a pack covering a negative period.
		Name: "clock went backwards", Elapsed: -3 * time.Hour, WantPacks: 0,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			runs, audits, base := seedPeriod(t)
			dir := t.TempDir()
			var mu sync.Mutex
			clock := base
			now := func() time.Time {
				mu.Lock()
				defer mu.Unlock()
				return clock
			}
			e := NewEmitter(runs, audits, "", dir, time.Hour, nil, WithClock(now))
			defer e.Close()
			// started is what an empty archive measures the first period from, and Start is what
			// stamps it. Setting it here drives emitDue without the loop goroutine racing the test.
			e.started = base

			mu.Lock()
			clock = base.Add(test.Elapsed)
			mu.Unlock()
			e.emitDue()

			got := packFiles(t, dir)
			if len(got) != test.WantPacks {
				t.Errorf("packs = %d (%v), want %d", len(got), got, test.WantPacks)
			}
			if test.WantPacks == 0 {
				return
			}
			// The one pack covers exactly the elapsed span, so nothing before it is skipped.
			from, to, ok := parsePeriod(got[0])
			if !ok {
				t.Fatalf("wrote a file that is not a pack: %s", got[0])
			}
			if !from.Equal(base.UTC()) {
				t.Errorf("pack starts at %s, want the period origin %s", from, base.UTC())
			}
			if !to.Equal(base.Add(test.Elapsed).UTC()) {
				t.Errorf("pack ends at %s, want the clock %s", to, base.Add(test.Elapsed).UTC())
			}
		})
	}
}

// TestEmitDueSurvivesAnArchiveItCannotRead pins that a broken archive is logged and survived rather
// than crashing the loop or emitting anyway.
//
// This runs on a background goroutine on a timer. A panic there takes the process down, and emitting
// with no idea where the last period ended would write a pack over a span nobody chose. Declining and
// logging is the only safe answer, and the next tick tries again.
func TestEmitDueSurvivesAnArchiveItCannotRead(t *testing.T) {
	t.Parallel()
	runs, audits, base := seedPeriod(t)
	missing := filepath.Join(t.TempDir(), "never-created")
	e := NewEmitter(runs, audits, "", missing, time.Hour, nil,
		WithClock(func() time.Time { return base.Add(10 * time.Hour) }))
	defer e.Close()
	e.started = base

	e.emitDue()
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Errorf("emitDue created the directory it could not read: %v", err)
	}
}

// TestEmitDueSurvivesAFailingCollectAndDoesNotAdvance pins the other loop failure: the pack could not
// be built.
//
// Nothing may advance on a failure. Progress lives in the archive, so a period whose write failed
// stays uncovered and the next attempt spans it again plus whatever followed. If the emitter tracked
// progress any other way, a transient database failure would leave a permanent hole in the evidence
// archive with nothing on disk saying a period was ever missed.
func TestEmitDueSurvivesAFailingCollectAndDoesNotAdvance(t *testing.T) {
	t.Parallel()
	runs, audits, base := seedPeriod(t)
	dir := t.TempDir()
	broken := failingRuns{Store: runs, err: errListRuns}
	var mu sync.Mutex
	clock := base.Add(2 * time.Hour)
	e := NewEmitter(broken, audits, "", dir, time.Hour, nil, WithClock(func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return clock
	}))
	defer e.Close()
	e.started = base

	e.emitDue()
	if got := packFiles(t, dir); len(got) != 0 {
		t.Fatalf("a failed collect still wrote %v", got)
	}
	// Nothing advanced, so the resume point is still the origin and the next attempt covers the
	// whole span rather than only the latest cadence.
	resumed, err := e.resume(base.Add(2 * time.Hour))
	if err != nil {
		t.Fatalf("resume() error = %v", err)
	}
	if !resumed.Equal(base) {
		t.Errorf("resume() = %s, want the unchanged origin %s: a failed period was skipped past",
			resumed, base)
	}

	// The store recovers and the next tick covers everything since the origin in one pack.
	e.runs = runs
	mu.Lock()
	clock = base.Add(5 * time.Hour)
	mu.Unlock()
	e.emitDue()
	got := packFiles(t, dir)
	if len(got) != 1 {
		t.Fatalf("packs after recovery = %v, want one covering the whole span", got)
	}
	from, to, ok := parsePeriod(got[0])
	if !ok {
		t.Fatalf("wrote a file that is not a pack: %s", got[0])
	}
	if !from.Equal(base.UTC()) || !to.Equal(base.Add(5*time.Hour).UTC()) {
		t.Errorf("recovery pack covers %s to %s, want %s to %s; the failed period was not made up",
			from, to, base.UTC(), base.Add(5*time.Hour).UTC())
	}
}

// TestEmitReportsACollectFailureRatherThanWritingAnEmptyPack pins that Emit fails loudly.
//
// An evidence archive with a silent hole is worse than one that is loudly incomplete, because only
// the second gets fixed. A pack written from a failed collect would be a document claiming a period
// had no changes, which is the strongest possible false statement this product can make: an auditor
// sampling that period would find a signed register saying nothing happened.
func TestEmitReportsACollectFailureRatherThanWritingAnEmptyPack(t *testing.T) {
	t.Parallel()
	runs, audits, base := seedPeriod(t)
	dir := t.TempDir()
	e := NewEmitter(failingRuns{Store: runs, err: errListRuns}, audits, "", dir, time.Hour, nil)
	defer e.Close()

	err := e.Emit(context.Background(), base, base.Add(time.Hour))
	if err == nil {
		t.Fatal("Emit() reported success while the changes could not be read, so the pack would " +
			"claim a period had no changes")
	}
	if !errors.Is(err, errListRuns) {
		t.Errorf("error = %v, does not wrap the underlying failure, so a caller cannot tell why", err)
	}
	if !strings.Contains(err.Error(), "collect register") {
		t.Errorf("error = %q, want it to name the step that failed", err)
	}
	if got := packFiles(t, dir); len(got) != 0 {
		t.Errorf("a failed collect left %v in the archive", got)
	}
	// Not even a temporary file is left behind to be mistaken for a pack later.
	entries, rerr := os.ReadDir(dir)
	if rerr != nil {
		t.Fatalf("ReadDir() error = %v", rerr)
	}
	if len(entries) != 0 {
		t.Errorf("the directory holds %d entries after a failed emit", len(entries))
	}
}

// TestEmitHonorsACanceledContext pins that a caller's cancellation reaches the collect step.
//
// Emit takes its own context so a pack generated by hand is not killed by an unrelated shutdown. The
// other half of that is that the caller's own cancellation must work: a CLI user pressing Ctrl+C
// during a manual generation should stop, and it must stop without leaving a partial document that
// reads as a complete period.
func TestEmitHonorsACanceledContext(t *testing.T) {
	t.Parallel()
	runs, audits, base := seedPeriod(t)
	dir := t.TempDir()
	e := NewEmitter(runs, audits, "", dir, time.Hour, nil)
	defer e.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := e.Emit(ctx, base, base.Add(time.Hour))
	if err == nil {
		// Not every store consults the context, so a success here is acceptable as long as what
		// landed is a whole pack rather than a partial one.
		got := packFiles(t, dir)
		if len(got) != 1 {
			t.Fatalf("Emit() with a canceled context succeeded but wrote %v", got)
		}
		return
	}
	if got := packFiles(t, dir); len(got) != 0 {
		t.Errorf("a canceled Emit left %v in the archive", got)
	}
}

// TestEmitWritesAPackForAPeriodWithNoChanges pins that a quiet period is still recorded.
//
// Skipping an empty period would leave a gap in the archive that is indistinguishable from a period
// whose write failed, and it would mean the archive's progress never advances through a quiet stretch
// so the next pack covers an ever-growing span. A register saying "no changes in this period" is
// itself the evidence an auditor is asking for.
func TestEmitWritesAPackForAPeriodWithNoChanges(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	runs := run.NewMemStore()
	audits := audit.NewMemStore()
	dir := t.TempDir()
	var gotFrom, gotTo time.Time
	e := NewEmitter(runs, audits, "", dir, time.Hour, nil,
		WithNotify(func(_ string, from, to time.Time) { gotFrom, gotTo = from, to }))
	defer e.Close()

	from, to := base, base.Add(time.Hour)
	if err := e.Emit(context.Background(), from, to); err != nil {
		t.Fatalf("Emit() error = %v", err)
	}
	got := packFiles(t, dir)
	if diff := cmp.Diff([]string{packName(from, to)}, got, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("archive contents (-want +got):\n%s", diff)
	}
	// The notification names the period the document actually covers, not the emitter's idea of it.
	if !gotFrom.Equal(from) || !gotTo.Equal(to) {
		t.Errorf("notified period = %s to %s, want %s to %s", gotFrom, gotTo, from, to)
	}
	body, err := os.ReadFile(filepath.Join(dir, got[0]))
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if len(body) == 0 {
		t.Error("the pack for a quiet period is empty, so it is not evidence of anything")
	}
}

// TestEmitRefusesToLeaveAGapWhenAPeriodCannotBeSplit pins the one case the split cannot rescue.
//
// A period holding more changes than one register carries is normally split into consecutive packs.
// That relies on a boundary strictly inside the period. When a whole register's worth of changes
// share the period's first instant there is no such boundary, and no bounded document can cover them.
// The rule is that this is the loudly incomplete case rather than the silent one: the pack is written
// and says on its face that it is truncated, and Emit returns an error naming the pack and the
// instant, so an operator learns the archive has a gap there instead of finding a directory that
// reads as continuous.
func TestEmitRefusesToLeaveAGapWhenAPeriodCannotBeSplit(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	runs := run.NewMemStore()
	// More changes than the cap, all sharing the period's first instant, so there is no clean cut.
	for i := range 5 {
		if err := runs.Save(ctx, &run.Run{ID: fmt.Sprintf("run_%d", i), Playbook: "site.yml",
			Status: run.StatusSucceeded, Actor: "deploy-bot", CreatedAt: base}); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
	}
	dir := t.TempDir()
	e := NewEmitter(runs, audit.NewMemStore(), "", dir, time.Hour, nil, WithMaxChanges(2))
	defer e.Close()

	to := base.Add(6 * time.Hour)
	err := e.Emit(ctx, base, to)
	if err == nil {
		t.Fatal("Emit() reported success for a period it could not cover, so the archive reads as " +
			"continuous while changes are missing from it")
	}
	if !strings.Contains(err.Error(), "truncated") {
		t.Errorf("error = %q, want it to say the pack is truncated", err)
	}
	if !strings.Contains(err.Error(), "gap") {
		t.Errorf("error = %q, want it to say the archive has a gap after it", err)
	}
	if !strings.Contains(err.Error(), base.UTC().Format(time.RFC3339)) {
		t.Errorf("error = %q, want it to name the instant %s the gap begins at", err,
			base.UTC().Format(time.RFC3339))
	}
	// The pack it could write is still there, and says so, so the incompleteness is on the record
	// in the archive and not only in a log line nobody kept.
	got := packFiles(t, dir)
	if len(got) != 1 {
		t.Fatalf("archive holds %v, want the one truncated pack", got)
	}
	if !strings.Contains(err.Error(), got[0]) {
		t.Errorf("error = %q, want it to name the pack %s", err, got[0])
	}
	body, rerr := os.ReadFile(filepath.Join(dir, got[0]))
	if rerr != nil {
		t.Fatalf("ReadFile() error = %v", rerr)
	}
	if !strings.Contains(string(body), "truncated") {
		t.Error("the pack was cut short and the document does not say so, so a reader holding it " +
			"cannot tell it is only a part")
	}
}

// TestEmitNamesEachPackForWhatItActuallyCovers pins the split's central invariant.
//
// Progress is read back out of pack names. A pack named for the whole period while carrying part of
// it would move the archive past the changes it left out, permanently, while the directory still read
// as continuous. So each name has to describe the range its own document covers, consecutive packs
// have to meet exactly, and every change has to appear once across the archive.
func TestEmitNamesEachPackForWhatItActuallyCovers(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	hour := time.Hour
	runs, audits := seedSpread(t, base, 0, hour, 2*hour, 3*hour, 4*hour, 5*hour, 6*hour)
	dir := t.TempDir()
	var notified [][2]time.Time
	e := NewEmitter(runs, audits, "", dir, hour, nil, WithMaxChanges(2),
		WithNotify(func(_ string, from, to time.Time) {
			notified = append(notified, [2]time.Time{from, to})
		}))
	defer e.Close()

	from, to := base, base.Add(8*hour)
	if err := e.Emit(context.Background(), from, to); err != nil {
		t.Fatalf("Emit() error = %v", err)
	}
	packs := readArchive(t, dir)
	if len(packs) < 2 {
		t.Fatalf("archive holds %d packs, want the period split", len(packs))
	}
	if !packs[0].From.Equal(from.UTC()) {
		t.Errorf("archive starts at %s, want %s", packs[0].From, from.UTC())
	}
	if last := packs[len(packs)-1].To; !last.Equal(to.UTC()) {
		t.Errorf("archive ends at %s, want %s", last, to.UTC())
	}
	for i := 1; i < len(packs); i++ {
		if !packs[i].From.Equal(packs[i-1].To) {
			t.Errorf("pack %d starts at %s but the previous ended at %s, so the archive has a hole",
				i, packs[i].From, packs[i-1].To)
		}
	}
	// Every change once, in order, across the whole archive.
	var ids []string
	for _, p := range packs {
		ids = append(ids, p.IDs...)
	}
	want := []string{"run_0", "run_1", "run_2", "run_3", "run_4", "run_5", "run_6"}
	if diff := cmp.Diff(want, ids, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("changes across the archive (-want +got):\n%s", diff)
	}
	// Each pack is announced, with the period its own document covers rather than the whole span.
	if len(notified) != len(packs) {
		t.Fatalf("notified %d times for %d packs", len(notified), len(packs))
	}
	for i, n := range notified {
		if !n[0].Equal(packs[i].From) || !n[1].Equal(packs[i].To) {
			t.Errorf("notification %d covered %s to %s, want the pack's own %s to %s",
				i, n[0], n[1], packs[i].From, packs[i].To)
		}
	}
	// A resume now picks up exactly where the last pack ended, which is the whole point of naming
	// packs for what they cover.
	resumed, err := e.resume(to.Add(100 * hour))
	if err != nil {
		t.Fatalf("resume() error = %v", err)
	}
	if !resumed.Equal(to.UTC()) {
		t.Errorf("resume() = %s, want %s", resumed, to.UTC())
	}
}

// TestEmitKeepsThePacksThatLandedWhenALaterOneFails pins the partial-progress rule.
//
// A split writes several packs. If a later one fails, the earlier ones stay, so the archive advances
// to the end of the last pack that landed and the next attempt picks up from exactly there. Rolling
// them back would redo work already done and, worse, would leave the archive at a resume point that
// no longer matches what is on disk.
func TestEmitKeepsThePacksThatLandedWhenALaterOneFails(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	hour := time.Hour
	runs, audits := seedSpread(t, base, 0, hour, 2*hour, 3*hour, 4*hour, 5*hour)
	dir := t.TempDir()
	e := NewEmitter(runs, audits, "", dir, hour, nil, WithMaxChanges(2))
	defer e.Close()

	// The first pack lands, then the store fails, so the rest of the split cannot be built.
	var wrote int
	e.notify = func(string, time.Time, time.Time) {
		wrote++
		if wrote == 1 {
			e.runs = failingRuns{Store: runs, err: errListRuns}
		}
	}
	err := e.Emit(context.Background(), base, base.Add(8*hour))
	if err == nil {
		t.Fatal("Emit() reported success while a later pack could not be built")
	}
	packs := readArchive(t, dir)
	if len(packs) != 1 {
		t.Fatalf("archive holds %d packs, want the one that landed before the failure", len(packs))
	}
	// The archive's own progress is the end of that pack, so the retry covers exactly the rest.
	e.runs = runs
	resumed, rerr := e.resume(base.Add(8 * hour))
	if rerr != nil {
		t.Fatalf("resume() error = %v", rerr)
	}
	if !resumed.Equal(packs[0].To) {
		t.Errorf("resume() = %s, want the end of the pack that landed %s", resumed, packs[0].To)
	}
	e.notify = nil
	if err := e.Emit(context.Background(), resumed, base.Add(8*hour)); err != nil {
		t.Fatalf("the retry error = %v", err)
	}
	// Every change is in the archive exactly once, with no gap and no repeat across the failure.
	after := readArchive(t, dir)
	var ids []string
	for i, p := range after {
		ids = append(ids, p.IDs...)
		if i > 0 && !p.From.Equal(after[i-1].To) {
			t.Errorf("pack %d starts at %s but the previous ended at %s", i, p.From, after[i-1].To)
		}
	}
	want := []string{"run_0", "run_1", "run_2", "run_3", "run_4", "run_5"}
	if diff := cmp.Diff(want, ids, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("changes across the archive after the failure and retry (-want +got):\n%s", diff)
	}
}

// TestWritePackLeavesNoTemporaryFileBehind pins that a pack is written whole or not at all, and that
// the temporary file it goes through never survives.
//
// A pack truncated by a crash or a full disk reads as a present period, which is the one way an
// archive lies without anything reporting it. The rename is what makes a visible pack a complete one,
// and a leftover temporary file would eventually be mistaken for an archive member by whatever tool
// an auditor points at the directory.
func TestWritePackLeavesNoTemporaryFileBehind(t *testing.T) {
	t.Parallel()
	runs, audits, base := seedPeriod(t)
	dir := t.TempDir()
	e := NewEmitter(runs, audits, "", dir, time.Hour, nil)
	defer e.Close()

	for i := range 4 {
		from := base.Add(time.Duration(i) * time.Hour)
		if err := e.Emit(context.Background(), from, from.Add(time.Hour)); err != nil {
			t.Fatalf("Emit() error = %v", err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir() error = %v", err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
		if strings.HasSuffix(entry.Name(), ".tmp") {
			t.Errorf("%s was left behind and will be mistaken for an archive member", entry.Name())
		}
		if _, _, ok := parsePeriod(entry.Name()); !ok {
			t.Errorf("%s is in the archive and is not a pack", entry.Name())
		}
	}
	if len(names) != 4 {
		t.Errorf("archive holds %d entries (%v), want 4 packs", len(names), names)
	}
}

// TestWritePackReportsAndCleansUpWhenTheRenameCannotHappen pins the failure branch of the atomic
// write.
//
// The temporary file is removed when the rename fails, so a directory that filled up or lost
// permission mid-write does not accumulate half-written documents that later look like packs. A
// directory standing where the pack file belongs is the reachable way to make the rename fail
// without changing permissions.
func TestWritePackReportsAndCleansUpWhenTheRenameCannotHappen(t *testing.T) {
	t.Parallel()
	runs, audits, base := seedPeriod(t)
	dir := t.TempDir()
	e := NewEmitter(runs, audits, "", dir, time.Hour, nil)
	defer e.Close()

	from, to := base, base.Add(time.Hour)
	// A non-empty directory sits exactly where the pack file goes, so the rename cannot replace it.
	blocked := filepath.Join(dir, packName(from, to))
	if err := os.Mkdir(blocked, 0o750); err != nil {
		t.Fatalf("Mkdir() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(blocked, "occupied"), []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	err := e.Emit(context.Background(), from, to)
	if err == nil {
		t.Fatal("Emit() reported success while the pack could not be put in place")
	}
	if !strings.Contains(err.Error(), "write pack") {
		t.Errorf("error = %q, want it to name the step that failed", err)
	}
	entries, rerr := os.ReadDir(dir)
	if rerr != nil {
		t.Fatalf("ReadDir() error = %v", rerr)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".tmp") {
			t.Errorf("%s survived a failed rename, so a partial document is in the archive",
				entry.Name())
		}
	}
}

// TestEmitCreatesTheArchiveDirectoryItWasGiven pins that a manual generation into a directory that
// does not exist yet works, and that the directory is not world readable.
//
// Emit is reachable without Start, from a CLI generating a pack by hand, and that path has to create
// its own directory. The mode matters because a change register names who ran what against which
// hosts, which is exactly the reconnaissance an attacker with a shell on the box would want.
func TestEmitCreatesTheArchiveDirectoryItWasGiven(t *testing.T) {
	t.Parallel()
	runs, audits, base := seedPeriod(t)
	dir := filepath.Join(t.TempDir(), "nested", "evidence")
	e := NewEmitter(runs, audits, "", dir, time.Hour, nil)
	defer e.Close()

	if err := e.Emit(context.Background(), base, base.Add(time.Hour)); err != nil {
		t.Fatalf("Emit() error = %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o750 {
		t.Errorf("archive directory mode = %o, want 750", perm)
	}
	got := packFiles(t, dir)
	if len(got) != 1 {
		t.Fatalf("archive holds %v, want one pack", got)
	}
	// The pack itself is owner-only, since it names who ran what against which hosts.
	packInfo, err := os.Stat(filepath.Join(dir, got[0]))
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if perm := packInfo.Mode().Perm(); perm != 0o600 {
		t.Errorf("pack mode = %o, want 600", perm)
	}
}

// TestCloseIsSafeToCallMoreThanOnceAndWithoutStart pins the shutdown path.
//
// Close is wired into a server's shutdown sequence, which can run twice on a signal that arrives
// during shutdown, and it runs whether or not Start succeeded. A second Close that panicked or hung
// would turn a clean shutdown into a hang or a crash at the worst moment.
func TestCloseIsSafeToCallMoreThanOnceAndWithoutStart(t *testing.T) {
	t.Parallel()
	runs, audits, _ := seedPeriod(t)

	// Never started.
	never := NewEmitter(runs, audits, "", t.TempDir(), time.Hour, nil)
	never.Close()
	never.Close()

	// Started, then closed twice.
	started := NewEmitter(runs, audits, "", t.TempDir(), time.Hour, nil)
	if err := started.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		started.Close()
		started.Close()
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Close() did not return, so a server shutdown hangs here")
	}
}

// TestStartStampsTheOriginBeforeTheLoopCanTick pins the fixed point an empty archive measures from.
//
// resume used to answer with the caller's own clock, so emitDue compared that instant against itself
// and the elapsed time was zero on every tick. The feature was inert from a clean install, for the
// life of the install, while the server logged that periodic change registers were enabled. The
// origin has to be stamped by Start, before the loop can read it, or the same failure returns in a
// form no existing test would catch.
func TestStartStampsTheOriginBeforeTheLoopCanTick(t *testing.T) {
	t.Parallel()
	runs, audits, base := seedPeriod(t)
	dir := t.TempDir()
	e := NewEmitter(runs, audits, "", dir, time.Hour, nil,
		WithClock(func() time.Time { return base }))
	defer e.Close()

	if !e.started.IsZero() {
		t.Fatal("the origin was stamped before Start")
	}
	// Before Start, resume falls back to the caller's clock, which keeps a direct Emit caller
	// working exactly as it did.
	askedAt := base.Add(50 * time.Hour)
	got, err := e.resume(askedAt)
	if err != nil {
		t.Fatalf("resume() error = %v", err)
	}
	if !got.Equal(askedAt) {
		t.Errorf("resume() before Start = %s, want the caller's clock %s", got, askedAt)
	}

	if err := e.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if !e.started.Equal(base) {
		t.Errorf("origin = %s, want the clock at Start %s", e.started, base)
	}
	// After Start, resume answers with that fixed origin rather than with whatever it was asked at,
	// which is what makes a cadence able to elapse at all.
	got, err = e.resume(askedAt)
	if err != nil {
		t.Fatalf("resume() error = %v", err)
	}
	if !got.Equal(base) {
		t.Errorf("resume() after Start = %s, want the stamped origin %s; if it answers with the "+
			"caller's clock the elapsed time is zero on every tick and no pack is ever due", got, base)
	}
	if askedAt.Sub(got) < e.cadence {
		t.Error("the first period never becomes due from an empty archive")
	}
}

// TestStartCreatesTheArchiveDirectoryAndReportsOneItCannotUse pins that the directory is settled at
// startup rather than at the first tick.
//
// With the documented quarterly cadence the first tick is three months after the misconfiguration,
// and the startup log would have claimed the archive was accumulating the whole time. By then the
// runs the packs would have covered may already have been trimmed by retention, so the evidence is
// not merely late but gone.
func TestStartCreatesTheArchiveDirectoryAndReportsOneItCannotUse(t *testing.T) {
	t.Parallel()
	runs, audits, _ := seedPeriod(t)
	dir := filepath.Join(t.TempDir(), "nested", "evidence")
	e := NewEmitter(runs, audits, "", dir, time.Hour, nil)
	defer e.Close()
	if err := e.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("Start() did not create the archive directory: %v", err)
	}
	if !info.IsDir() {
		t.Fatal("the archive path is not a directory")
	}
	if perm := info.Mode().Perm(); perm != 0o750 {
		t.Errorf("archive directory mode = %o, want 750", perm)
	}
}

// TestEmitterIsSafeWhileItsLoopRuns pins that a manual generation alongside the running loop does not
// race, under -race.
//
// A CLI or an API route can ask for a pack by hand while the scheduled loop is running, and both go
// through the same emitter and the same directory. The loop reads the clock and the archive from its
// own goroutine, so anything the manual path touches is shared with it.
func TestEmitterIsSafeWhileItsLoopRuns(t *testing.T) {
	t.Parallel()
	runs, audits, base := seedPeriod(t)
	dir := t.TempDir()
	var mu sync.Mutex
	clock := base
	e := NewEmitter(runs, audits, "", dir, time.Hour, nil, WithClock(func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return clock
	}))
	if err := e.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Distinct periods, so these never contend for the same file name.
			from := base.Add(time.Duration(100+i*2) * time.Hour)
			if err := e.Emit(context.Background(), from, from.Add(time.Hour)); err != nil {
				t.Errorf("Emit() error = %v", err)
			}
		}(i)
	}
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			mu.Lock()
			clock = clock.Add(time.Hour)
			mu.Unlock()
			e.emitDue()
		}()
	}
	wg.Wait()
	e.Close()

	// Whatever landed, every file in the archive is a complete pack under a name that parses.
	for _, name := range packFiles(t, dir) {
		if _, _, ok := parsePeriod(name); !ok {
			t.Errorf("archive holds %q, which is not a pack", name)
		}
		body, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("ReadFile() error = %v", err)
		}
		if len(body) == 0 {
			t.Errorf("%s is empty, so a partial write became a visible pack", name)
		}
	}
}

// TestNotifyFailureDoesNotUnwriteThePack pins that the announcement is not part of the write.
//
// notify hands the pack to whatever channel the operator already uses, and that channel can be down.
// The pack is on disk before notify is called, so a chat integration failing must not make the
// archive lose a period. A panic in the callback is the operator's bug, and this records that it
// propagates rather than being swallowed, so it is not mistaken for the emitter failing.
func TestNotifyFailureDoesNotUnwriteThePack(t *testing.T) {
	t.Parallel()
	runs, audits, base := seedPeriod(t)
	dir := t.TempDir()
	e := NewEmitter(runs, audits, "", dir, time.Hour, nil,
		WithNotify(func(path string, _, _ time.Time) {
			// The pack is already in place by the time anyone is told about it.
			if _, err := os.Stat(path); err != nil {
				t.Errorf("notified about %s before it was on disk: %v", path, err)
			}
			panic("the notification channel is down")
		}))
	defer e.Close()

	from, to := base, base.Add(time.Hour)
	func() {
		defer func() { _ = recover() }()
		_ = e.Emit(context.Background(), from, to)
	}()
	got := packFiles(t, dir)
	if diff := cmp.Diff([]string{packName(from, to)}, got, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("the pack was lost when the notification failed (-want +got):\n%s", diff)
	}
}

// TestPeriodBoundariesAreHalfOpenSoNoChangeIsCountedTwice pins that consecutive packs meet without
// overlapping.
//
// The archive is a chain of adjacent periods, so a change created exactly on a boundary has to land
// in exactly one of them. Counting it in both would make an auditor's totals disagree with the run
// history, and counting it in neither would lose it entirely with the directory still reading as
// continuous.
func TestPeriodBoundariesAreHalfOpenSoNoChangeIsCountedTwice(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	hour := time.Hour
	// One change exactly on each boundary, plus one strictly inside the first period.
	runs, audits := seedSpread(t, base, 0, 30*time.Minute, hour, 2*hour)
	dir := t.TempDir()
	e := NewEmitter(runs, audits, "", dir, hour, nil)
	defer e.Close()

	ctx := context.Background()
	for i := range 3 {
		from := base.Add(time.Duration(i) * hour)
		if err := e.Emit(ctx, from, from.Add(hour)); err != nil {
			t.Fatalf("Emit() error = %v", err)
		}
	}
	packs := readArchive(t, dir)
	if len(packs) != 3 {
		t.Fatalf("archive holds %d packs, want 3", len(packs))
	}
	var all []string
	for _, p := range packs {
		all = append(all, p.IDs...)
	}
	// run_0 at the start, run_1 inside, run_2 on the first boundary, run_3 on the second: each once.
	want := []string{"run_0", "run_1", "run_2", "run_3"}
	if diff := cmp.Diff(want, all, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("changes across three adjacent periods (-want +got):\n%s", diff)
	}
	// A change on a boundary belongs to the period that starts there, not the one that ends there.
	if diff := cmp.Diff([]string{"run_0", "run_1"}, packs[0].IDs, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("first period contents (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]string{"run_2"}, packs[1].IDs, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("second period contents (-want +got):\n%s", diff)
	}
}
