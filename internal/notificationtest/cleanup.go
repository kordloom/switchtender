package notificationtest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/notification"
)

// Attachable is one object kind a notification target can be attached to, as the backend under
// test provides it: a way to create an object of the kind, the store's ordinary delete, and its
// delete that holds to a recorded cleanup.
type Attachable struct {
	// Create stores an object of the kind with the given id.
	Create func(ctx context.Context, id string) error
	// Delete removes the object through the kind's store, the way every delete path does.
	Delete func(ctx context.Context, id string) error
	// Recorded is the kind's store as a RecordedDeleter.
	Recorded notification.RecordedDeleter
}

// CleanupContract proves, for every kind notification.Kinds names, that deleting an object of the
// kind removes its notification attachments in the same transaction and leaves the targets and
// every other object's attachments alone, that a failed delete removes nothing, and that a delete
// held to a recorded cleanup which no longer matches is refused whole.
//
// newHarness returns a fresh store and the attachable kinds the backend provides. A kind it does
// not provide fails the contract. That is the point: attachments name their object by kind and id,
// so no foreign key cascades them, and a kind added to notification.Kinds without its cleanup on a
// backend fails here on that backend rather than leaving attachments to deleted objects behind.
func CleanupContract(t *testing.T,
	newHarness func(t *testing.T) (notification.Store, map[string]Attachable)) {
	t.Helper()
	for _, kind := range notification.Kinds() {
		t.Run(kind, func(t *testing.T) {
			store, kinds := newHarness(t)
			a, ok := kinds[kind]
			if !ok || a.Create == nil || a.Delete == nil || a.Recorded == nil {
				t.Fatalf("the backend provides no delete cleanup for the attachable kind %q: "+
					"deleting one would leave its notification attachments behind", kind)
			}
			testKindCleanup(t, store, kind, a)
		})
	}
}

// testKindCleanup runs the cleanup contract for one attachable kind.
func testKindCleanup(t *testing.T, store notification.Store, kind string, a Attachable) {
	ctx := context.Background()
	at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	for _, id := range []string{"ntf_1", "ntf_2"} {
		if err := store.Save(ctx, target(id, at)); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
	}
	for _, id := range []string{"obj_a", "obj_b", "obj_c"} {
		if err := a.Create(ctx, id); err != nil {
			t.Fatalf("Create(%s) error = %v", id, err)
		}
	}
	for i, att := range []*notification.Attachment{
		attach("nta_a1", "ntf_1", kind, "obj_a", "failure", at),
		attach("nta_a2", "ntf_2", kind, "obj_a", "success", at.Add(time.Second)),
		attach("nta_b1", "ntf_1", kind, "obj_b", "failure", at.Add(2*time.Second)),
		attach("nta_c1", "ntf_1", kind, "obj_c", "failure", at.Add(3*time.Second)),
		// An attachment to an object that does not exist, the orphan the doctor reports.
		attach("nta_x1", "ntf_2", kind, "obj_gone", "failure", at.Add(4*time.Second)),
	} {
		if err := store.Attach(ctx, att); err != nil {
			t.Fatalf("Attach(%d) error = %v", i, err)
		}
	}
	count := func(object string) int {
		t.Helper()
		list, err := store.AttachedTo(ctx, kind, object)
		if err != nil {
			t.Fatalf("AttachedTo(%s) error = %v", object, err)
		}
		return len(list)
	}

	if err := a.Delete(ctx, "obj_a"); err != nil {
		t.Fatalf("Delete(obj_a) error = %v", err)
	}
	got := map[string]int{"obj_a": count("obj_a"), "obj_b": count("obj_b"),
		"obj_c": count("obj_c"), "obj_gone": count("obj_gone")}
	want := map[string]int{"obj_a": 0, "obj_b": 1, "obj_c": 1, "obj_gone": 1}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("attachments after deleting obj_a mismatch (-want +got):\n%s", diff)
	}
	for _, id := range []string{"ntf_1", "ntf_2"} {
		if _, err := store.Get(ctx, id); err != nil {
			t.Errorf("deleting an object removed target %s: %v", id, err)
		}
	}

	// A delete that fails removes nothing, attachments included: they go in the same transaction.
	if err := a.Delete(ctx, "obj_gone"); err == nil {
		t.Errorf("Delete(obj_gone) of an object that does not exist succeeded")
	}
	if n := count("obj_gone"); n != 1 {
		t.Errorf("a failed delete removed %d attachments, want none", 1-n)
	}

	// The chain entry recorded no attachments, and obj_c has one: refused, nothing removed.
	err := a.Recorded.DeleteRecorded(ctx, "obj_c", notification.Cleanup{})
	if !errors.Is(err, notification.ErrCleanupChanged) {
		t.Errorf("DeleteRecorded() with a stale record = %v, want ErrCleanupChanged", err)
	}
	if n := count("obj_c"); n != 1 {
		t.Errorf("a refused delete removed attachments: %d left, want 1", n)
	}
	recorded := notification.Cleanup{Removed: 1, Targets: []string{"ntf_1"}}
	if err := a.Recorded.DeleteRecorded(ctx, "obj_c", recorded); err != nil {
		t.Fatalf("DeleteRecorded() with the matching record error = %v; a refused delete must "+
			"leave the object in place", err)
	}
	if n := count("obj_c"); n != 0 {
		t.Errorf("DeleteRecorded() left %d attachments, want none", n)
	}
	if err := a.Recorded.DeleteRecorded(ctx, "obj_c", notification.Cleanup{}); err == nil {
		t.Errorf("DeleteRecorded() of an object already deleted succeeded")
	}
}
