package pgstore_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/schedule"
	"github.com/kordloom/switchtender/internal/user"
)

// clearUsers empties the accounts table so a last-administrator test reasons about a population it
// created itself.
func clearUsers(t *testing.T, dsn string) {
	t.Helper()
	raw, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	defer func() { _ = raw.Close() }()
	if _, err := raw.Exec("TRUNCATE users"); err != nil {
		t.Fatalf("truncate users: %v", err)
	}
}

// newUser builds an account with the given id and role.
func newUser(id string, role user.Role) *user.User {
	return &user.User{
		ID: id, Username: id, PasswordHash: "hash", Role: role, CreatedAt: time.Now().UTC(),
	}
}

// TestDeleteUnlessLastAdminNeverEmptiesTheAdminSet pins the guard that stops an install locking
// itself out. Removing the final administrator leaves nobody who can create another, grant a role,
// or approve anything: the control plane keeps running while nobody can administer it, and there is
// no recovery path inside the product. The refusal has to fail closed.
//
// Not parallel: the guard counts every administrator in the table, so it reasons about state that is
// global to the database rather than scoped to one row.
//
//nolint:funlen // Test function.
func TestDeleteUnlessLastAdminNeverEmptiesTheAdminSet(t *testing.T) {
	dsn := testDSN(t)
	db := openShared(t)
	ctx := context.Background()
	users := db.Users()

	tests := []struct {
		Name        string
		Population  []*user.User
		Target      string
		WantChanged bool
		Want        error
	}{{ // Test 0: The only administrator cannot be deleted.
		Name:       "sole admin",
		Population: []*user.User{newUser("u_admin", user.RoleAdmin)},
		Target:     "u_admin", WantChanged: false, Want: nil,
	}, { // Test 1: Nor when every other account is a non-administrator.
		Name: "sole admin among operators",
		Population: []*user.User{
			newUser("u_admin", user.RoleAdmin),
			newUser("u_op1", user.RoleOperator),
			newUser("u_op2", user.RoleOperator),
		},
		Target: "u_admin", WantChanged: false, Want: nil,
	}, { // Test 2: With a second administrator present, the first may go.
		Name: "one of two admins",
		Population: []*user.User{
			newUser("u_admin", user.RoleAdmin),
			newUser("u_admin2", user.RoleAdmin),
		},
		Target: "u_admin", WantChanged: true, Want: nil,
	}, { // Test 3: A non-administrator is never guarded, even as the only account left.
		Name: "sole operator",
		Population: []*user.User{
			newUser("u_admin", user.RoleAdmin),
			newUser("u_op1", user.RoleOperator),
		},
		Target: "u_op1", WantChanged: true, Want: nil,
	}, { // Test 4: An account that is not there is reported as absent, not as a refusal. The two
		// are different answers: one means "protected", the other means "no such account".
		Name:       "missing account",
		Population: []*user.User{newUser("u_admin", user.RoleAdmin)},
		Target:     "u_ghost", WantChanged: false, Want: user.ErrNotFound,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			clearUsers(t, dsn)
			for _, u := range test.Population {
				if err := users.Save(ctx, u); err != nil {
					t.Fatalf("Save(%s) error = %v", u.ID, err)
				}
			}
			changed, err := users.DeleteUnlessLastAdmin(ctx, test.Target)
			if !errors.Is(err, test.Want) {
				t.Fatalf("%s: DeleteUnlessLastAdmin() error = %v, want %v", test.Name, err, test.Want)
			}
			if changed != test.WantChanged {
				t.Fatalf("%s: changed = %v, want %v. Deleting the last administrator leaves an "+
					"install nobody can administer, with no recovery path inside the product.",
					test.Name, changed, test.WantChanged)
			}
			// Whatever happened, at least one administrator must remain.
			assertAdminRemains(t, ctx, users)
		})
	}
}

