package runfiles

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

// fakeClock is a monotonic clock and a wall clock a test moves by hand, so the deletion rule is
// measured without a five minute sleep.
type fakeClock struct {
	// mu guards the readings.
	mu sync.Mutex
	// elapsed is the monotonic reading.
	elapsed time.Duration
	// wall is the wall clock reading.
	wall time.Time
}

// newFakeClock returns a clock whose wall reading starts at the real time, so directories a test
// creates are as old as they are.
func newFakeClock() *fakeClock { return &fakeClock{wall: time.Now()} }

// mono reads the monotonic clock.
func (c *fakeClock) mono() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.elapsed
}

// now reads the wall clock.
func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.wall
}

// advance moves both readings forward by d.
func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.elapsed += d
	c.wall = c.wall.Add(d)
}

// testSweeper returns a sweeper over root that reads clock.
func testSweeper(root string, clock *fakeClock) *Sweeper {
	s := NewSweeper(root)
	s.mono = clock.mono
	s.wall = clock.now
	return s
}

// deadDir creates a run directory under root and releases it the way a dead owner does: lock free,
// counter still, everything else left behind.
func deadDir(t *testing.T, root, label string) string {
	t.Helper()
	d, err := Create(root, label)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if _, err := d.WriteFile("cred-*", label+"-secret"); err != nil {
		t.Fatal(err)
	}
	d.beat.halt()
	if err := unlockFile(d.lock); err != nil {
		t.Fatal(err)
	}
	_ = d.lock.Close()
	return d.Path()
}

// exists reports whether path is there.
func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// mustPass runs one pass and fails the test on an error.
func mustPass(t *testing.T, s *Sweeper) PassResult {
	t.Helper()
	res, err := s.Pass()
	if err != nil {
		t.Fatalf("Pass() error = %v", err)
	}
	return res
}

// TestSweepNeedsTwoObservationsAGapApart pins the deletion rule's timing: a dead directory survives
// the pass that first sees it and any pass less than ObservationGap later, and goes at the first
// pass a full gap after it was first seen in that state.
func TestSweepNeedsTwoObservationsAGapApart(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "root")
	dead := deadDir(t, root, "dead")
	clock := newFakeClock()
	clock.advance(MinAge)
	s := testSweeper(root, clock)

	steps := []struct {
		// Advance moves the clock before the pass.
		Advance time.Duration
		// WantResult is the pass's result.
		WantResult PassResult
		// WantThere says whether the directory survives the pass.
		WantThere bool
	}{{ // Test 0: The first sighting only records it.
		WantResult: PassResult{Watched: 1}, WantThere: true,
	}, { // Test 1: A second look one tick short of the gap still waits.
		Advance: ObservationGap - time.Nanosecond, WantResult: PassResult{Watched: 1}, WantThere: true,
	}, { // Test 2: A full gap after the first sighting it is removed.
		Advance: time.Nanosecond, WantResult: PassResult{Removed: 1}, WantThere: false,
	}}
	for stepNum, step := range steps {
		clock.advance(step.Advance)
		got := mustPass(t, s)
		if diff := cmp.Diff(step.WantResult, got); diff != "" {
			t.Errorf("step %d: pass result (-want +got):\n%s", stepNum, diff)
		}
		if exists(dead) != step.WantThere {
			t.Errorf("step %d: directory there = %v, want %v", stepNum, exists(dead), step.WantThere)
		}
	}
}

// TestSweepNeverTakesALiveRun proves a held lock wins over every other sign: a live run's directory
// survives passes across an hour of clock with its counter standing still, which is what a quiet
// tool and a stopped heartbeat would look like.
func TestSweepNeverTakesALiveRun(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "root")
	live, err := Create(root, "live")
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	defer func() { _ = live.Remove() }()
	live.beat.halt()
	live.beat = nil
	clock := newFakeClock()
	s := testSweeper(root, clock)
	for range 13 {
		if got := mustPass(t, s); got != (PassResult{Live: 1}) {
			t.Fatalf("pass over a live run = %+v, want it counted live and nothing else", got)
		}
		clock.advance(ObservationGap)
	}
	if !exists(live.Path()) {
		t.Fatal("a live run's directory was removed")
	}
}

