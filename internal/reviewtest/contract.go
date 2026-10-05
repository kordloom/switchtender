// Package reviewtest holds the review.Store contract every backend runs, so the in-memory, SQLite,
// and PostgreSQL stores cannot drift apart on how a report record round-trips, how a claim is won,
// and how a settle is fenced.
package reviewtest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/review"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/trigger"
)

// Contract runs the review.Store contract against a fresh store from newStore.
func Contract(t *testing.T, newStore func() review.Store) {
	t.Helper()
	t.Run("round trip keeps every field", func(t *testing.T) { testRoundTrip(t, newStore()) })
	t.Run("create never replaces a record", func(t *testing.T) { testCreateOnce(t, newStore()) })
	t.Run("pending is the records not done, oldest first", func(t *testing.T) {
		testPending(t, newStore())
	})
	t.Run("claim is a compare-and-set on the version", func(t *testing.T) {
		testClaim(t, newStore())
	})
	t.Run("one claim per pull request at a time", func(t *testing.T) { testLane(t, newStore()) })
	t.Run("settle is fenced on the claim", func(t *testing.T) { testSettle(t, newStore()) })
	t.Run("concurrent claims have one winner", func(t *testing.T) {
		testConcurrentClaims(t, newStore())
	})
	t.Run("concurrent claims on one pull request have one winner", func(t *testing.T) {
		testConcurrentLane(t, newStore())
	})
	t.Run("unrepresentable text stores the same on every backend", func(t *testing.T) {
		testUnrepresentableText(t, newStore())
	})
	t.Run("a comment is marked handled once and owes no report", func(t *testing.T) {
		testCommentMarkedOnce(t, newStore())
	})
	t.Run("pruning removes only old comment marks and done replies", func(t *testing.T) {
		testPruneComments(t, newStore())
	})
}

// testPruneComments pins that pruning bounds what comment commands leave behind without touching
// anything still owed: old command marks and old done replies go, while a reply still owed, a new
// mark, and a plan's record stay.
func testPruneComments(t *testing.T, store review.Store) {
	ctx := context.Background()
	tg := &trigger.Trigger{ID: "trg_prune", Review: &trigger.Review{
		Provider: trigger.ProviderGitHub, Repository: "acme/infra"}}
	event := func(id int64) *review.CommentEvent {
		return &review.CommentEvent{Provider: trigger.ProviderGitHub, Repository: "acme/infra",
			Number: 7, CommentID: id, AuthorID: 1001, Body: "/switchtender apply"}
	}
	old, cutoff, fresh := base, base.Add(24*time.Hour), base.Add(48*time.Hour)
	oldMark := review.CommandRecord(tg, event(1), old)
	newMark := review.CommandRecord(tg, event(2), fresh)
	doneReply := review.ReplyRecord(tg, event(3), "told", old)
	doneReply.Done = true
	owedReply := review.ReplyRecord(tg, event(4), "still owed", old)
	plan := fullRecord("prune")
	plan.CreatedAt = old
	for _, rec := range []*review.Record{oldMark, newMark, doneReply, owedReply, plan} {
		mustCreate(t, store, rec)
	}
	n, err := store.PruneComments(ctx, cutoff)
	if err != nil {
		t.Fatalf("PruneComments() error = %v", err)
	}
	if n != 2 {
		t.Errorf("PruneComments() removed %d, want 2", n)
	}
	for _, id := range []string{oldMark.ID, doneReply.ID} {
		if _, err := store.Get(ctx, id); !errors.Is(err, review.ErrRecordNotFound) {
			t.Errorf("Get(%s) after prune = %v, want not found", id, err)
		}
	}
	for _, id := range []string{newMark.ID, owedReply.ID, plan.ID} {
		mustGet(t, store, id)
	}
}

