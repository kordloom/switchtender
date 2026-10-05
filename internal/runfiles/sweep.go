package runfiles

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	// ObservationGap is how far apart, on the sweeper's own monotonic clock, the two observations
	// that condemn a run directory must be.
	ObservationGap = 5 * time.Minute
	// MinAge is how long a run directory must have gone without a change to its entries before a
	// sweep deletes it.
	MinAge = 5 * time.Minute
	// SweepInterval is roughly how often a running server or worker sweeps its root.
	SweepInterval = 5 * time.Minute
	// sweepJitter is the fraction of SweepInterval each wait is moved by at random, so processes that
	// started together do not keep sweeping together.
	sweepJitter = 0.1
	// firstSweepMax bounds the random delay before the first periodic sweep after the startup pass.
	firstSweepMax = 60 * time.Second
	// sweepLockName is the lock in the root that lets one process per host sweep a given pass.
	sweepLockName = ".sweep"
)

// PassResult is what one sweep pass did.
type PassResult struct {
	// Skipped reports that another process held the root's sweep lock, so this pass did nothing.
	Skipped bool
	// Live counts run directories whose lock was held, which a pass never touches.
	Live int
	// Watched counts run directories whose lock was free and that the pass recorded or kept watching
	// rather than deleting yet.
	Watched int
	// Removed counts run directories the pass deleted.
	Removed int
}

// sighting is what a sweeper last recorded about one lock-free run directory.
type sighting struct {
	// info identifies the directory, so one deleted and recreated under the same name starts over.
	info fs.FileInfo
	// locked reports that the directory had a lock file, which a directory only lacks in the moment
	// it is being created or when its creator died in that moment.
	locked bool
	// beat is the heartbeat counter file's content, nil when there was none.
	beat []byte
	// at is when, on the sweeper's monotonic clock, this exact state was first seen.
	at time.Duration
}

// Sweeper removes the run directories under one root that processes which died mid-run left behind.
//
// A directory is deleted only when all of these hold: its lock was free at an earlier pass and is
// free now, the two passes are at least ObservationGap apart on this sweeper's monotonic clock, its
// heartbeat counter did not change between them, and nothing in it changed for at least MinAge. The
// lock is the liveness signal. The counter is never a liveness oracle: it only stops a deletion
// when a lock that reads free somehow belongs to a process still counting. MinAge is read from the
// directory's modification time, which can only defer a deletion. A time in the future, which only
// a clock stepped backward produces, counts as old, so a wrong wall clock cannot keep a dead run's
// secrets on disk forever.
//
// Observations live in memory across passes and survive a pass that failed, but not a restart,
// which simply starts fresh. That makes a restart cost one ObservationGap of extra wait and never a
// deletion that skipped the wait.
type Sweeper struct {
	// root is the directory the run directories live under.
	root string
	// mu serializes passes in this process and guards seen.
	mu sync.Mutex
	// seen holds the last sighting of each lock-free run directory, by name.
	seen map[string]sighting
	// mono reads the sweeper's monotonic clock as time elapsed since the sweeper was made.
	mono func() time.Duration
	// wall reads the wall clock, used only to judge MinAge.
	wall func() time.Time
	// gap is the ObservationGap this sweeper applies.
	gap time.Duration
	// minAge is the MinAge this sweeper applies.
	minAge time.Duration
	// interval is the SweepInterval this sweeper's Run loop waits between passes, before jitter.
	interval time.Duration
	// firstMax bounds the random wait before the first periodic pass.
	firstMax time.Duration
	// random returns a number in [0, 1), for the jitter.
	random func() float64
	// remove deletes a condemned run directory whose lock the pass holds through lock, nil for a
	// directory without one, releasing the lock whatever happens.
	remove func(path string, lock *os.File) error
}

// NewSweeper returns a sweeper for root with the default timing.
func NewSweeper(root string) *Sweeper {
	start := time.Now()
	return &Sweeper{
		root: root, seen: map[string]sighting{},
		// time.Since reads the monotonic clock time.Now recorded, so a wall clock stepped by an
		// administrator or by NTP moves nothing here.
		mono:     func() time.Duration { return time.Since(start) },
		wall:     time.Now,
		gap:      ObservationGap,
		minAge:   MinAge,
		interval: SweepInterval,
		firstMax: firstSweepMax,
		random:   rand.Float64,
		remove:   removeRun,
	}
}

