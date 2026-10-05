package notificationtest

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/notification"
)

// base is the instant the delivery contract measures from, whole milliseconds so every store hands
// it back unchanged.
var base = time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)

// lease is the claim length the contract uses.
const lease = time.Minute

// event builds an event of a run with a snapshot that names it and its time, so two moments
// differ.
func event(runID, name, branch string, at time.Time) *notification.RunEvent {
	return &notification.RunEvent{RunID: runID, Event: name, Branch: branch,
		Snapshot: []byte(`{"id":"` + runID + `","moment":"` + name + "/" + branch + "/" +
			at.Format(time.RFC3339Nano) + `"}`),
		CreatedAt: at}
}

// to builds the recipients for target ids.
func to(ids ...string) []notification.Recipient {
	out := make([]notification.Recipient, 0, len(ids))
	for _, id := range ids {
		out = append(out, notification.Recipient{NotificationID: id, Name: "name " + id,
			Kind: "webhook"})
	}
	return out
}

// mustRecord records an event and fails the test when it is not recorded.
func mustRecord(t *testing.T, store notification.Store, ev *notification.RunEvent,
	recipients []notification.Recipient) {
	t.Helper()
	ok, err := store.Record(context.Background(), ev, recipients)
	if err != nil || !ok {
		t.Fatalf("Record(%s %s) = %v, %v, want recorded", ev.RunID, ev.Event, ok, err)
	}
}

// keys renders claimed deliveries as target/run/seq, sorted.
func keys(list []*notification.Delivery) []string {
	out := []string{}
	for _, d := range list {
		out = append(out, d.Key())
	}
	sort.Strings(out)
	return out
}

// finishAll records each claimed delivery as delivered.
func finishAll(t *testing.T, store notification.Store, owner string,
	list []*notification.Delivery, at time.Time) {
	t.Helper()
	for _, d := range list {
		err := store.Finish(context.Background(), d, owner, notification.Outcome{
			Status: notification.DeliveryDelivered, At: at})
		if err != nil {
			t.Fatalf("Finish(%s) error = %v", d.Key(), err)
		}
	}
}

// testRecord verifies a run's events take consecutive sequence numbers in the order they are
// recorded, a moment recorded twice is kept once, two different moments of the same event are both
// kept, and a skipped recipient is recorded finished with its reason.
func testRecord(t *testing.T, store notification.Store) {
	ctx := context.Background()
	started := event("run_r", "started", "", base)
	mustRecord(t, store, started, to("ntf_a"))
	held := event("run_r", "approval", "deploy", base.Add(time.Second))
	mustRecord(t, store, held, to("ntf_a", "ntf_b"))
	again := event("run_r", "approval", "deploy", base.Add(2*time.Second))
	again.Snapshot = held.Snapshot
	ok, err := store.Record(ctx, again, to("ntf_a"))
	if err != nil || ok {
		t.Errorf("Record() of the same moment again = %v, %v, want not recorded", ok, err)
	}
	other := event("run_r", "approval", "verify", base.Add(3*time.Second))
	mustRecord(t, store, other, []notification.Recipient{{NotificationID: "ntf_a", Name: "a",
		Kind: "slack", Skip: "waiting for its secret"}})
	mustRecord(t, store, event("run_s", "started", "", base), to("ntf_a"))

	if diff := cmp.Diff([]int64{1, 2, 3}, []int64{started.Seq, held.Seq, other.Seq}); diff != "" {
		t.Errorf("sequence numbers mismatch (-want +got):\n%s", diff)
	}
	got, err := store.Deliveries(ctx, notification.DeliveryFilter{RunID: "run_r"})
	if err != nil {
		t.Fatalf("Deliveries() error = %v", err)
	}
	skippedAt := base.Add(3 * time.Second)
	want := []*notification.Delivery{{
		NotificationID: "ntf_a", RunID: "run_r", Seq: 3, Event: "approval", Branch: "verify",
		TargetName: "a", TargetKind: "slack", Status: notification.DeliverySkipped,
		NextAttemptAt: skippedAt, LastError: "waiting for its secret", CreatedAt: skippedAt,
		FinishedAt: &skippedAt,
	}, {
		NotificationID: "ntf_a", RunID: "run_r", Seq: 2, Event: "approval", Branch: "deploy",
		TargetName: "name ntf_a", TargetKind: "webhook", Status: notification.DeliveryPending,
		NextAttemptAt: base.Add(time.Second), CreatedAt: base.Add(time.Second),
	}, {
		NotificationID: "ntf_b", RunID: "run_r", Seq: 2, Event: "approval", Branch: "deploy",
		TargetName: "name ntf_b", TargetKind: "webhook", Status: notification.DeliveryPending,
		NextAttemptAt: base.Add(time.Second), CreatedAt: base.Add(time.Second),
	}, {
		NotificationID: "ntf_a", RunID: "run_r", Seq: 1, Event: "started",
		TargetName: "name ntf_a", TargetKind: "webhook", Status: notification.DeliveryPending,
		NextAttemptAt: base, CreatedAt: base,
	}}
	if diff := cmp.Diff(want, got, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("Deliveries() mismatch (-want +got):\n%s", diff)
	}
}