// testCommentMarkedOnce pins what makes a pull request comment act once across every process: the
// record marking it is created once, a second create for the same comment reports that it exists,
// and the record, born done, is never pending, while the comment's reply is.
func testCommentMarkedOnce(t *testing.T, store review.Store) {
	ctx := context.Background()
	tg := &trigger.Trigger{ID: "trg_comment", Review: &trigger.Review{
		Provider: trigger.ProviderGitHub, Repository: "acme/infra"}}
	ev := &review.CommentEvent{Provider: trigger.ProviderGitHub, Repository: "acme/infra",
		Number: 7, CommentID: 901, AuthorID: 1001, Body: "/switchtender apply"}
	mark := review.CommandRecord(tg, ev, base)
	created, err := store.Create(ctx, mark)
	if err != nil || !created {
		t.Fatalf("Create() of the comment's mark = %v, %v, want created", created, err)
	}
	again, err := store.Create(ctx, review.CommandRecord(tg, ev, base.Add(time.Second)))
	if err != nil || again {
		t.Fatalf("Create() of the same comment's mark = %v, %v, want an existing record", again, err)
	}
	reply := review.ReplyRecord(tg, ev, "Your GitHub account is not linked.", base)
	mustCreate(t, store, reply)
	got := mustGet(t, store, mark.ID)
	if !got.Done || got.Kind != review.KindCommand || got.Reason != review.CommandApply {
		t.Errorf("mark = done %v kind %q command %q, want a done command record for apply",
			got.Done, got.Kind, got.Reason)
	}
	pending, err := store.Pending(ctx, 0)
	if err != nil {
		t.Fatalf("Pending() error = %v", err)
	}
	var ids []string
	for _, r := range pending {
		ids = append(ids, r.ID)
	}
	if diff := cmp.Diff([]string{reply.ID}, ids); diff != "" {
		t.Errorf("pending mismatch (-want +got):\n%s", diff)
	}
}

// base is the instant the contract's records are stamped around, carrying nanoseconds so a store
// that rounds a time is caught.
var base = time.Date(2026, 10, 1, 9, 30, 0, 123456789, time.UTC)

// fullRecord returns a record with every field set to a value nothing else produces.
func fullRecord(id string) *review.Record {
	return &review.Record{
		ID: id, Kind: review.KindPlan, RunID: "run_" + id, TriggerID: "trg_full", PullRequest: 42,
		CommitSHA: "0123456789abcdef0123456789abcdef01234567", Reason: "reason " + id,
		Receipt: "41:9f2c", Phase: review.PhaseHeld, HeldBy: "prod gate", StatusState: "pending",
		CommentSHA256: "c0ffee", RecordedSHA256: "facade", ReportedAt: base.Add(time.Second),
		Done: false, Version: 7, ClaimedBy: "rep_a", ClaimedUntil: base.Add(time.Minute),
		Attempts: 3, FailingSince: base.Add(2 * time.Second), RetryAt: base.Add(3 * time.Second),
		LastError: "GET /user answered 502: bad gateway", CreatedAt: base,
		Provider: trigger.ProviderGitLab, APIURL: "https://gitlab.example.com/api/v4",
		Repository: "infra/" + id, StatusContext: "switchtender/" + id,
	}
}

// lane returns a trigger with id and no review configuration, which is all a record's lane needs.
func lane(id string) *trigger.Trigger {
	return &trigger.Trigger{ID: id}
}

// mustCreate inserts rec and fails the test when the store refuses or reports a duplicate.
func mustCreate(t *testing.T, store review.Store, rec *review.Record) {
	t.Helper()
	created, err := store.Create(context.Background(), rec)
	if err != nil {
		t.Fatalf("Create(%s) error = %v", rec.ID, err)
	}
	if !created {
		t.Fatalf("Create(%s) = false for a new id, want true", rec.ID)
	}
}

// mustGet reads a record back.
func mustGet(t *testing.T, store review.Store, id string) *review.Record {
	t.Helper()
	got, err := store.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("Get(%s) error = %v", id, err)
	}
	return got
}