// TestSweepCounterHoldsADeletionBack proves the counter does what it is for: a directory whose lock
// reads free while its counter still moves, which a lock lost on a live process would look like, is
// never removed while the counter moves, and only a full gap after it stops.
func TestSweepCounterHoldsADeletionBack(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "root")
	dir := deadDir(t, root, "counting")
	beat := filepath.Join(dir, beatName)
	clock := newFakeClock()
	clock.advance(MinAge)
	s := testSweeper(root, clock)
	for n := uint64(1); n <= 4; n++ {
		mustPass(t, s)
		clock.advance(ObservationGap)
		if err := os.WriteFile(beat, encodeBeat(n), 0o600); err != nil {
			t.Fatal(err)
		}
		// The write changes the directory's entries only if it creates the file, so the age rule
		// cannot be what holds this back.
	}
	if got := mustPass(t, s); got.Removed != 0 || !exists(dir) {
		t.Fatalf("pass after the counter moved = %+v, want the directory kept", got)
	}
	clock.advance(ObservationGap)
	if got := mustPass(t, s); got.Removed != 1 || exists(dir) {
		t.Fatalf("pass a gap after the counter stopped = %+v, want the directory removed", got)
	}
}

// TestSweepAgeRule proves the minimum age defers a deletion when the directory's entries changed
// recently, and that a modification time in the future, which only a clock stepped backward makes,
// does not keep a dead run's secrets on disk.
func TestSweepAgeRule(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// MTimeOffset sets the directory's modification time relative to the wall clock at the
		// second pass.
		MTimeOffset time.Duration
		// WantRemoved is whether the second pass, a full gap after the first, removes it.
		WantRemoved bool
	}{{ // Test 0: Changed a minute ago, so not yet old enough.
		MTimeOffset: -time.Minute, WantRemoved: false,
	}, { // Test 1: Changed exactly MinAge ago, so old enough.
		MTimeOffset: -MinAge, WantRemoved: true,
	}, { // Test 2: Stamped a day in the future by a clock that later stepped back: old enough.
		MTimeOffset: 24 * time.Hour, WantRemoved: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			root := filepath.Join(t.TempDir(), "root")
			dir := deadDir(t, root, "aged")
			clock := newFakeClock()
			s := testSweeper(root, clock)
			mustPass(t, s)
			clock.advance(ObservationGap)
			mtime := clock.now().Add(test.MTimeOffset)
			if err := os.Chtimes(dir, mtime, mtime); err != nil {
				t.Fatal(err)
			}
			got := mustPass(t, s)
			if (got.Removed == 1) != test.WantRemoved || exists(dir) == test.WantRemoved {
				t.Errorf("second pass = %+v with the directory there = %v, want removed = %v", got,
					exists(dir), test.WantRemoved)
			}
		})
	}
}

// TestSweepStartsOverOnAChange proves an observation is of one directory in one state. A directory
// replaced under the same name, or one that gained a lock file since the first look, is a new
// sighting with its own full gap to wait.
func TestSweepStartsOverOnAChange(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Change alters the directory between the two passes.
		Change func(t *testing.T, path string)
	}{{ // Test 0: The directory was moved away and another one made under its name.
		Change: func(t *testing.T, path string) {
			// The first directory stays on disk, out of the root, so the new one cannot be given
			// its inode number. A filesystem such as ext4 can give a deleted directory's number to
			// the next directory it makes.
			aside := filepath.Join(filepath.Dir(filepath.Dir(path)), "moved")
			if err := os.Rename(path, aside); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
		},
	}, { // Test 1: A lockless directory gained its lock file, as one being created does.
		Change: func(t *testing.T, path string) {
			if err := os.WriteFile(filepath.Join(path, lockName), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			root := filepath.Join(t.TempDir(), "root")
			if err := os.MkdirAll(root, 0o700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, dirPrefix+"changing")
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
			clock := newFakeClock()
			s := testSweeper(root, clock)
			mustPass(t, s)
			clock.advance(ObservationGap)
			test.Change(t, path)
			old := clock.now().Add(-MinAge)
			if err := os.Chtimes(path, old, old); err != nil {
				t.Fatal(err)
			}
			if got := mustPass(t, s); got.Removed != 0 || !exists(path) {
				t.Fatalf("pass after the change = %+v, want a fresh sighting and nothing removed", got)
			}
			clock.advance(ObservationGap)
			if got := mustPass(t, s); got.Removed != 1 || exists(path) {
				t.Fatalf("pass a gap after the change = %+v, want it removed", got)
			}
		})
	}
}