// removeRun deletes a run directory whose lock the caller holds through lock, or one with no lock
// when lock is nil.
func removeRun(path string, lock *os.File) error {
	if lock != nil {
		return removeLocked(path, lock)
	}
	return removeTree(path)
}

// sweepers holds this process's sweeper for each root Sweep is called with, so successive calls
// accumulate the observations the deletion rule needs.
var sweepers = struct {
	// mu guards m.
	mu sync.Mutex
	// m maps a cleaned root to its sweeper.
	m map[string]*Sweeper
}{m: map[string]*Sweeper{}}

// sweeperFor returns this process's sweeper for root, making it on first use.
func sweeperFor(root string) *Sweeper {
	root = filepath.Clean(root)
	sweepers.mu.Lock()
	defer sweepers.mu.Unlock()
	s, ok := sweepers.m[root]
	if !ok {
		s = NewSweeper(root)
		sweepers.m[root] = s
	}
	return s
}

// Root returns the directory this sweeper sweeps.
func (s *Sweeper) Root() string { return s.root }

// Run sweeps once immediately, which is the startup sweep, then again after a random delay of up to
// a minute and about every SweepInterval after that, with jitter, until ctx ends. report receives
// every pass's result, and may be nil.
func (s *Sweeper) Run(ctx context.Context, report func(PassResult, error)) {
	if report == nil {
		report = func(PassResult, error) {}
	}
	report(s.Pass())
	timer := time.NewTimer(time.Duration(s.random() * float64(s.firstMax)))
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		report(s.Pass())
		timer.Reset(s.nextWait())
	}
}

// nextWait returns the interval moved at random by up to sweepJitter of itself either way.
func (s *Sweeper) nextWait() time.Duration {
	spread := (s.random()*2 - 1) * sweepJitter
	return time.Duration(float64(s.interval) * (1 + spread))
}

// Pass sweeps the root once. It skips the pass when another process holds the root's sweep lock, so
// one process per host sweeps at a time, and otherwise records every lock-free run directory and
// removes the ones the deletion rule condemns. A missing root has nothing to sweep. An error about
// one directory does not stop the pass, and the observations made so far are kept.
func (s *Sweeper) Pass() (PassResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var res PassResult
	entries, err := os.ReadDir(s.root)
	if errors.Is(err, fs.ErrNotExist) {
		return res, nil
	}
	if err != nil {
		return res, fmt.Errorf("sweep run directories: %w", err)
	}
	if err := prepareRoot(s.root); err != nil {
		return res, err
	}
	release, held, err := s.takeSweepLock()
	if err != nil {
		return res, fmt.Errorf("sweep run directories: %w", err)
	}
	if !held {
		res.Skipped = true
		return res, nil
	}
	defer release()

	now := s.mono()
	present := map[string]bool{}
	var errs []error
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), dirPrefix) {
			continue
		}
		present[e.Name()] = true
		outcome, err := s.examine(e.Name(), now)
		if err != nil {
			errs = append(errs, err)
		}
		switch outcome {
		case outcomeLive:
			res.Live++
		case outcomeWatched:
			res.Watched++
		case outcomeRemoved:
			res.Removed++
		}
	}
	// A directory that is gone, removed by its owner or by another process's sweep, needs no memory.
	for name := range s.seen {
		if !present[name] {
			delete(s.seen, name)
		}
	}
	if len(errs) > 0 {
		return res, fmt.Errorf("sweep run directories: %w", errors.Join(errs...))
	}
	return res, nil
}

// outcome is what a pass did with one run directory.
type outcome int

const (
	// outcomeSkipped means the directory could not be judged this pass.
	outcomeSkipped outcome = iota
	// outcomeLive means its lock was held.
	outcomeLive
	// outcomeWatched means its lock was free and it was recorded or kept under watch.
	outcomeWatched
	// outcomeRemoved means it was deleted.
	outcomeRemoved
)

