// Package notificationtest provides a shared behavior contract for notification.Store
// implementations so the in-memory, SQLite, and PostgreSQL backends cannot drift apart.
package notificationtest

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/notification"
)

// Contract runs the notification.Store contract against a fresh store from newStore.
func Contract(t *testing.T, newStore func() notification.Store) {
	t.Helper()
	t.Run("target round trips with its sealed secrets", func(t *testing.T) {
		testRoundTrip(t, newStore())
	})
	t.Run("update refuses a deleted target", func(t *testing.T) { testUpdate(t, newStore()) })
	t.Run("list is ordered and non-nil", func(t *testing.T) { testList(t, newStore()) })
	t.Run("attachments by target and by object", func(t *testing.T) {
		testAttachments(t, newStore())
	})
	t.Run("delete takes the attachments with it", func(t *testing.T) {
		testDeleteCascades(t, newStore())
	})
	t.Run("every attachment lists and an object detaches", func(t *testing.T) {
		testListAndDetachObject(t, newStore())
	})
	t.Run("a run's events are numbered in order and recorded once", func(t *testing.T) {
		testRecord(t, newStore())
	})
	t.Run("delivery follows each target's order per run", func(t *testing.T) {
		testClaimOrder(t, newStore())
	})
	t.Run("a failed delivery is kept and later ones carry a note", func(t *testing.T) {
		testFinish(t, newStore())
	})
	t.Run("concurrent claimants never take one delivery twice", func(t *testing.T) {
		testClaimOnce(t, newStore())
	})
	t.Run("deliveries list newest first and filter", func(t *testing.T) {
		testDeliveries(t, newStore())
	})
	t.Run("a run's end is recorded once", func(t *testing.T) { testRecordOneEnd(t, newStore()) })
	t.Run("a claimant holds at most its share of one target", func(t *testing.T) {
		testClaimPerTarget(t, newStore())
	})
	t.Run("a release counts no attempt and a skip holds nothing back", func(t *testing.T) {
		testReleaseAndSkip(t, newStore())
	})
}

// target returns a webhook target with sealed values standing in for real ciphertext.
func target(id string, created time.Time) *notification.Notification {
	return &notification.Notification{
		ID: id, Name: "ops " + id, Description: "pages ops", OrgID: "org_ops", Kind: "grafana",
		URLHint: "https://grafana.example.com…", KeySet: true, SealedURL: "sealed-url-" + id,
		SealedKey: "sealed-key-" + id, CreatedAt: created, CreatedBy: "admin",
	}
}

// testRoundTrip verifies every field, the sealed secrets above all, survives a store round trip.
// A backend that drops a sealed column stores a target that delivers nowhere, silently.
func testRoundTrip(t *testing.T, store notification.Store) {
	ctx := context.Background()
	created := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		In *notification.Notification
	}{{ // Test 0: A target with a sealed address and key.
		In: target("ntf_a", created),
	}, { // Test 1: An imported shell still waiting for its secret.
		In: &notification.Notification{ID: "ntf_b", Name: "slack", Kind: "slack",
			NeedsSecret: true, CreatedAt: created},
	}, { // Test 2: A recipient-only target, which carries no secret at all.
		In: &notification.Notification{ID: "ntf_c", Name: "mail", Kind: "email",
			To: "ops@example.com", CreatedAt: created},
	}}
	for testNum, test := range tests {
		if err := store.Save(ctx, test.In); err != nil {
			t.Fatalf("test %d: Save() error = %v", testNum, err)
		}
		got, err := store.Get(ctx, test.In.ID)
		if err != nil {
			t.Fatalf("test %d: Get() error = %v", testNum, err)
		}
		if diff := cmp.Diff(test.In, got, cmpopts.EquateEmpty()); diff != "" {
			t.Errorf("test %d: Get() mismatch (-want +got):\n%s", testNum, diff)
		}
	}
	if _, err := store.Get(ctx, "ntf_missing"); !errors.Is(err, notification.ErrNotFound) {
		t.Errorf("Get() of a missing target = %v, want ErrNotFound", err)
	}
}

// testUpdate verifies an edit replaces a target and refuses one that is gone.
func testUpdate(t *testing.T, store notification.Store) {
	ctx := context.Background()
	n := target("ntf_u", time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))
	if err := store.Save(ctx, n); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	n.Name, n.SealedURL, n.KeySet, n.SealedKey = "renamed", "resealed", false, ""
	if err := store.Update(ctx, n); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	got, err := store.Get(ctx, "ntf_u")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if diff := cmp.Diff(n, got, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("Get() after Update mismatch (-want +got):\n%s", diff)
	}
	gone := target("ntf_gone", time.Now())
	if err := store.Update(ctx, gone); !errors.Is(err, notification.ErrNotFound) {
		t.Errorf("Update() of a missing target = %v, want ErrNotFound", err)
	}
	if _, err := store.Get(ctx, "ntf_gone"); !errors.Is(err, notification.ErrNotFound) {
		t.Errorf("Update() of a missing target created it: Get() = %v", err)
	}
}

// testList verifies targets list oldest first and an empty store lists as an empty slice.
func testList(t *testing.T, store notification.Store) {
	ctx := context.Background()
	empty, err := store.List(ctx)
	if err != nil || empty == nil {
		t.Fatalf("List() on an empty store = %v, %v, want a non-nil empty slice", empty, err)
	}
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	for i, id := range []string{"ntf_3", "ntf_1", "ntf_2"} {
		created := base.Add(time.Duration([]int{2, 0, 1}[i]) * time.Hour)
		if err := store.Save(ctx, target(id, created)); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
	}
	list, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	var ids []string
	for _, n := range list {
		ids = append(ids, n.ID)
	}
	if diff := cmp.Diff([]string{"ntf_1", "ntf_2", "ntf_3"}, ids); diff != "" {
		t.Errorf("List() order mismatch (-want +got):\n%s", diff)
	}
}

