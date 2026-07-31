package server

import (
	"context"
	"sync"
	"testing"

	"github.com/kordloom/switchtender/internal/user"
)

// TestConcurrentDeletesCannotEmptyTheAdmins checks that two requests racing to remove the last two
// administrators cannot both succeed.
//
// Counting the admins and then deleting is two statements, and the count another request took
// between them is the same count. Both saw a survivor and both went through, leaving an install with
// no administrator: every admin-gated route is then unreachable, including the one that creates a
// user, so the only way back in is a shell on the host. Several control nodes share one database, so
// a lock held in one process would not have closed it either. The store decides, in one statement.
func TestConcurrentDeletesCannotEmptyTheAdmins(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := user.NewMemStore()
	for _, id := range []string{"user_a", "user_b"} {
		u, err := user.New(id, "hunter2hunter2", user.RoleAdmin)
		if err != nil {
			t.Fatalf("New(%s) error = %v", id, err)
		}
		u.ID = id
		if err := store.Save(ctx, u); err != nil {
			t.Fatalf("Save(%s) error = %v", id, err)
		}
	}

	var wg sync.WaitGroup
	results := make([]bool, 2)
	start := make(chan struct{})
	for i, id := range []string{"user_a", "user_b"} {
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			<-start
			ok, err := store.DeleteUnlessLastAdmin(ctx, id)
			if err != nil {
				t.Errorf("DeleteUnlessLastAdmin(%s) error = %v", id, err)
			}
			results[i] = ok
		}(i, id)
	}
	close(start)
	wg.Wait()

	left, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	admins := 0
	for _, u := range left {
		if u.Role == user.RoleAdmin {
			admins++
		}
	}
	if admins == 0 {
		t.Errorf("both deletes reported %v and the install has no administrator left, so nobody "+
			"can reach an admin route and there is no way back in but a shell on the host", results)
	}
	if results[0] && results[1] {
		t.Error("both deletes succeeded")
	}
}

// TestDemotingTheLastAdminIsRefused checks the other route to zero administrators.
func TestDemotingTheLastAdminIsRefused(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := user.NewMemStore()
	u, err := user.New("solo", "hunter2hunter2", user.RoleAdmin)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	u.ID = "user_solo"
	if err := store.Save(ctx, u); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	demoted := *u
	demoted.Role = user.RoleViewer
	applied, err := store.UpdateUnlessLastAdmin(ctx, &demoted)
	if err != nil {
		t.Fatalf("UpdateUnlessLastAdmin() error = %v", err)
	}
	if applied {
		t.Error("the only administrator was demoted, locking everyone out of the admin routes")
	}
	got, err := store.Get(ctx, u.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Role != user.RoleAdmin {
		t.Errorf("role = %q, want it left admin", got.Role)
	}

	// An admin among several may still be demoted, and their other fields still update.
	second, err := user.New("second", "hunter2hunter2", user.RoleAdmin)
	if err != nil {
		t.Fatalf("New() second error = %v", err)
	}
	second.ID = "user_second"
	if err := store.Save(ctx, second); err != nil {
		t.Fatalf("Save() second error = %v", err)
	}
	if applied, err := store.UpdateUnlessLastAdmin(ctx, &demoted); err != nil || !applied {
		t.Errorf("demoting one of two admins = (%v, %v), want it applied", applied, err)
	}
}
