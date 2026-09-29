package dispatch

import (
	"fmt"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/invsource"
)

// TestSourceDue covers the scheduled-sync staleness decision: no interval never syncs, a never-synced
// source is due, and a source is due once its interval has elapsed.
func TestSourceDue(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) *time.Time { u := now.Add(-d); return &u }
	tests := []struct {
		Name     string
		Interval int
		SyncedAt *time.Time
		WantDue  bool
	}{
		{"no interval", 0, ago(time.Hour), false},
		{"never synced", 300, nil, true},
		{"fresh", 300, ago(time.Minute), false},
		{"stale", 300, ago(10 * time.Minute), true},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			src := &invsource.Source{SyncIntervalSeconds: test.Interval, SyncedAt: test.SyncedAt}
			if got := sourceDue(src, now); got != test.WantDue {
				t.Errorf("sourceDue() = %v, want %v", got, test.WantDue)
			}
		})
	}
}

// TestLaunchStale covers the update-on-launch decision: a zero interval or never-synced source always
// refreshes, a fresh source within its interval does not, and a stale one does.
func TestLaunchStale(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) *time.Time { u := now.Add(-d); return &u }
	tests := []struct {
		Name      string
		Interval  int
		SyncedAt  *time.Time
		WantStale bool
	}{
		{"no interval always refreshes", 0, ago(time.Minute), true},
		{"never synced", 300, nil, true},
		{"fresh", 300, ago(time.Minute), false},
		{"stale", 300, ago(10 * time.Minute), true},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			src := &invsource.Source{SyncIntervalSeconds: test.Interval, SyncedAt: test.SyncedAt}
			if got := launchStale(src, now); got != test.WantStale {
				t.Errorf("launchStale() = %v, want %v", got, test.WantStale)
			}
		})
	}
}
