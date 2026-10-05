package spanbeat_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/kordloom/switchtender/spanbeat"
)

// racedStore is a chain whose first beat loses a race: an ordinary append takes the lock between
// the emitter's clock reading and the beat, dated a millisecond after that reading, so the store
// refuses the beat rather than write it behind that entry. Every later beat dated after the racing
// entry is written.
type racedStore struct {
	// mu guards the fields below.
	mu sync.Mutex
	// head is the time of the chain's newest entry, zero before the race.
	head time.Time
	// times are the beat times the emitter handed over, in order.
	times []time.Time
	// beats counts the beats written.
	beats int64
}

// AppendSpanBeat refuses the first beat as behind the racing entry and writes any beat after it.
func (s *racedStore) AppendSpanBeat(_ context.Context, at time.Time, _ int) (spanbeat.AppendedBeat,
	error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.times = append(s.times, at)
	if s.head.IsZero() {
		s.head = at.Add(time.Millisecond)
		// The refusal reaches the emitter after the racing entry's time, as it does when the
		// racing request held the lock for a moment.
		time.Sleep(5 * time.Millisecond)
		return spanbeat.AppendedBeat{}, fmt.Errorf("append span beat: %w",
			&clockBehindErr{beat: s.beats + 1, last: s.head, clock: at})
	}
	if at.Before(s.head) {
		return spanbeat.AppendedBeat{}, fmt.Errorf("append span beat: %w",
			&clockBehindErr{beat: s.beats + 1, last: s.head, clock: at})
	}
	s.beats++
	s.head = at
	return spanbeat.AppendedBeat{At: at, Seq: s.beats, Hash: fmt.Sprintf("hash-%d", s.beats),
		Beat: s.beats}, nil
}

// written returns how many beats were written and the times the emitter offered.
func (s *racedStore) written() (int64, []time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.beats, append([]time.Time(nil), s.times...)
}

// TestEmitterRetriesABeatThatLostARace verifies a beat refused because another append took the lock
// between the emitter's clock reading and the beat is tried again at once, at a fresh reading. The
// refusal is a race and not a clock fault, and leaving it to the next tick put a gap in the record
// of a healthy install for every beat that lost one, which under steady writes was most of them.
func TestEmitterRetriesABeatThatLostARace(t *testing.T) {
	t.Parallel()
	store := &racedStore{}
	emitter := spanbeat.NewEmitter(store, time.Hour, nil)
	emitter.Start()
	ok := waitFor(t, 2*time.Second, func() bool {
		n, _ := store.written()
		return n >= 1
	})
	emitter.Close()
	n, times := store.written()
	if !ok {
		t.Fatalf("the beat that lost a race was not written: %d beats from %d attempts, want the "+
			"immediate beat written on its retry", n, len(times))
	}
	if len(times) != 2 || !times[1].After(times[0]) {
		t.Errorf("the emitter offered times %v, want one refused reading and one fresh reading after it",
			times)
	}
}

// clockedStore is a chain that times beats with its own clock and records which entry point the
// emitter used.
type clockedStore struct {
	// mu guards the counters.
	mu sync.Mutex
	// now counts beats appended through AppendSpanBeatNow.
	now int
	// handed counts beats appended through AppendSpanBeat with a time the emitter chose.
	handed int
}

// AppendSpanBeat records a beat timed by the emitter.
func (s *clockedStore) AppendSpanBeat(_ context.Context, at time.Time, _ int) (spanbeat.AppendedBeat,
	error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handed++
	return spanbeat.AppendedBeat{At: at, Seq: int64(s.now + s.handed), Hash: "h", Beat: 1}, nil
}

// AppendSpanBeatNow records a beat timed by the store.
func (s *clockedStore) AppendSpanBeatNow(_ context.Context, _ int) (spanbeat.AppendedBeat, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.now++
	return spanbeat.AppendedBeat{At: time.Now(), Seq: int64(s.now + s.handed), Hash: "h",
		Beat: int64(s.now)}, nil
}

// counts returns how many beats went through each entry point.
func (s *clockedStore) counts() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.now, s.handed
}

// TestEmitterLetsAClockedStoreTimeTheBeat verifies a store that reads its own clock under its
// append lock is asked to, so a beat is never timed before the lock it waits on.
func TestEmitterLetsAClockedStoreTimeTheBeat(t *testing.T) {
	t.Parallel()
	store := &clockedStore{}
	emitter := spanbeat.NewEmitter(store, time.Hour, nil)
	emitter.Start()
	ok := waitFor(t, 2*time.Second, func() bool {
		n, _ := store.counts()
		return n >= 1
	})
	emitter.Close()
	now, handed := store.counts()
	if !ok || handed != 0 {
		t.Errorf("the emitter appended %d beats timed by the store and %d timed by itself, want "+
			"every beat timed by the store", now, handed)
	}
}