// TestUpdateUnlessLastAdminNeverDemotesTheLastOne pins the other route to zero administrators. The
// guard's own comment names it: demoting is the second way to reach an install with no admin, and a
// delete-only guard would be trivially bypassed by editing the role instead.
func TestUpdateUnlessLastAdminNeverDemotesTheLastOne(t *testing.T) {
	dsn := testDSN(t)
	db := openShared(t)
	ctx := context.Background()
	users := db.Users()

	tests := []struct {
		Name        string
		Population  []*user.User
		Target      string
		NewRole     user.Role
		WantChanged bool
		Want        error
	}{{ // Test 0: The only administrator cannot be demoted to operator.
		Name:       "demote sole admin",
		Population: []*user.User{newUser("u_admin", user.RoleAdmin)},
		Target:     "u_admin", NewRole: user.RoleOperator, WantChanged: false, Want: nil,
	}, { // Test 1: Nor to viewer, which is the same demotion by another name.
		Name:       "demote sole admin to viewer",
		Population: []*user.User{newUser("u_admin", user.RoleAdmin)},
		Target:     "u_admin", NewRole: user.RoleViewer, WantChanged: false, Want: nil,
	}, { // Test 2: The sole administrator may still be edited while staying an administrator, or
		// the guard would lock the last admin out of changing their own password.
		Name:       "edit sole admin as admin",
		Population: []*user.User{newUser("u_admin", user.RoleAdmin)},
		Target:     "u_admin", NewRole: user.RoleAdmin, WantChanged: true, Want: nil,
	}, { // Test 3: With a second administrator, a demotion is allowed.
		Name: "demote one of two admins",
		Population: []*user.User{
			newUser("u_admin", user.RoleAdmin), newUser("u_admin2", user.RoleAdmin),
		},
		Target: "u_admin", NewRole: user.RoleOperator, WantChanged: true, Want: nil,
	}, { // Test 4: Promoting a non-administrator is never guarded.
		Name: "promote operator",
		Population: []*user.User{
			newUser("u_admin", user.RoleAdmin), newUser("u_op1", user.RoleOperator),
		},
		Target: "u_op1", NewRole: user.RoleAdmin, WantChanged: true, Want: nil,
	}, { // Test 5: An account that is not there is reported as absent.
		Name:       "missing account",
		Population: []*user.User{newUser("u_admin", user.RoleAdmin)},
		Target:     "u_ghost", NewRole: user.RoleOperator, WantChanged: false, Want: user.ErrNotFound,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			clearUsers(t, dsn)
			for _, u := range test.Population {
				if err := users.Save(ctx, u); err != nil {
					t.Fatalf("Save(%s) error = %v", u.ID, err)
				}
			}
			target := newUser(test.Target, test.NewRole)
			changed, err := users.UpdateUnlessLastAdmin(ctx, target)
			if !errors.Is(err, test.Want) {
				t.Fatalf("%s: UpdateUnlessLastAdmin() error = %v, want %v", test.Name, err, test.Want)
			}
			if changed != test.WantChanged {
				t.Fatalf("%s: changed = %v, want %v. A demotion is the second route to an install "+
					"with no administrator, so a delete-only guard is bypassed by editing the role.",
					test.Name, changed, test.WantChanged)
			}
			assertAdminRemains(t, ctx, users)
		})
	}
}

// TestConcurrentLastAdminRemovalsCannotBothWin pins the advisory lock behind the guard. Two removals
// of the last two administrators touch different rows, so without the lock neither sees the other:
// both read a population containing two admins, both are allowed, and the install ends with none.
// The comment on guardedAdminChange states exactly this. Run with -race.
func TestConcurrentLastAdminRemovalsCannotBothWin(t *testing.T) {
	dsn := testDSN(t)
	db := openShared(t)
	ctx := context.Background()
	users := db.Users()

	// Repeated, because the race the lock closes is narrow and one attempt proves little.
	for attempt := range 12 {
		clearUsers(t, dsn)
		for _, id := range []string{"u_admin_a", "u_admin_b"} {
			if err := users.Save(ctx, newUser(id, user.RoleAdmin)); err != nil {
				t.Fatalf("Save(%s) error = %v", id, err)
			}
		}
		var wg sync.WaitGroup
		results := make([]bool, 2)
		for i, id := range []string{"u_admin_a", "u_admin_b"} {
			wg.Add(1)
			go func(i int, id string) {
				defer wg.Done()
				changed, err := users.DeleteUnlessLastAdmin(ctx, id)
				if err != nil {
					t.Errorf("DeleteUnlessLastAdmin(%s) error = %v", id, err)
					return
				}
				results[i] = changed
			}(i, id)
		}
		wg.Wait()
		if results[0] && results[1] {
			t.Fatalf("attempt %d: both administrators were deleted concurrently, leaving an "+
				"install nobody can administer. The two deletes touch different rows, so only the "+
				"advisory lock makes them see each other.", attempt)
		}
		assertAdminRemains(t, ctx, users)
	}
}

// assertAdminRemains fails the test when the accounts table holds no administrator. Every guarded
// operation must leave one, whichever branch it took.
func assertAdminRemains(t *testing.T, ctx context.Context, users user.Store) {
	t.Helper()
	all, err := users.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(all) == 0 {
		return
	}
	for _, u := range all {
		if u.Role == user.RoleAdmin {
			return
		}
	}
	t.Fatalf("the accounts table holds %d users and not one administrator, so nobody can create "+
		"an account, grant a role, or approve anything, and there is no recovery inside the product",
		len(all))
}