// testRoundTrip pins that every field reads back as written, the times to the nanosecond, and that
// a missing id is ErrRecordNotFound. A store that dropped the claim columns would let two replicas
// believe they each held a report.
func testRoundTrip(t *testing.T, store review.Store) {
	want := fullRecord("round")
	mustCreate(t, store, want)
	if diff := cmp.Diff(want, mustGet(t, store, "round")); diff != "" {
		t.Errorf("round trip mismatch (-want +got):\n%s", diff)
	}
	bare := &review.Record{ID: "bare", Kind: review.KindFork, TriggerID: "trg_b", PullRequest: 1,
		CreatedAt: base}
	mustCreate(t, store, bare)
	if diff := cmp.Diff(bare, mustGet(t, store, "bare")); diff != "" {
		t.Errorf("a record with no report yet does not read back empty (-want +got):\n%s", diff)
	}
	_, err := store.Get(context.Background(), "missing")
	if !errors.Is(err, review.ErrRecordNotFound) {
		t.Errorf("Get(missing) error = %v, want ErrRecordNotFound", err)
	}
}

// testCreateOnce pins that a second create for an id changes nothing and says so, which is what
// makes a redelivered webhook and two replicas adopting one plan leave a single record.
func testCreateOnce(t *testing.T, store review.Store) {
	first := fullRecord("once")
	mustCreate(t, store, first)
	second := fullRecord("once")
	second.Phase, second.Version = review.PhaseFailed, 99
	created, err := store.Create(context.Background(), second)
	if err != nil {
		t.Fatalf("Create() again error = %v", err)
	}
	if created {
		t.Error("Create() of an existing id = true, want false")
	}
	if diff := cmp.Diff(first, mustGet(t, store, "once")); diff != "" {
		t.Errorf("a second create changed the record (-want +got):\n%s", diff)
	}
}

// testPending pins that Pending answers only records not done, oldest first even inside one second,
// and honors its limit.
func testPending(t *testing.T, store review.Store) {
	ctx := context.Background()
	for i, id := range []string{"p_c", "p_a", "p_b", "p_done"} {
		rec := &review.Record{ID: id, Kind: review.KindPlan, RunID: id, TriggerID: "trg_p",
			PullRequest: i + 1, CreatedAt: base.Add(time.Duration(3-i) * 100 * time.Millisecond)}
		mustCreate(t, store, rec)
	}
	// The whole second sorts after its fractions only if the store orders by time rather than by
	// text, which is the inversion a stored RFC 3339 string invites.
	mustCreate(t, store, &review.Record{ID: "p_whole", Kind: review.KindPlan, RunID: "p_whole",
		TriggerID: "trg_p", PullRequest: 9, CreatedAt: base.Truncate(time.Second)})
	done := mustGet(t, store, "p_done")
	won, err := store.Claim(ctx, "p_done", done.Version, "rep_a", base, base.Add(time.Minute))
	if err != nil || !won {
		t.Fatalf("Claim() = %v, %v", won, err)
	}
	done.Done = true
	if ok, err := store.Settle(ctx, done, done.Version+1); err != nil || !ok {
		t.Fatalf("Settle() = %v, %v", ok, err)
	}
	got, err := store.Pending(ctx, 0)
	if err != nil {
		t.Fatalf("Pending() error = %v", err)
	}
	var ids []string
	for _, r := range got {
		ids = append(ids, r.ID)
	}
	if diff := cmp.Diff([]string{"p_whole", "p_b", "p_a", "p_c"}, ids); diff != "" {
		t.Errorf("Pending() order mismatch (-want +got):\n%s", diff)
	}
	limited, err := store.Pending(ctx, 2)
	if err != nil {
		t.Fatalf("Pending(2) error = %v", err)
	}
	if len(limited) != 2 || limited[0].ID != "p_whole" || limited[1].ID != "p_b" {
		t.Errorf("Pending(2) = %d records, want the two oldest", len(limited))
	}
}