// attach builds an attachment.
func attach(id, ntf, kind, object, event string, at time.Time) *notification.Attachment {
	return &notification.Attachment{ID: id, NotificationID: ntf, ObjectKind: kind,
		ObjectID: object, Event: event, CreatedAt: at, CreatedBy: "admin"}
}

// testAttachments verifies attachments store, refuse a duplicate, list by target and by object, and
// detach.
func testAttachments(t *testing.T, store notification.Store) {
	ctx := context.Background()
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	for _, id := range []string{"ntf_x", "ntf_y"} {
		if err := store.Save(ctx, target(id, base)); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
	}
	tests := []struct {
		Want error
		In   *notification.Attachment
	}{{ // Test 0: A first attachment.
		In: attach("nta_1", "ntf_x", "template", "tpl_1", "failure", base),
	}, { // Test 1: The same target on the same object for another event.
		In: attach("nta_2", "ntf_x", "template", "tpl_1", "success", base.Add(time.Minute)),
	}, { // Test 2: Another target on the same object.
		In: attach("nta_3", "ntf_y", "template", "tpl_1", "failure", base.Add(2*time.Minute)),
	}, { // Test 3: The same target on another object.
		In: attach("nta_4", "ntf_x", "org", "org_1", "approval", base.Add(3*time.Minute)),
	}, { // Test 4: An exact repeat is refused, so a target is never told twice.
		In:   attach("nta_5", "ntf_x", "template", "tpl_1", "failure", base.Add(4*time.Minute)),
		Want: notification.ErrDuplicate,
	}}
	for testNum, test := range tests {
		if err := store.Attach(ctx, test.In); !errors.Is(err, test.Want) {
			t.Fatalf("test %d: Attach() error = %v, want %v", testNum, err, test.Want)
		}
	}
	ids := func(list []*notification.Attachment, err error) []string {
		if err != nil {
			t.Fatalf("list attachments error = %v", err)
		}
		out := []string{}
		for _, a := range list {
			out = append(out, a.ID)
		}
		return out
	}
	byTarget := ids(store.Attachments(ctx, "ntf_x"))
	if diff := cmp.Diff([]string{"nta_1", "nta_2", "nta_4"}, byTarget); diff != "" {
		t.Errorf("Attachments() mismatch (-want +got):\n%s", diff)
	}
	byObject := ids(store.AttachedTo(ctx, "template", "tpl_1"))
	if diff := cmp.Diff([]string{"nta_1", "nta_2", "nta_3"}, byObject); diff != "" {
		t.Errorf("AttachedTo() mismatch (-want +got):\n%s", diff)
	}
	full, err := store.AttachedTo(ctx, "org", "org_1")
	if err != nil {
		t.Fatalf("AttachedTo() error = %v", err)
	}
	want := []*notification.Attachment{tests[3].In}
	if diff := cmp.Diff(want, full); diff != "" {
		t.Errorf("AttachedTo() fields mismatch (-want +got):\n%s", diff)
	}
	if err := store.Detach(ctx, "nta_2"); err != nil {
		t.Fatalf("Detach() error = %v", err)
	}
	if err := store.Detach(ctx, "nta_2"); !errors.Is(err, notification.ErrAttachmentNotFound) {
		t.Errorf("Detach() twice = %v, want ErrAttachmentNotFound", err)
	}
	after := ids(store.AttachedTo(ctx, "template", "tpl_1"))
	if diff := cmp.Diff([]string{"nta_1", "nta_3"}, after); diff != "" {
		t.Errorf("AttachedTo() after Detach mismatch (-want +got):\n%s", diff)
	}
	if got := ids(store.AttachedTo(ctx, "schedule", "sch_none")); len(got) != 0 {
		t.Errorf("AttachedTo() on an object with none = %v, want empty", got)
	}
}

// testDeleteCascades verifies deleting a target removes its attachments, so no object keeps a
// pointer to a target that is gone, and leaves other targets' attachments alone.
func testDeleteCascades(t *testing.T, store notification.Store) {
	ctx := context.Background()
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	for _, id := range []string{"ntf_del", "ntf_keep"} {
		if err := store.Save(ctx, target(id, base)); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
	}
	for i, ntf := range []string{"ntf_del", "ntf_del", "ntf_keep"} {
		a := attach(fmt.Sprintf("nta_d%d", i), ntf, "project", "proj_1",
			[]string{"started", "failure", "failure"}[i], base.Add(time.Duration(i)*time.Second))
		if err := store.Attach(ctx, a); err != nil {
			t.Fatalf("Attach() error = %v", err)
		}
	}
	if err := store.Delete(ctx, "ntf_del"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if err := store.Delete(ctx, "ntf_del"); !errors.Is(err, notification.ErrNotFound) {
		t.Errorf("Delete() twice = %v, want ErrNotFound", err)
	}
	left, err := store.AttachedTo(ctx, "project", "proj_1")
	if err != nil {
		t.Fatalf("AttachedTo() error = %v", err)
	}
	if len(left) != 1 || left[0].NotificationID != "ntf_keep" {
		t.Errorf("attachments after delete = %+v, want only ntf_keep's", left)
	}
}