// TestClaimDueIsAnExactlyOnceCompareAndSwap pins the primitive the whole schedule-time migration
// exists to protect. Every node ticking the scheduler calls this for the same schedule at the same
// minute, and the loser reads its failed claim as another node having won. If two callers could both
// win, one cron entry would launch two runs against the fleet; if neither could, the schedule stops
// firing silently. The swap is on next_run_at as text, which is why the stored bytes are load
// bearing.
func TestClaimDueIsAnExactlyOnceCompareAndSwap(t *testing.T) {
	t.Parallel()
	db := openShared(t)
	ctx := context.Background()
	schedules := db.Schedules()
	stamp := time.Now().UnixNano()

	oldNext := time.Date(2027, 6, 1, 12, 0, 0, 0, time.UTC)
	newNext := oldNext.Add(time.Hour)

	t.Run("test 0", func(t *testing.T) { // Test 0: The holder of the expected time wins once.
		t.Parallel()
		id := fmt.Sprintf("sch_claim_%d_0", stamp)
		saveSchedule(t, ctx, schedules, id, oldNext)
		won, err := schedules.ClaimDue(ctx, id, oldNext, newNext)
		if err != nil {
			t.Fatalf("ClaimDue() error = %v", err)
		}
		if !won {
			t.Fatal("the first claim lost against the exact time it was told to expect, so the " +
				"schedule never fires: the scheduler reads a failed claim as another node winning")
		}
		// The same claim again must lose, since the stored time has moved on.
		again, err := schedules.ClaimDue(ctx, id, oldNext, newNext)
		if err != nil {
			t.Fatalf("ClaimDue() error = %v", err)
		}
		if again {
			t.Fatal("the same claim won twice, so one cron entry launches two runs at the fleet")
		}
	})
	t.Run("test 1", func(t *testing.T) { // Test 1: A claim against the wrong expected time loses.
		t.Parallel()
		id := fmt.Sprintf("sch_claim_%d_1", stamp)
		saveSchedule(t, ctx, schedules, id, oldNext)
		won, err := schedules.ClaimDue(ctx, id, oldNext.Add(time.Minute), newNext)
		if err != nil {
			t.Fatalf("ClaimDue() error = %v", err)
		}
		if won {
			t.Error("a claim against a time the row does not hold still won, so the compare half " +
				"of the compare-and-swap is not doing anything")
		}
	})
	t.Run("test 2", func(t *testing.T) { // Test 2: A schedule that is not there cannot be claimed.
		t.Parallel()
		won, err := schedules.ClaimDue(ctx, "sch_no_such_claim", oldNext, newNext)
		if err != nil {
			t.Fatalf("ClaimDue() error = %v", err)
		}
		if won {
			t.Error("a claim won against a schedule that does not exist")
		}
	})
	t.Run("test 3", func(t *testing.T) { // Test 3: Exactly one of many racing nodes wins.
		t.Parallel()
		id := fmt.Sprintf("sch_claim_%d_3", stamp)
		saveSchedule(t, ctx, schedules, id, oldNext)
		const nodes = 10
		var mu sync.Mutex
		wins := 0
		var wg sync.WaitGroup
		for range nodes {
			wg.Add(1)
			go func() {
				defer wg.Done()
				won, err := schedules.ClaimDue(ctx, id, oldNext, newNext)
				if err != nil {
					t.Errorf("ClaimDue() error = %v", err)
					return
				}
				if won {
					mu.Lock()
					wins++
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
		if wins != 1 {
			t.Errorf("%d of %d nodes won the same claim, want exactly 1: more than one means the "+
				"cron entry fires that many times against the fleet, none means it never fires",
				wins, nodes)
		}
	})
	t.Run("test 4", func(t *testing.T) { // Test 4: A time whose text form is unusual still round
		// trips, since the swap compares the stored bytes rather than an instant. This is the
		// property the normalize migration exists to preserve.
		t.Parallel()
		id := fmt.Sprintf("sch_claim_%d_4", stamp)
		odd := time.Date(2027, 6, 1, 12, 0, 0, 123456000, time.UTC)
		saveSchedule(t, ctx, schedules, id, odd)
		won, err := schedules.ClaimDue(ctx, id, odd, odd.Add(time.Hour))
		if err != nil {
			t.Fatalf("ClaimDue() error = %v", err)
		}
		if !won {
			t.Error("a schedule stored with a fractional second could not be claimed against the " +
				"same instant, which is exactly the failure that silently stopped every schedule " +
				"firing once before")
		}
	})
}

// saveSchedule stores a schedule with the given next fire time.
func saveSchedule(t *testing.T, ctx context.Context, s schedule.Store, id string, next time.Time) {
	t.Helper()
	at := next
	if err := s.Save(ctx, &schedule.Schedule{
		ID: id, Cron: "0 * * * *", Playbook: "p.yml", CreatedAt: time.Now().UTC(),
		NextRunAt: &at, Enabled: true,
	}); err != nil {
		t.Fatalf("Save(%s) error = %v", id, err)
	}
}