// TestSweepTouchesOnlyRunDirectories proves a pass leaves alone everything under the root that is
// not a run directory: other entries, files, the sweep lock, a probe file, and a link named like a
// run directory, which is neither followed nor removed.
func TestSweepTouchesOnlyRunDirectories(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("symbolic links need privileges on Windows")
	}
	root := filepath.Join(t.TempDir(), "root")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	keep := []string{
		filepath.Join(root, "unrelated"), filepath.Join(root, dirPrefix+"file"),
		filepath.Join(root, probePrefix+"1-x"), filepath.Join(root, dirPrefix+"link"),
	}
	if err := os.Mkdir(keep[0], 0o700); err != nil {
		t.Fatal(err)
	}
	for _, p := range keep[1:3] {
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(target, keep[3]); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	for _, p := range append([]string{target}, keep[:3]...) {
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	clock := newFakeClock()
	s := testSweeper(root, clock)
	for range 3 {
		mustPass(t, s)
		clock.advance(ObservationGap)
	}
	for _, p := range append(keep, target, filepath.Join(root, sweepLockName)) {
		if !exists(p) {
			t.Errorf("%s was removed by a sweep", p)
		}
	}
}

// TestSweeperRestartStartsFresh proves observations do not survive a restart: a new sweeper, which
// is what a restarted process has, waits its own full gap rather than inheriting the old one's.
func TestSweeperRestartStartsFresh(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "root")
	dead := deadDir(t, root, "restart")
	clock := newFakeClock()
	clock.advance(MinAge)
	mustPass(t, testSweeper(root, clock))
	clock.advance(ObservationGap)
	restarted := testSweeper(root, clock)
	if got := mustPass(t, restarted); got.Removed != 0 || !exists(dead) {
		t.Fatalf("a restarted sweeper's first pass = %+v, want it to only record", got)
	}
	clock.advance(ObservationGap)
	if got := mustPass(t, restarted); got.Removed != 1 {
		t.Fatalf("a restarted sweeper's pass a gap later = %+v, want the directory removed", got)
	}
}

// TestSweepSkipsWhileAnotherPassRuns proves the root's sweep lock: while another sweep holds it, a
// pass does nothing at all, and once it is let go the next pass runs.
func TestSweepSkipsWhileAnotherPassRuns(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "root")
	dead := deadDir(t, root, "held")
	clock := newFakeClock()
	clock.advance(MinAge)
	s := testSweeper(root, clock)
	mustPass(t, s)
	clock.advance(ObservationGap)

	other, err := os.OpenFile(filepath.Join(root, sweepLockName), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if held, err := lockFile(other, true); err != nil || !held {
		t.Fatalf("take the sweep lock: %v", err)
	}
	if got := mustPass(t, s); got != (PassResult{Skipped: true}) || !exists(dead) {
		t.Fatalf("pass while another sweep runs = %+v, want it skipped and nothing touched", got)
	}
	_ = unlockFile(other)
	_ = other.Close()
	if got := mustPass(t, s); got.Removed != 1 {
		t.Fatalf("pass once the other sweep let go = %+v, want the directory removed", got)
	}
}

