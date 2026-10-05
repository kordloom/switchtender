package audittest

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
)

// clockTolerance is how far apart this process's clock and a store's clock may read, which for the
// PostgreSQL store is the database server's clock. It is far smaller than the excursions the tests
// below plant, so a store that follows a fast clock cannot pass inside it.
const clockTolerance = 10 * time.Second

// testOutcomeOnce verifies a run's outcome reaches the chain once however many times it is
// appended. A committer whose append failed after it reached the database, a janitor that found the
// outcome owed while the process that finished the run was still committing it, and two replicas
// sweeping the same owed outcome all append it again, and a second outcome entry for one run is a
// receipt with two answers to what the run did.
func testOutcomeOnce(t *testing.T, store audit.Store) {
	t.Helper()
	ctx := context.Background()
	appendOne := func(method, path string) error {
		t.Helper()
		return store.Append(ctx, &audit.Entry{ID: audit.NewID(), Actor: "system:dispatcher",
			ActorType: "system", Method: method, Path: path})
	}
	if err := appendOne(audit.MethodRun, audit.OutcomePath("run_once", "succeeded")); err != nil {
		t.Fatalf("Append() of the first outcome error = %v", err)
	}
	for _, status := range []string{"succeeded", "failed"} {
		err := appendOne(audit.MethodRun, audit.OutcomePath("run_once", status))
		if !errors.Is(err, audit.ErrOutcomeRecorded) {
			t.Errorf("Append() of a second %s outcome for one run error = %v, want "+
				"audit.ErrOutcomeRecorded", status, err)
		}
	}
	// Another run's outcome, a request that merely names the same path, and the run's decision are
	// not a second outcome.
	for _, entry := range []struct {
		Method string
		Path   string
	}{
		{Method: audit.MethodRun, Path: audit.OutcomePath("run_other", "failed")},
		{Method: "POST", Path: audit.OutcomePath("run_once", "succeeded")},
		{Method: audit.MethodDecision, Path: "/runs/run_once/decision/approved"},
	} {
		if err := appendOne(entry.Method, entry.Path); err != nil {
			t.Errorf("Append() %s %s error = %v, want it written", entry.Method, entry.Path, err)
		}
	}

	// The race the rule exists for: every committer at once, one of them wins.
	const racers = 8
	var wg sync.WaitGroup
	errs := make([]error, racers)
	for i := range racers {
		wg.Go(func() {
			errs[i] = appendOne(audit.MethodRun, audit.OutcomePath("run_raced", "succeeded"))
		})
	}
	wg.Wait()
	won := 0
	for _, err := range errs {
		switch {
		case err == nil:
			won++
		case !errors.Is(err, audit.ErrOutcomeRecorded):
			t.Errorf("a racing outcome append error = %v, want nil or audit.ErrOutcomeRecorded", err)
		}
	}
	if won != 1 {
		t.Errorf("%d of %d racing appends of one run's outcome were written, want exactly 1", won,
			racers)
	}

	chain, err := store.Chain(ctx)
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	outcomes := map[string]int{}
	for _, e := range chain {
		if id, ok := audit.OutcomeRunID(e); ok {
			outcomes[id]++
		}
	}
	for _, id := range []string{"run_once", "run_other", "run_raced"} {
		if outcomes[id] != 1 {
			t.Errorf("the chain holds %d outcome entries for %s, want 1", outcomes[id], id)
		}
	}
	if ok, at := audit.Verify(chain); !ok {
		t.Errorf("the chain does not verify after refused outcome appends, broken at %d", at)
	}
}