// testClaimOrder verifies a delivery waits for every earlier delivery to the same target for the
// same run of the run's own lifecycle, of its own workflow step, and of the steps it comes after,
// and for nothing else: a step that runs beside it, another target, and another run never hold it.
func testClaimOrder(t *testing.T, store notification.Store) {
	ctx := context.Background()
	at := base.Add(time.Hour)
	after := func(ev *notification.RunEvent, follows ...string) *notification.RunEvent {
		ev.Follows = follows
		return ev
	}
	mustRecord(t, store, event("run_w", "started", "", base), to("ntf_a", "ntf_b"))
	mustRecord(t, store, event("run_w", "approval", "left", base.Add(time.Second)), to("ntf_a"))
	mustRecord(t, store, event("run_w", "approval", "right", base.Add(2*time.Second)),
		to("ntf_a"))
	mustRecord(t, store, after(event("run_w", "approval", "deploy", base.Add(3*time.Second)),
		"left"), to("ntf_a"))
	mustRecord(t, store, event("run_w", "failure", "", base.Add(4*time.Second)), to("ntf_a"))
	mustRecord(t, store, event("run_x", "started", "", base), to("ntf_a"))

	tests := []struct {
		WantClaimed []string
		Finish      []string
	}{{ // Test 0: The first of each run for each target.
		WantClaimed: []string{"ntf_a/run_w/1", "ntf_a/run_x/1", "ntf_b/run_w/1"},
		Finish:      []string{"ntf_a/run_w/1", "ntf_a/run_x/1", "ntf_b/run_w/1"},
	}, { // Test 1: Both branches go at once, and deploy waits for left, which it comes after.
		WantClaimed: []string{"ntf_a/run_w/2", "ntf_a/run_w/3"},
		Finish:      []string{"ntf_a/run_w/2"},
	}, { // Test 2: Deploy goes once left is done, though right, beside it, is still in flight.
		WantClaimed: []string{"ntf_a/run_w/4"},
		Finish:      []string{"ntf_a/run_w/3", "ntf_a/run_w/4"},
	}, { // Test 3: The run's own end follows every branch.
		WantClaimed: []string{"ntf_a/run_w/5"},
		Finish:      []string{"ntf_a/run_w/5"},
	}, { // Test 4: Nothing is left.
		WantClaimed: []string{},
	}}
	held := map[string]*notification.Delivery{}
	for testNum, test := range tests {
		claimed, err := store.Claim(ctx, "worker", at, lease, 100, 0)
		if err != nil {
			t.Fatalf("test %d: Claim() error = %v", testNum, err)
		}
		if diff := cmp.Diff(test.WantClaimed, keys(claimed)); diff != "" {
			t.Fatalf("test %d: Claim() mismatch (-want +got):\n%s", testNum, diff)
		}
		again, err := store.Claim(ctx, "other", at, lease, 100, 0)
		if err != nil || len(again) != 0 {
			t.Fatalf("test %d: a second claimant took %v, %v while the first held them",
				testNum, keys(again), err)
		}
		for _, d := range claimed {
			if len(d.Snapshot) == 0 {
				t.Errorf("test %d: claimed %s without its snapshot", testNum, d.Key())
			}
			held[d.Key()] = d
		}
		var done []*notification.Delivery
		for _, key := range test.Finish {
			done = append(done, held[key])
		}
		finishAll(t, store, "worker", done, at)
	}
}