// TestSimultaneousSweepersRemoveEachDirectoryOnce runs many sweepers against one root at once, the
// way every server and worker on a host does, and proves every dead directory goes exactly once, no
// pass trips over another's deletion, and the live directory survives them all.
func TestSimultaneousSweepersRemoveEachDirectoryOnce(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "root")
	const deadCount = 12
	for i := range deadCount {
		deadDir(t, root, fmt.Sprintf("dead-%d", i))
	}
	live, err := Create(root, "live")
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	defer func() { _ = live.Remove() }()
	clock := newFakeClock()
	clock.advance(MinAge)
	const sweeperCount = 8
	sweepers := make([]*Sweeper, sweeperCount)
	for i := range sweepers {
		sweepers[i] = testSweeper(root, clock)
	}
	var mu sync.Mutex
	removed, skipped := 0, 0
	var errs []error
	// Rounds run until nothing dead is left, a gap apart, with every sweeper passing at once in each.
	for round := 0; round < 20 && removed < deadCount; round++ {
		var wg sync.WaitGroup
		for _, s := range sweepers {
			wg.Add(1)
			go func(s *Sweeper) {
				defer wg.Done()
				res, err := s.Pass()
				mu.Lock()
				defer mu.Unlock()
				removed += res.Removed
				if res.Skipped {
					skipped++
				}
				if err != nil {
					errs = append(errs, err)
				}
			}(s)
		}
		wg.Wait()
		clock.advance(ObservationGap)
	}
	if len(errs) > 0 {
		t.Fatalf("passes failed: %v", errors.Join(errs...))
	}
	if removed != deadCount {
		t.Errorf("directories removed across all sweepers = %d, want each of the %d exactly once "+
			"(%d passes were skipped for another in progress)", removed, deadCount, skipped)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	var left []string
	for _, e := range entries {
		if e.IsDir() {
			left = append(left, e.Name())
		}
	}
	if diff := cmp.Diff([]string{filepath.Base(live.Path())}, left); diff != "" {
		t.Errorf("directories left under the root (-want +got):\n%s", diff)
	}
}

// TestSweepKeepsObservationsAcrossAFailedPass is cleanup failure handling for the sweep. A removal
// that fails reports the error, leaves the directory, and keeps its sighting, so the pass after the
// problem clears removes it without waiting a second gap, and the failure costs the other
// directories in the pass nothing.
func TestSweepKeepsObservationsAcrossAFailedPass(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "root")
	stuck := deadDir(t, root, "stuck")
	other := deadDir(t, root, "other")
	clock := newFakeClock()
	clock.advance(MinAge)
	s := testSweeper(root, clock)
	mustPass(t, s)
	clock.advance(ObservationGap)
	failure := errors.New("the disk refused")
	s.remove = func(path string, lock *os.File) error {
		if path == stuck {
			_ = unlockFile(lock)
			_ = lock.Close()
			return failure
		}
		return removeRun(path, lock)
	}
	res, err := s.Pass()
	if !errors.Is(err, failure) {
		t.Fatalf("pass with a failing removal error = %v, want the failure reported", err)
	}
	if res.Removed != 1 || exists(other) || !exists(stuck) {
		t.Fatalf("pass with a failing removal = %+v, want the other directory removed and the "+
			"stuck one kept", res)
	}
	s.remove = removeRun
	if got := mustPass(t, s); got.Removed != 1 || exists(stuck) {
		t.Fatalf("pass after the failure cleared = %+v, want it removed without another gap", got)
	}
}