// testClaim pins the compare-and-set: the version read wins once, a stale version loses, a won
// claim moves the version on and names its holder, and a done or missing record loses without an
// error.
func testClaim(t *testing.T, store review.Store) {
	ctx := context.Background()
	rec := &review.Record{ID: "c_1", Kind: review.KindPlan, RunID: "c_1", TriggerID: "trg_c",
		PullRequest: 1, Version: 4, CreatedAt: base}
	mustCreate(t, store, rec)
	until := base.Add(time.Minute)
	tests := []struct {
		// Version is the version the caller claims against.
		Version int64
		// WantWon is whether the claim wins.
		WantWon bool
	}{{ // Test 0: A version older than the stored one loses.
		Version: 3, WantWon: false,
	}, { // Test 1: The stored version wins.
		Version: 4, WantWon: true,
	}, { // Test 2: The same version again loses, since the win moved it on.
		Version: 4, WantWon: false,
	}}
	for testNum, test := range tests {
		won, err := store.Claim(ctx, "c_1", test.Version, "rep_a", base, until)
		if err != nil {
			t.Fatalf("test %d: Claim() error = %v", testNum, err)
		}
		if won != test.WantWon {
			t.Errorf("test %d: Claim(version %d) = %v, want %v", testNum, test.Version, won,
				test.WantWon)
		}
	}
	got := mustGet(t, store, "c_1")
	if got.Version != 5 || got.ClaimedBy != "rep_a" || !got.ClaimedUntil.Equal(until) {
		t.Errorf("after the win: version %d by %q until %v, want 5 by rep_a until %v", got.Version,
			got.ClaimedBy, got.ClaimedUntil, until)
	}
	if won, err := store.Claim(ctx, "missing", 0, "rep_a", base, until); err != nil || won {
		t.Errorf("Claim(missing) = %v, %v, want a loss with no error", won, err)
	}
	got.Done = true
	if ok, err := store.Settle(ctx, got, got.Version); err != nil || !ok {
		t.Fatalf("Settle() = %v, %v", ok, err)
	}
	done := mustGet(t, store, "c_1")
	if won, err := store.Claim(ctx, "c_1", done.Version, "rep_b", base, until); err != nil || won {
		t.Errorf("Claim(done) = %v, %v, want a loss with no error", won, err)
	}
}

// testLane pins that a live claim on one of a pull request's records keeps every other record of
// that pull request and trigger from being claimed, that another pull request is unaffected, and
// that the lane frees once the claim is settled or has lapsed.
func testLane(t *testing.T, store review.Store) {
	ctx := context.Background()
	for _, r := range []*review.Record{
		review.PlanRecord("l_1", lane("trg_l"), 7, base), review.PlanRecord("l_2", lane("trg_l"), 7, base),
		review.PlanRecord("l_3", lane("trg_l"), 8, base), review.PlanRecord("l_4", lane("trg_x"), 7, base),
	} {
		mustCreate(t, store, r)
	}
	until := base.Add(time.Minute)
	if won, err := store.Claim(ctx, "l_1", 0, "rep_a", base, until); err != nil || !won {
		t.Fatalf("Claim(l_1) = %v, %v", won, err)
	}
	tests := []struct {
		// ID is the record claimed.
		ID string
		// Now is the store clock the claim is judged at.
		Now time.Time
		// WantWon is whether the claim wins.
		WantWon bool
	}{{ // Test 0: The same pull request through the same trigger waits for the live claim.
		ID: "l_2", Now: base.Add(time.Second), WantWon: false,
	}, { // Test 1: Another pull request is unaffected.
		ID: "l_3", Now: base.Add(time.Second), WantWon: true,
	}, { // Test 2: The same number through another trigger is another comment.
		ID: "l_4", Now: base.Add(time.Second), WantWon: true,
	}, { // Test 3: Once the first claim has lapsed, the pull request is free again.
		ID: "l_2", Now: until.Add(time.Second), WantWon: true,
	}}
	for testNum, test := range tests {
		won, err := store.Claim(ctx, test.ID, 0, "rep_b", test.Now, test.Now.Add(time.Minute))
		if err != nil {
			t.Fatalf("test %d: Claim() error = %v", testNum, err)
		}
		if won != test.WantWon {
			t.Errorf("test %d: Claim(%s) = %v, want %v", testNum, test.ID, won, test.WantWon)
		}
	}

	// A settled claim frees the lane at once, without waiting for it to lapse.
	for _, r := range []*review.Record{
		review.PlanRecord("s_1", lane("trg_s"), 1, base), review.PlanRecord("s_2", lane("trg_s"), 1, base),
	} {
		mustCreate(t, store, r)
	}
	if won, err := store.Claim(ctx, "s_1", 0, "rep_a", base, until); err != nil || !won {
		t.Fatalf("Claim(s_1) = %v, %v", won, err)
	}
	if won, err := store.Claim(ctx, "s_2", 0, "rep_b", base, until); err != nil || won {
		t.Fatalf("Claim(s_2) under a live claim = %v, %v, want a loss", won, err)
	}
	held := mustGet(t, store, "s_1")
	if ok, err := store.Settle(ctx, held, held.Version); err != nil || !ok {
		t.Fatalf("Settle(s_1) = %v, %v", ok, err)
	}
	if won, err := store.Claim(ctx, "s_2", 0, "rep_b", base, until); err != nil || !won {
		t.Errorf("Claim(s_2) after the settle = %v, %v, want a win", won, err)
	}
}

