package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/forgelink"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/user"
)

// TestDeletingAUserRemovesTheirForgeLinks proves an account's forge links go when the account does,
// so the forge account can be linked again and no comment acts as an account that is gone, while
// another account's links stay.
func TestDeletingAUserRemovesTheirForgeLinks(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Delete        int
		WantRemaining []int64
	}{{ // Test 0: Deleting the first account removes its link and keeps the other's.
		Delete: 0, WantRemaining: []int64{2002},
	}, { // Test 1: Deleting the second account removes its link and keeps the first's.
		Delete: 1, WantRemaining: []int64{2001},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			users := user.NewMemStore()
			links := forgelink.NewMemStore()
			var ids []string
			for i, name := range []string{"admin-one", "admin-two"} {
				u, err := user.New(name, "a-long-enough-password", user.RoleAdmin)
				if err != nil {
					t.Fatalf("user.New() error = %v", err)
				}
				if err := users.Save(ctx, u); err != nil {
					t.Fatalf("Save() error = %v", err)
				}
				if err := links.Create(ctx, &forgelink.Link{ID: fmt.Sprintf("fl_del%d", i),
					UserID: u.ID, Provider: "github", APIURL: "https://api.github.com",
					ForgeUserID: int64(2001 + i), CreatedAt: time.Now()}); err != nil {
					t.Fatalf("Create() error = %v", err)
				}
				ids = append(ids, u.ID)
			}
			srv := New(run.NewMemStore(), &fakeSubmitter{}, zap.NewNop(), WithUsers(users),
				WithForgeLinks(links))
			req := httptest.NewRequest(http.MethodDelete, "/v1/users/"+ids[test.Delete], nil)
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("delete status = %d, body %s", rec.Code, rec.Body.String())
			}
			var remaining []int64
			for _, id := range ids {
				list, err := links.ForUser(ctx, id)
				if err != nil {
					t.Fatalf("ForUser() error = %v", err)
				}
				for _, l := range list {
					remaining = append(remaining, l.ForgeUserID)
				}
			}
			if diff := cmp.Diff(test.WantRemaining, remaining, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("links mismatch (-want +got):\n%s", diff)
			}
			if _, err := links.Lookup(ctx, "github", "https://api.github.com",
				int64(2001+test.Delete)); err == nil {
				t.Errorf("the deleted account's forge account still resolves to a link")
			}
		})
	}
}

// TestDeletingAUserFailsClosedOnTheChain proves an account whose forge links cannot be recorded as
// unlinked is not deleted, and keeps its links, so the chain never misses a link's end and never
// shows one that did not happen. An account with no links is deleted as before.
func TestDeletingAUserFailsClosedOnTheChain(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Linked     bool
		WantStatus int
		WantKept   bool
	}{{ // Test 0: A linked account whose unlink cannot be recorded is kept with its link.
		Linked: true, WantStatus: http.StatusServiceUnavailable, WantKept: true,
	}, { // Test 1: An account with no links needs no unlink record and is deleted.
		Linked: false, WantStatus: http.StatusOK, WantKept: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			users := user.NewMemStore()
			links := forgelink.NewMemStore()
			var ids []string
			for _, name := range []string{"admin-keep", "admin-gone"} {
				u, err := user.New(name, "a-long-enough-password", user.RoleAdmin)
				if err != nil {
					t.Fatalf("user.New() error = %v", err)
				}
				if err := users.Save(ctx, u); err != nil {
					t.Fatalf("Save() error = %v", err)
				}
				ids = append(ids, u.ID)
			}
			if test.Linked {
				if err := links.Create(ctx, &forgelink.Link{ID: "fl_failclosed", UserID: ids[1],
					Provider: "github", APIURL: "https://api.github.com", ForgeUserID: 5001,
					CreatedAt: time.Now()}); err != nil {
					t.Fatalf("Create() error = %v", err)
				}
			}
			h := deleteUserHandler(users, links, &failingAudits{err: errors.New("disk full")},
				zap.NewNop())
			req := httptest.NewRequest(http.MethodDelete, "/v1/users/"+ids[1], nil)
			req.SetPathValue("id", ids[1])
			rec := httptest.NewRecorder()
			h(rec, req)
			if rec.Code != test.WantStatus {
				t.Fatalf("delete status = %d, want %d, body %s", rec.Code, test.WantStatus,
					rec.Body.String())
			}
			_, err := users.Get(ctx, ids[1])
			if kept := err == nil; kept != test.WantKept {
				t.Errorf("account kept = %v, want %v", kept, test.WantKept)
			}
			_, err = links.Lookup(ctx, "github", "https://api.github.com", 5001)
			if kept := err == nil; kept != (test.Linked && test.WantKept) {
				t.Errorf("link kept = %v, want %v", kept, test.Linked && test.WantKept)
			}
		})
	}
}