// examine judges one run directory by name at monotonic time now, deleting it when the rule allows.
func (s *Sweeper) examine(name string, now time.Duration) (outcome, error) {
	path := filepath.Join(s.root, name)
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		delete(s.seen, name)
		return outcomeSkipped, nil
	}
	if err != nil {
		return outcomeSkipped, err
	}
	// A link is not something Create makes, so it is neither followed nor removed.
	if !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 {
		return outcomeSkipped, nil
	}
	lock, err := openLockFile(filepath.Join(path, lockName))
	locked := true
	switch {
	case errors.Is(err, fs.ErrNotExist):
		locked = false
	case err != nil:
		return outcomeSkipped, err
	default:
		free, lerr := lockFile(lock, false)
		if lerr != nil || !free {
			_ = lock.Close()
			delete(s.seen, name)
			if lerr != nil {
				return outcomeSkipped, fmt.Errorf("%s: %w", name, lerr)
			}
			return outcomeLive, nil
		}
	}
	// From here this pass holds the directory's lock, when it has one, so its owner cannot be
	// mid-way through anything: a live owner would be holding it.
	releaseLock := func() {
		if lock != nil {
			_ = unlockFile(lock)
			_ = lock.Close()
		}
	}
	beat, err := os.ReadFile(filepath.Join(path, beatName))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		releaseLock()
		return outcomeSkipped, fmt.Errorf("%s: read heartbeat: %w", name, err)
	}
	current := sighting{info: info, locked: locked, beat: beat, at: now}
	prior, ok := s.seen[name]
	if !ok || !sameSighting(prior, current) {
		s.seen[name] = current
		releaseLock()
		return outcomeWatched, nil
	}
	if now-prior.at < s.gap || !s.oldEnough(info) {
		releaseLock()
		return outcomeWatched, nil
	}
	if err := s.remove(path, lock); err != nil {
		// What is left keeps its first sighting, so the pass after the problem clears finishes the
		// job without waiting another gap. Nothing can bring a condemned run directory back to life,
		// since Create never reuses a name, so a sighting that outlives a failed pass is safe.
		if left, ok := restate(path, prior.at); ok {
			s.seen[name] = left
		} else {
			delete(s.seen, name)
		}
		return outcomeWatched, fmt.Errorf("%s: %w", name, err)
	}
	delete(s.seen, name)
	return outcomeRemoved, nil
}

// restate records what is left of a run directory after a removal failed part way, as first seen at
// at, and reports false when nothing is left.
func restate(path string, at time.Duration) (sighting, bool) {
	info, err := os.Lstat(path)
	if err != nil {
		return sighting{}, false
	}
	_, lerr := os.Lstat(filepath.Join(path, lockName))
	beat, berr := os.ReadFile(filepath.Join(path, beatName))
	if berr != nil {
		beat = nil
	}
	return sighting{info: info, locked: lerr == nil, beat: beat, at: at}, true
}

// sameSighting reports whether b shows the same directory in the same state as a: the same
// identity, the same lock file presence, and a heartbeat counter that did not move.
func sameSighting(a, b sighting) bool {
	return os.SameFile(a.info, b.info) && a.locked == b.locked &&
		(a.beat == nil) == (b.beat == nil) && bytes.Equal(a.beat, b.beat)
}

// oldEnough reports whether the directory's entries went unchanged for at least minAge. A
// modification time in the future counts as old enough, since only a clock stepped backward makes
// one.
func (s *Sweeper) oldEnough(info fs.FileInfo) bool {
	age := s.wall().Sub(info.ModTime())
	return age < 0 || age >= s.minAge
}

// takeSweepLock takes the root's sweep lock without waiting. It reports false when another process
// holds it, and returns the release to call when the pass is over.
func (s *Sweeper) takeSweepLock() (func(), bool, error) {
	f, err := os.OpenFile(filepath.Join(s.root, sweepLockName), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, false, err
	}
	held, err := lockFile(f, false)
	if err != nil || !held {
		_ = f.Close()
		return nil, false, err
	}
	return func() {
		_ = unlockFile(f)
		_ = f.Close()
	}, true, nil
}