// testSettle pins that a settle writes the reporting state and releases the claim only for the
// version its claim produced, and that a lapsed claim's settle writes nothing.
func testSettle(t *testing.T, store review.Store) {
	ctx := context.Background()
	mustCreate(t, store, &review.Record{ID: "t_1", Kind: review.KindPlan, RunID: "t_1",
		TriggerID: "trg_t", PullRequest: 3, CreatedAt: base})
	won, err := store.Claim(ctx, "t_1", 0, "rep_a", base, base.Add(time.Minute))
	if err != nil || !won {
		t.Fatalf("Claim() = %v, %v", won, err)
	}
	// Another process takes the record once the first claim lapses, so the first claim's settle
	// arrives fenced on a version that is no longer stored.
	lapsed := base.Add(2 * time.Minute)
	won, err = store.Claim(ctx, "t_1", 1, "rep_b", lapsed, lapsed.Add(time.Minute))
	if err != nil || !won {
		t.Fatalf("Claim() after the lapse = %v, %v", won, err)
	}
	stale := mustGet(t, store, "t_1")
	stale.Phase, stale.LastError = review.PhaseFailed, "stale"
	if ok, err := store.Settle(ctx, stale, 1); err != nil || ok {
		t.Errorf("Settle() on the lapsed claim's version = %v, %v, want a refusal", ok, err)
	}
	if got := mustGet(t, store, "t_1"); got.Phase != "" || got.ClaimedBy != "rep_b" {
		t.Errorf("a refused settle wrote: phase %q, claimed by %q", got.Phase, got.ClaimedBy)
	}

	want := mustGet(t, store, "t_1")
	want.Phase, want.HeldBy, want.StatusState = review.PhaseSucceeded, "", "success"
	want.CommentSHA256, want.RecordedSHA256 = "abc", "def"
	want.ReportedAt, want.Done = lapsed.Add(time.Second), true
	want.Attempts, want.FailingSince, want.RetryAt = 2, lapsed, lapsed.Add(time.Hour)
	want.LastError = "kept"
	// A record an earlier release made takes its destination at its first report, and every record
	// keeps the status context its first status was set under, so a settle writes both.
	want.Provider, want.APIURL = trigger.ProviderGitHub, "https://api.github.com"
	want.Repository, want.StatusContext = "acme/infra", "switchtender/network"
	if ok, err := store.Settle(ctx, want, want.Version); err != nil || !ok {
		t.Fatalf("Settle() = %v, %v", ok, err)
	}
	want.Version++
	want.ClaimedBy, want.ClaimedUntil = "", time.Time{}
	if diff := cmp.Diff(want, mustGet(t, store, "t_1"), cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("settled record mismatch (-want +got):\n%s", diff)
	}
}

// claimers is how many goroutines race in the concurrency tests.
const claimers = 12