// TestSweepReportsALockItCannotOpen is lock failure handling. A lock file the sweep cannot open is
// an error for that directory alone: the directory is kept, other directories are still judged, and
// the pass says what went wrong.
func TestSweepReportsALockItCannotOpen(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs POSIX file permissions that bind the account opening them")
	}
	root := filepath.Join(t.TempDir(), "root")
	blocked := deadDir(t, root, "blocked")
	dead := deadDir(t, root, "dead")
	if err := os.Chmod(filepath.Join(blocked, lockName), 0o000); err != nil {
		t.Fatal(err)
	}
	clock := newFakeClock()
	clock.advance(MinAge)
	s := testSweeper(root, clock)
	if _, err := s.Pass(); err == nil || !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("first pass error = %v, want the unreadable lock reported", err)
	}
	clock.advance(ObservationGap)
	res, err := s.Pass()
	if err == nil {
		t.Fatalf("second pass = %+v and no error, want the unreadable lock reported again", res)
	}
	if res.Removed != 1 || exists(dead) {
		t.Errorf("second pass = %+v, want the other dead directory removed regardless", res)
	}
	if !exists(blocked) {
		t.Error("a directory whose lock could not be read was removed")
	}
	_ = os.Chmod(filepath.Join(blocked, lockName), 0o600)
}

// TestSweepPackageFunctionKeepsObservations proves the package-level Sweep, which callers already
// use at startup, applies the same rule: its first call over a dead directory records it rather
// than removing it, and it never touches a live one.
func TestSweepPackageFunctionKeepsObservations(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "root")
	dead := deadDir(t, root, "dead")
	live, err := Create(root, "live")
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	defer func() { _ = live.Remove() }()
	if n, err := Sweep(root); err != nil || n != 0 {
		t.Fatalf("Sweep() = %d, %v, want 0 and no error on a first sighting", n, err)
	}
	if !exists(dead) || !exists(live.Path()) {
		t.Fatal("a first sighting removed a directory")
	}
	if got := sweeperFor(root).seen; len(got) != 1 {
		t.Errorf("observations kept after Sweep = %d, want the dead directory recorded", len(got))
	}
}

// TestSweepMissingRoot proves a host that never ran anything has nothing to sweep and no error.
func TestSweepMissingRoot(t *testing.T) {
	t.Parallel()
	n, err := Sweep(filepath.Join(t.TempDir(), "never"))
	if err != nil || n != 0 {
		t.Errorf("Sweep() = %d, %v, want 0 and no error", n, err)
	}
}

// TestRunSweepsAtStartupThenOnASchedule proves the loop's shape: one pass at once, which is the
// startup sweep, then passes on the jittered schedule until the context ends, after which it
// returns.
func TestRunSweepsAtStartupThenOnASchedule(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "root")
	s := NewSweeper(root)
	s.interval = time.Millisecond
	s.firstMax = time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	passes := make(chan struct{}, 64)
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Run(ctx, func(PassResult, error) {
			select {
			case passes <- struct{}{}:
			default:
			}
		})
	}()
	for i := range 3 {
		select {
		case <-passes:
		case <-time.After(10 * time.Second):
			t.Fatalf("pass %d never came", i)
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return once its context ended")
	}
}

// TestSweepScheduleJitter pins the schedule's bounds: the first periodic pass comes within a minute
// of startup, and each later wait is within a tenth of the interval either side of it.
func TestSweepScheduleJitter(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Random is what the sweeper's random source returns.
		Random float64
		// WantWait is the wait between periodic passes.
		WantWait time.Duration
	}{{ // Test 0: The lowest draw waits nine tenths of the interval.
		Random: 0, WantWait: SweepInterval * 9 / 10,
	}, { // Test 1: The middle draw waits the interval.
		Random: 0.5, WantWait: SweepInterval,
	}, { // Test 2: The top of the range waits eleven tenths.
		Random: 1, WantWait: SweepInterval * 11 / 10,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			s := NewSweeper(t.TempDir())
			s.random = func() float64 { return test.Random }
			if got := s.nextWait(); got != test.WantWait {
				t.Errorf("nextWait() = %v, want %v", got, test.WantWait)
			}
		})
	}
	if firstSweepMax != time.Minute || SweepInterval != 5*time.Minute {
		t.Errorf("first delay bound %v and interval %v, want a minute and five minutes",
			firstSweepMax, SweepInterval)
	}
}