// testFinish verifies a retried delivery waits for its next attempt and holds back what follows,
// a delivery that fails for good is kept and stops holding anything back, the next delivery to the
// same target for the same run is told an earlier one failed, a claim that lapsed can be taken by
// another worker, and the first worker can then no longer record an outcome.
func testFinish(t *testing.T, store notification.Store) {
	ctx := context.Background()
	mustRecord(t, store, event("run_f", "started", "", base), to("ntf_a"))
	mustRecord(t, store, event("run_f", "success", "", base.Add(time.Second)), to("ntf_a"))

	first, err := store.Claim(ctx, "w1", base, lease, 10, 0)
	if err != nil || len(first) != 1 {
		t.Fatalf("Claim() = %v, %v, want the first delivery", keys(first), err)
	}
	retryAt := base.Add(time.Minute)
	if err := store.Finish(ctx, first[0], "w1", notification.Outcome{
		Status: notification.DeliveryPending, Error: "the target answered 503", At: base,
		NextAttemptAt: retryAt}); err != nil {
		t.Fatalf("Finish(retry) error = %v", err)
	}
	if early, err := store.Claim(ctx, "w1", base.Add(time.Second), lease, 10, 0); err != nil ||
		len(early) != 0 {
		t.Fatalf("Claim() before the retry is due = %v, %v, want nothing, the success waiting",
			keys(early), err)
	}
	second, err := store.Claim(ctx, "w1", retryAt, lease, 10, 0)
	if err != nil || len(second) != 1 || second[0].Attempts != 1 {
		t.Fatalf("Claim() when the retry is due = %+v, %v, want the first again after 1 attempt",
			second, err)
	}
	// The claim lapses unrecorded, as when a worker dies mid-attempt, and another worker takes it.
	afterLapse := retryAt.Add(2 * lease)
	stolen, err := store.Claim(ctx, "w2", afterLapse, lease, 10, 0)
	if err != nil || len(stolen) != 1 {
		t.Fatalf("Claim() after the lease lapsed = %v, %v, want the first delivery",
			keys(stolen), err)
	}
	err = store.Finish(ctx, second[0], "w1", notification.Outcome{
		Status: notification.DeliveryDelivered, At: afterLapse})
	if !errors.Is(err, notification.ErrDeliveryLost) {
		t.Errorf("Finish() by the worker whose claim lapsed = %v, want ErrDeliveryLost", err)
	}
	if err := store.Finish(ctx, stolen[0], "w2", notification.Outcome{
		Status: notification.DeliveryFailed, Error: "the target answered 404",
		At: afterLapse}); err != nil {
		t.Fatalf("Finish(failed) error = %v", err)
	}
	next, err := store.Claim(ctx, "w2", afterLapse, lease, 10, 0)
	if err != nil || len(next) != 1 || next[0].Seq != 2 || !next[0].EarlierFailed {
		t.Fatalf("Claim() after the failure = %+v, %v, want the success marked earlier-failed",
			next, err)
	}
	if err := store.Finish(ctx, next[0], "w2", notification.Outcome{
		Status: notification.DeliveryDelivered, Note: notification.EarlierFailedNote,
		At: afterLapse}); err != nil {
		t.Fatalf("Finish(delivered) error = %v", err)
	}
	got, err := store.Deliveries(ctx, notification.DeliveryFilter{NotificationID: "ntf_a"})
	if err != nil {
		t.Fatalf("Deliveries() error = %v", err)
	}
	want := []*notification.Delivery{{
		NotificationID: "ntf_a", RunID: "run_f", Seq: 2, Event: "success",
		TargetName: "name ntf_a", TargetKind: "webhook", Status: notification.DeliveryDelivered,
		Attempts: 1, NextAttemptAt: base.Add(time.Second), Note: notification.EarlierFailedNote,
		CreatedAt: base.Add(time.Second), FinishedAt: &afterLapse,
	}, {
		NotificationID: "ntf_a", RunID: "run_f", Seq: 1, Event: "started",
		TargetName: "name ntf_a", TargetKind: "webhook", Status: notification.DeliveryFailed,
		Attempts: 2, NextAttemptAt: retryAt, LastError: "the target answered 404",
		CreatedAt: base, FinishedAt: &afterLapse,
	}}
	if diff := cmp.Diff(want, got, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("Deliveries() mismatch (-want +got):\n%s", diff)
	}
}