// testConcurrentClaims pins that many processes claiming the same version at once produce exactly
// one winner. It is the property the whole design rests on: one report, once, across replicas.
func testConcurrentClaims(t *testing.T, store review.Store) {
	ctx := context.Background()
	mustCreate(t, store, &review.Record{ID: "race", Kind: review.KindPlan, RunID: "race",
		TriggerID: "trg_r", PullRequest: 1, CreatedAt: base})
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		wins []string
		errs []error
	)
	for i := range claimers {
		wg.Add(1)
		go func(owner string) {
			defer wg.Done()
			won, err := store.Claim(ctx, "race", 0, owner, base, base.Add(time.Minute))
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
			}
			if won {
				wins = append(wins, owner)
			}
		}(fmt.Sprintf("rep_%d", i))
	}
	wg.Wait()
	if len(errs) > 0 {
		t.Fatalf("Claim() errors under contention: %v", errs)
	}
	if len(wins) != 1 {
		t.Fatalf("%d claimers won the same version, want exactly one: %v", len(wins), wins)
	}
	if got := mustGet(t, store, "race"); got.ClaimedBy != wins[0] || got.Version != 1 {
		t.Errorf("record held by %q at version %d, want the one winner %q at version 1",
			got.ClaimedBy, got.Version, wins[0])
	}
}

// testConcurrentLane pins that many processes claiming different records of one pull request at
// once produce exactly one winner, so two pushes in quick succession never have two processes
// writing the same comment.
func testConcurrentLane(t *testing.T, store review.Store) {
	ctx := context.Background()
	for i := range claimers {
		id := fmt.Sprintf("lane_%02d", i)
		mustCreate(t, store, &review.Record{ID: id, Kind: review.KindPlan, RunID: id,
			TriggerID: "trg_lane", PullRequest: 5, CreatedAt: base})
	}
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		wins []string
		errs []error
	)
	for i := range claimers {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			won, err := store.Claim(ctx, id, 0, "rep_"+id, base, base.Add(time.Minute))
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
			}
			if won {
				wins = append(wins, id)
			}
		}(fmt.Sprintf("lane_%02d", i))
	}
	wg.Wait()
	if len(errs) > 0 {
		t.Fatalf("Claim() errors under contention: %v", errs)
	}
	if len(wins) != 1 {
		t.Errorf("%d records of one pull request were claimed at once, want exactly one: %v",
			len(wins), wins)
	}
}

// runPurger is the retention side of a run store: deleting finished runs created before a cutoff.
type runPurger interface {
	// PurgeRunsBefore deletes terminal runs created before cutoff and returns how many it deleted.
	PurgeRunsBefore(ctx context.Context, cutoff time.Time) (int, error)
}

