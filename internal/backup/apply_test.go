package backup

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/kordloom/switchtender/internal/user"
)

// failingUserStore wraps a user.Store and fails a chosen Save call, so a restore that dies partway
// through the users loop can be observed without a real database fault.
type failingUserStore struct {
	user.Store
	// failOn is the one-based index of the Save call that returns an error.
	failOn int
	// calls counts Save invocations so far.
	calls int
}

// Save records the call and fails the chosen one, otherwise delegating to the wrapped store.
func (f *failingUserStore) Save(ctx context.Context, u *user.User) error {
	f.calls++
	if f.calls == f.failOn {
		return errors.New("disk full")
	}
	return f.Store.Save(ctx, u)
}

// TestApplyReportsRowsWrittenOnPartialFailure proves a restore that fails partway through a store
// reports the true number of rows it committed, not zero. The count is raised as each object is
// saved, so an operator sees what a half-applied restore actually wrote.
func TestApplyReportsRowsWrittenOnPartialFailure(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name      string
		FailOn    int
		Users     int
		WantUsers int
	}{{ // Test 0: A failure on the first save writes nothing.
		Name: "fails on first", FailOn: 1, Users: 4, WantUsers: 0,
	}, { // Test 1: A failure on the third save leaves the first two committed.
		Name: "fails midway", FailOn: 3, Users: 4, WantUsers: 2,
	}, { // Test 2: A failure on the last save leaves every earlier row committed.
		Name: "fails on last", FailOn: 4, Users: 4, WantUsers: 3,
	}}
	for testNum, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			stores := freshStores()
			stores.Users = &failingUserStore{Store: user.NewMemStore(), failOn: test.FailOn}
			var p payload
			for i := 0; i < test.Users; i++ {
				p.Users = append(p.Users, userDTO{User: user.User{
					ID: fmt.Sprintf("usr_%d", i), Username: fmt.Sprintf("u%d", i), Role: user.RoleViewer,
				}})
			}
			sum, err := apply(t.Context(), stores, &p)
			if err == nil {
				t.Fatalf("test %d: apply of a store that fails on save %d returned no error",
					testNum, test.FailOn)
			}
			if sum.Users != test.WantUsers {
				t.Errorf("test %d: Summary.Users = %d, want %d rows actually written",
					testNum, sum.Users, test.WantUsers)
			}
		})
	}
}