// testClaimOnce verifies that claimants racing over many deliveries take each exactly once.
func testClaimOnce(t *testing.T, store notification.Store) {
	ctx := context.Background()
	const runs = 40
	for i := range runs {
		mustRecord(t, store, event(fmt.Sprintf("run_%02d", i), "started", "", base),
			to("ntf_a", "ntf_b"))
	}
	var (
		mu    sync.Mutex
		taken = map[string]string{}
		wg    sync.WaitGroup
	)
	for w := range 4 {
		owner := fmt.Sprintf("w%d", w)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 20 {
				claimed, err := store.Claim(ctx, owner, base, lease, 7, 0)
				if err != nil {
					t.Errorf("Claim(%s) error = %v", owner, err)
					return
				}
				mu.Lock()
				for _, d := range claimed {
					if by, dup := taken[d.Key()]; dup {
						t.Errorf("%s claimed by %s and %s", d.Key(), by, owner)
					}
					taken[d.Key()] = owner
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(taken) != 2*runs {
		t.Errorf("claimants took %d deliveries, want %d", len(taken), 2*runs)
	}
}

// testDeliveries verifies a listing is newest first, filters by run, target, status, and time,
// and honors its limit.
func testDeliveries(t *testing.T, store notification.Store) {
	ctx := context.Background()
	for i, id := range []string{"run_1", "run_2", "run_3"} {
		mustRecord(t, store, event(id, "started", "", base.Add(time.Duration(i)*time.Minute)),
			to("ntf_a", "ntf_b"))
	}
	tests := []struct {
		In       notification.DeliveryFilter
		WantKeys []string
	}{{ // Test 0: Everything, newest first.
		In: notification.DeliveryFilter{},
		WantKeys: []string{"ntf_a/run_3/1", "ntf_b/run_3/1", "ntf_a/run_2/1", "ntf_b/run_2/1",
			"ntf_a/run_1/1", "ntf_b/run_1/1"},
	}, { // Test 1: One run.
		In: notification.DeliveryFilter{RunID: "run_2"}, WantKeys: []string{"ntf_a/run_2/1",
			"ntf_b/run_2/1"},
	}, { // Test 2: One target since a time.
		In: notification.DeliveryFilter{NotificationID: "ntf_b",
			Since: base.Add(time.Minute)},
		WantKeys: []string{"ntf_b/run_3/1", "ntf_b/run_2/1"},
	}, { // Test 3: A status nothing is in.
		In: notification.DeliveryFilter{Status: notification.DeliveryFailed}, WantKeys: []string{},
	}, { // Test 4: A limit.
		In: notification.DeliveryFilter{Limit: 2}, WantKeys: []string{"ntf_a/run_3/1",
			"ntf_b/run_3/1"},
	}}
	for testNum, test := range tests {
		got, err := store.Deliveries(ctx, test.In)
		if err != nil {
			t.Fatalf("test %d: Deliveries() error = %v", testNum, err)
		}
		gotKeys := []string{}
		for _, d := range got {
			gotKeys = append(gotKeys, d.Key())
		}
		if diff := cmp.Diff(test.WantKeys, gotKeys); diff != "" {
			t.Errorf("test %d: Deliveries() mismatch (-want +got):\n%s", testNum, diff)
		}
	}
}

// testListAndDetachObject verifies every attachment lists oldest first and detaching an object
// removes its attachments alone and summarizes what it removed.
func testListAndDetachObject(t *testing.T, store notification.Store) {
	ctx := context.Background()
	created := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	for _, id := range []string{"ntf_x", "ntf_y"} {
		if err := store.Save(ctx, target(id, created)); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
	}
	for i, a := range []*notification.Attachment{
		attach("nta_1", "ntf_y", "schedule", "sch_1", "failure", created),
		attach("nta_2", "ntf_x", "schedule", "sch_1", "failure", created.Add(time.Minute)),
		attach("nta_3", "ntf_x", "schedule", "sch_1", "success", created.Add(2*time.Minute)),
		attach("nta_4", "ntf_x", "template", "tpl_1", "failure", created.Add(3*time.Minute)),
	} {
		if err := store.Attach(ctx, a); err != nil {
			t.Fatalf("Attach(%d) error = %v", i, err)
		}
	}
	all, err := store.ListAttachments(ctx)
	if err != nil {
		t.Fatalf("ListAttachments() error = %v", err)
	}
	ids := []string{}
	for _, a := range all {
		ids = append(ids, a.ID)
	}
	if diff := cmp.Diff([]string{"nta_1", "nta_2", "nta_3", "nta_4"}, ids); diff != "" {
		t.Errorf("ListAttachments() mismatch (-want +got):\n%s", diff)
	}
	cleaned, err := store.DetachObject(ctx, "schedule", "sch_1")
	if err != nil {
		t.Fatalf("DetachObject() error = %v", err)
	}
	want := notification.Cleanup{Removed: 3, Targets: []string{"ntf_x", "ntf_y"}}
	if diff := cmp.Diff(want, cleaned); diff != "" {
		t.Errorf("DetachObject() mismatch (-want +got):\n%s", diff)
	}
	left, err := store.ListAttachments(ctx)
	if err != nil || len(left) != 1 || left[0].ID != "nta_4" {
		t.Errorf("attachments after DetachObject = %v, %v, want only nta_4", left, err)
	}
	for _, id := range []string{"ntf_x", "ntf_y"} {
		if _, err := store.Get(ctx, id); err != nil {
			t.Errorf("DetachObject() removed target %s: %v", id, err)
		}
	}
	none, err := store.DetachObject(ctx, "schedule", "sch_1")
	if err != nil || !none.Equal(notification.Cleanup{}) {
		t.Errorf("DetachObject() of an object with none = %+v, %v, want nothing removed", none, err)
	}
}

// testRecordOneEnd verifies a run's end is recorded once whatever copy of the run announces it,
// while an end on a workflow branch, another run's end, and a run's other events are unaffected.
func testRecordOneEnd(t *testing.T, store notification.Store) {
	ctx := context.Background()
	mustRecord(t, store, event("run_e", "started", "", base), to("ntf_a"))
	mustRecord(t, store, event("run_e", "success", "", base.Add(time.Second)), to("ntf_a"))
	tests := []struct {
		In           *notification.RunEvent
		WantRecorded bool
	}{{ // Test 0: The same end again from another copy of the run.
		In: event("run_e", "success", "", base.Add(2*time.Second)),
	}, { // Test 1: A different end for a run that already ended.
		In: event("run_e", "failure", "", base.Add(3*time.Second)),
	}, { // Test 2: An event named like an end on a workflow branch is not the run's end.
		In: event("run_e", "failure", "deploy", base.Add(4*time.Second)), WantRecorded: true,
	}, { // Test 3: Another run's end.
		In: event("run_f", "failure", "", base.Add(5*time.Second)), WantRecorded: true,
	}}
	for testNum, test := range tests {
		recorded, err := store.Record(ctx, test.In, to("ntf_a"))
		if err != nil {
			t.Fatalf("test %d: Record() error = %v", testNum, err)
		}
		if recorded != test.WantRecorded {
			t.Errorf("test %d: Record(%s %s on %q) recorded = %v, want %v", testNum,
				test.In.RunID, test.In.Event, test.In.Branch, recorded, test.WantRecorded)
		}
	}
	got, err := store.Deliveries(ctx, notification.DeliveryFilter{RunID: "run_e"})
	if err != nil {
		t.Fatalf("Deliveries() error = %v", err)
	}
	if diff := cmp.Diff([]string{"ntf_a/run_e/1", "ntf_a/run_e/2", "ntf_a/run_e/3"},
		keys(got)); diff != "" {
		t.Errorf("deliveries of run_e mismatch (-want +got):\n%s", diff)
	}
}

// testClaimPerTarget verifies a claimant is given at most its share of one target's deliveries,
// counting what it already holds, while every other target's deliveries are still claimed, and that
// the share is per claimant.
func testClaimPerTarget(t *testing.T, store notification.Store) {
	ctx := context.Background()
	for i := range 4 {
		mustRecord(t, store, event(fmt.Sprintf("run_%d", i), "started", "", base),
			to("ntf_hung", "ntf_live"))
	}
	tests := []struct {
		Owner       string
		Finish      bool
		WantClaimed []string
	}{{ // Test 0: Two of each target, though more are due.
		Owner: "w1",
		WantClaimed: []string{"ntf_hung/run_0/1", "ntf_hung/run_1/1", "ntf_live/run_0/1",
			"ntf_live/run_1/1"},
	}, { // Test 1: Holding its share of both, the claimant takes nothing more.
		Owner: "w1", WantClaimed: []string{},
	}, { // Test 2: Another claimant has a share of its own.
		Owner: "w2",
		WantClaimed: []string{"ntf_hung/run_2/1", "ntf_hung/run_3/1", "ntf_live/run_2/1",
			"ntf_live/run_3/1"},
	}}
	held := map[string][]*notification.Delivery{}
	for testNum, test := range tests {
		claimed, err := store.Claim(ctx, test.Owner, base, lease, 10, 2)
		if err != nil {
			t.Fatalf("test %d: Claim() error = %v", testNum, err)
		}
		if diff := cmp.Diff(test.WantClaimed, keys(claimed)); diff != "" {
			t.Fatalf("test %d: Claim(%s) mismatch (-want +got):\n%s", testNum, test.Owner, diff)
		}
		held[test.Owner] = append(held[test.Owner], claimed...)
	}
	// One of w1's live attempts ends, which frees one place for that target and none for the other.
	var live []*notification.Delivery
	for _, d := range held["w1"] {
		if d.NotificationID == "ntf_live" {
			live = append(live, d)
		}
	}
	finishAll(t, store, "w1", live[:1], base)
	mustRecord(t, store, event("run_4", "started", "", base), to("ntf_hung", "ntf_live"))
	claimed, err := store.Claim(ctx, "w1", base, lease, 10, 2)
	if err != nil {
		t.Fatalf("Claim() after an attempt ended error = %v", err)
	}
	if diff := cmp.Diff([]string{"ntf_live/run_4/1"}, keys(claimed)); diff != "" {
		t.Errorf("Claim() after one live attempt ended mismatch (-want +got):\n%s", diff)
	}
}

// testReleaseAndSkip verifies a released delivery counts no attempt, keeps what its last attempt
// recorded, and is due again at once for any claimant, that only its holder can release it, and
// that a skipped delivery is finished without holding back what follows or marking it as following
// a failure.
func testReleaseAndSkip(t *testing.T, store notification.Store) {
	ctx := context.Background()
	mustRecord(t, store, event("run_g", "started", "", base), to("ntf_a"))
	mustRecord(t, store, event("run_g", "success", "", base.Add(time.Second)), to("ntf_a"))
	first, err := store.Claim(ctx, "w1", base, lease, 10, 0)
	if err != nil || len(first) != 1 {
		t.Fatalf("Claim() = %v, %v, want the first delivery", keys(first), err)
	}
	retryAt := base.Add(time.Minute)
	if err := store.Finish(ctx, first[0], "w1", notification.Outcome{
		Status: notification.DeliveryPending, Error: "the target answered 503", At: base,
		NextAttemptAt: retryAt}); err != nil {
		t.Fatalf("Finish(retry) error = %v", err)
	}
	second, err := store.Claim(ctx, "w1", retryAt, lease, 10, 0)
	if err != nil || len(second) != 1 {
		t.Fatalf("Claim() when the retry is due = %v, %v, want the first again", keys(second), err)
	}
	if err := store.Release(ctx, second[0], "w2"); !errors.Is(err, notification.ErrDeliveryLost) {
		t.Errorf("Release() by a claimant that does not hold it = %v, want ErrDeliveryLost", err)
	}
	if err := store.Release(ctx, second[0], "w1"); err != nil {
		t.Fatalf("Release() error = %v", err)
	}
	again, err := store.Claim(ctx, "w2", retryAt, lease, 10, 0)
	if err != nil || len(again) != 1 {
		t.Fatalf("Claim() after the release = %v, %v, want the delivery at once", keys(again), err)
	}
	got := fmt.Sprintf("%s %d %s %s", again[0].Status, again[0].Attempts, again[0].LastError,
		again[0].NextAttemptAt.Format(time.RFC3339))
	want := fmt.Sprintf("pending 1 the target answered 503 %s", retryAt.Format(time.RFC3339))
	if got != want {
		t.Errorf("released delivery = %q, want %q: a release counts no attempt", got, want)
	}
	if err := store.Finish(ctx, again[0], "w2", notification.Outcome{
		Status: notification.DeliverySkipped, Error: "not sent: the target was changed",
		At: retryAt}); err != nil {
		t.Fatalf("Finish(skipped) error = %v", err)
	}
	next, err := store.Claim(ctx, "w2", retryAt, lease, 10, 0)
	if err != nil || len(next) != 1 || next[0].Seq != 2 || next[0].EarlierFailed {
		t.Fatalf("Claim() after the skip = %+v, %v, want the success, not marked earlier-failed",
			next, err)
	}
	list, err := store.Deliveries(ctx, notification.DeliveryFilter{RunID: "run_g",
		Status: notification.DeliverySkipped})
	if err != nil || len(list) != 1 || list[0].FinishedAt == nil ||
		list[0].LastError != "not sent: the target was changed" {
		t.Errorf("skipped deliveries = %+v, %v, want the first, finished with its reason", list, err)
	}
}
