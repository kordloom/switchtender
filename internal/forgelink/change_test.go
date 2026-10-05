package forgelink

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

// TestUnlinkUserFailsClosedOneLinkAtATime proves an account's links end one at a time, each only
// after its record was written, so a record that fails keeps that link and every link after it.
func TestUnlinkUserFailsClosedOneLinkAtATime(t *testing.T) {
	t.Parallel()
	tests := []struct {
		FailAt      int
		WantRemoved int
		WantKept    []string
		WantErr     bool
	}{{ // Test 0: Every record lands, so every link ends.
		FailAt: -1, WantRemoved: 3,
	}, { // Test 1: The second record fails, so the first link ends and the rest stay.
		FailAt: 1, WantRemoved: 1, WantKept: []string{"fl_1", "fl_2"}, WantErr: true,
	}, { // Test 2: The first record fails, so every link stays.
		FailAt: 0, WantKept: []string{"fl_0", "fl_1", "fl_2"}, WantErr: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			store := NewMemStore()
			base := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
			for i, api := range []string{"https://api.github.com", "https://gitlab.com/api/v4",
				"https://github.example.com/api/v3"} {
				provider := "github"
				if i == 1 {
					provider = "gitlab"
				}
				if err := store.Create(ctx, &Link{ID: fmt.Sprintf("fl_%d", i), UserID: "usr_gone",
					Provider: provider, APIURL: api, ForgeUserID: int64(100 + i),
					CreatedAt: base.Add(time.Duration(i) * time.Second)}); err != nil {
					t.Fatalf("Create() error = %v", err)
				}
			}
			var recorded []string
			removed, err := UnlinkUser(ctx, store, "usr_gone", func(l *Link) error {
				if len(recorded) == test.FailAt {
					return errors.New("chain unavailable")
				}
				recorded = append(recorded, l.ID)
				return nil
			})
			if (err != nil) != test.WantErr {
				t.Fatalf("UnlinkUser() error = %v, want an error %v", err, test.WantErr)
			}
			if removed != test.WantRemoved {
				t.Errorf("removed = %d, want %d", removed, test.WantRemoved)
			}
			left, err := store.ForUser(ctx, "usr_gone")
			if err != nil {
				t.Fatalf("ForUser() error = %v", err)
			}
			var kept []string
			for _, l := range left {
				kept = append(kept, l.ID)
			}
			if diff := cmp.Diff(test.WantKept, kept, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("kept links mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