// PurgeFollowsRuns pins that retention deletes a review plan's report record with its run, and a
// refusal's record once it is finished and older than the cutoff, and keeps every record whose run
// is kept or whose report is still owed. Without it the records would outlive retention forever.
func PurgeFollowsRuns(t *testing.T, runs run.Store, reports review.Store) {
	t.Helper()
	ctx := context.Background()
	purger, ok := runs.(runPurger)
	if !ok {
		t.Fatalf("run store %T has no PurgeRunsBefore", runs)
	}
	cutoff := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	old, recent := cutoff.Add(-24*time.Hour), cutoff.Add(time.Minute)
	tests := []struct {
		// ID is the record id.
		ID string
		// Run is the run the record reports, nil for a refusal.
		Run *run.Run
		// Done is whether the record is finished.
		Done bool
		// CreatedAt is when a refusal's record was made.
		CreatedAt time.Time
		// WantKept is whether the record survives the purge.
		WantKept bool
	}{{ // Test 0: An old finished plan goes with its run.
		ID:   "run_rp_old",
		Run:  &run.Run{ID: "run_rp_old", Status: run.StatusSucceeded, CreatedAt: old},
		Done: true, WantKept: false,
	}, { // Test 1: A recent finished plan is kept with its run.
		ID:   "run_rp_new",
		Run:  &run.Run{ID: "run_rp_new", Status: run.StatusSucceeded, CreatedAt: recent},
		Done: true, WantKept: true,
	}, { // Test 2: An old plan still in flight keeps its run, and so its record.
		ID:   "run_rp_live",
		Run:  &run.Run{ID: "run_rp_live", Status: run.StatusPendingApproval, CreatedAt: old},
		Done: false, WantKept: true,
	}, { // Test 3: An old finished refusal goes.
		ID: "rfs_old_done", Done: true, CreatedAt: old, WantKept: false,
	}, { // Test 4: An old refusal still owed its report is kept.
		ID: "rfs_old_owed", Done: false, CreatedAt: old, WantKept: true,
	}, { // Test 5: A recent finished refusal is kept.
		ID: "rfs_new_done", Done: true, CreatedAt: recent, WantKept: true,
	}}
	for _, test := range tests {
		rec := &review.Record{ID: test.ID, Kind: review.KindFork, TriggerID: "trg_p",
			PullRequest: 1, Done: test.Done, CreatedAt: test.CreatedAt}
		if test.Run != nil {
			test.Run.Playbook, test.Run.Source = "site.yml", review.Source
			if err := runs.Save(ctx, test.Run); err != nil {
				t.Fatalf("Save(%s) error = %v", test.Run.ID, err)
			}
			rec = review.PlanRecord(test.Run.ID, lane("trg_p"), 1, test.Run.CreatedAt)
			rec.Done = test.Done
		}
		mustCreate(t, reports, rec)
	}
	if _, err := purger.PurgeRunsBefore(ctx, cutoff); err != nil {
		t.Fatalf("PurgeRunsBefore() error = %v", err)
	}
	for testNum, test := range tests {
		_, err := reports.Get(ctx, test.ID)
		kept := err == nil
		if err != nil && !errors.Is(err, review.ErrRecordNotFound) {
			t.Fatalf("test %d: Get() error = %v", testNum, err)
		}
		if kept != test.WantKept {
			t.Errorf("test %d: record %s kept = %v after the purge, want %v", testNum, test.ID,
				kept, test.WantKept)
		}
	}
}

// testUnrepresentableText pins that a record whose text carries a NUL byte or bytes that are not
// UTF-8 is created and settled on every backend, reading back the same cleaned text. The text is a
// forge's answer, a webhook's reason, or a policy's rule name: SQLite stored it and PostgreSQL
// refused it, so the same settle landed on one backend and left the record failing forever on the
// other.
func testUnrepresentableText(t *testing.T, store review.Store) {
	ctx := context.Background()
	const dirty = "bad\x00byte \xff\xfe end"
	rec := &review.Record{ID: "dirty", Kind: review.KindRefusal, TriggerID: "trg_d", PullRequest: 1,
		CommitSHA: "sha\x00", Reason: dirty, HeldBy: dirty, Repository: dirty, CreatedAt: base}
	mustCreate(t, store, rec)
	got := mustGet(t, store, "dirty")
	for name, value := range map[string]string{"reason": got.Reason, "held by": got.HeldBy,
		"repository": got.Repository, "commit": got.CommitSHA} {
		if !utf8.ValidString(value) || strings.ContainsRune(value, 0) {
			t.Errorf("the %s read back as %q, which PostgreSQL refuses", name, value)
		}
	}
	if won, err := store.Claim(ctx, "dirty", got.Version, "rep_a", base, base.Add(time.Minute)); err != nil ||
		!won {
		t.Fatalf("Claim() = %v, %v", won, err)
	}
	held := mustGet(t, store, "dirty")
	held.LastError, held.StatusContext = "forge answered: "+dirty, "switchtender/"+dirty
	if ok, err := store.Settle(ctx, held, held.Version); err != nil || !ok {
		t.Fatalf("Settle() with unrepresentable text = %v, %v, want it to land", ok, err)
	}
	settled := mustGet(t, store, "dirty")
	// A NUL becomes one replacement character, and so does a run of bytes that are not UTF-8.
	if settled.LastError != "forge answered: bad\ufffdbyte \ufffd end" ||
		settled.StatusContext != "switchtender/bad\ufffdbyte \ufffd end" {
		t.Errorf("settled text = %q, %q, want the unrepresentable bytes replaced",
			settled.LastError, settled.StatusContext)
	}
}