// testFastClockIsHeld verifies a caller's time ahead of the store's clock is held to it, and that
// the next entry is not dragged forward behind it. A replica whose clock ran half an hour fast
// committed its outcome half an hour in the future, the pin then dated every later entry from
// every replica there, and an anchor a correct authority took over the next head was refused,
// failing every bundle drawn from the chain.
func testFastClockIsHeld(t *testing.T, store audit.Store) {
	t.Helper()
	ctx := context.Background()
	fast := &audit.Entry{ID: audit.NewID(), At: time.Now().Add(30 * time.Minute),
		Actor: "system:dispatcher", ActorType: "system", Method: audit.MethodRun,
		Path: audit.OutcomePath("run_fast_replica", "succeeded")}
	if err := store.Append(ctx, fast); err != nil {
		t.Fatalf("Append() from the fast clock error = %v", err)
	}
	healthy := &audit.Entry{ID: audit.NewID(), Actor: "admin", ActorType: "token",
		Method: "POST", Path: "/v1/templates"}
	if err := store.Append(ctx, healthy); err != nil {
		t.Fatalf("Append() from the healthy clock error = %v", err)
	}
	chain, err := store.Chain(ctx)
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	limit := time.Now().Add(clockTolerance)
	for _, e := range chain {
		if e.At.After(limit) {
			t.Errorf("entry %d (%s %s) is dated %s, %s ahead of the clock: chain time ran ahead "+
				"of the real clock", e.Seq, e.Method, e.Path, e.At.Format(time.RFC3339),
				time.Until(e.At).Round(time.Second))
		}
	}
	if len(chain) == 2 && chain[1].At.Before(chain[0].At) {
		t.Errorf("the healthy entry is dated %s, before the entry ahead of it at %s",
			chain[1].At, chain[0].At)
	}
}

// testSpanBeatBehindHead verifies a beat is never written behind the chain's newest entry. The
// newest entry is often not a beat: a request that took the append lock between the beat's clock
// reading and the beat reaching the store, or a replica whose clock reads ahead, leaves an ordinary
// entry dated after the beat's time. CheckBeatAdvance alone held the beat against the last beat,
// so the beat was written behind that entry and the chain's times ran backward with nothing
// tampered with.
func testSpanBeatBehindHead(t *testing.T, store audit.Store) {
	t.Helper()
	ctx := context.Background()
	base := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	if _, err := store.AppendSpanBeat(ctx, base, 60); err != nil {
		t.Fatalf("AppendSpanBeat() first beat error = %v", err)
	}
	entry := &audit.Entry{ID: audit.NewID(), At: base.Add(10 * time.Second), Actor: "admin",
		ActorType: "token", Method: "POST", Path: "/v1/templates"}
	if err := store.Append(ctx, entry); err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	refused, err := store.AppendSpanBeat(ctx, base.Add(5*time.Second), 60)
	if !errors.Is(err, audit.ErrClockBehind) {
		t.Fatalf("AppendSpanBeat() behind the newest entry error = %v, want audit.ErrClockBehind",
			err)
	}
	if refused != nil {
		t.Errorf("AppendSpanBeat() behind the newest entry returned %+v, want nil", refused)
	}
	checkHead(t, store, entry, 2)

	// A beat at the newest entry's own time is not behind it, and it takes the number the refused
	// beat did not use.
	level, err := store.AppendSpanBeat(ctx, base.Add(10*time.Second), 60)
	if err != nil {
		t.Fatalf("AppendSpanBeat() at the newest entry's time error = %v", err)
	}
	checkBeat(t, level, base.Add(10*time.Second), 2, 1, 60)
}

// testSpanBeatReadsTheStoreClock verifies a beat given no time is stamped from the store's own
// clock under the append lock, so ordinary appends racing it never put it behind the head. A beat
// timed by its caller before the lock races every request, and under steady writes that refused it
// over and over: the record went silent on a healthy install.
func testSpanBeatReadsTheStoreClock(t *testing.T, store audit.Store) {
	t.Helper()
	ctx := context.Background()
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				if err := store.Append(ctx, &audit.Entry{ID: audit.NewID(), Actor: "admin",
					ActorType: "token", Method: "POST", Path: "/v1/templates"}); err != nil {
					t.Errorf("Append() beside the beats error = %v", err)
					return
				}
			}
		})
	}
	for i := range 10 {
		beat, err := store.AppendSpanBeat(ctx, time.Time{}, 60)
		if err != nil {
			t.Errorf("AppendSpanBeat() %d with the store's clock error = %v, want every beat "+
				"written", i, err)
			continue
		}
		if d := time.Since(beat.At); d < -clockTolerance || d > clockTolerance {
			t.Errorf("beat %d is dated %s, %s from this clock: want the store's clock now", i,
				beat.At, d)
		}
	}
	close(stop)
	wg.Wait()

	chain, err := store.Chain(ctx)
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	for i := 1; i < len(chain); i++ {
		if chain[i].At.Before(chain[i-1].At) {
			t.Fatalf("entry %d (%s) is dated %s, before entry %d at %s", chain[i].Seq,
				chain[i].Method, chain[i].At, chain[i-1].Seq, chain[i-1].At)
		}
	}
	if ok, at := audit.Verify(chain); !ok {
		t.Errorf("the chain does not verify, broken at %d", at)
	}
}
