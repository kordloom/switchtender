package run_test

import (
	"fmt"
	"testing"

	"github.com/kordloom/switchtender/internal/run"
)

// TestANotificationListIsBounded covers a request that turns one small submission into a flood.
//
// Each notification target a run carries becomes its own delivery when the run ends, and each delivery
// its own goroutine and its own outbound connection. Nothing bounded how many targets a run could carry,
// so a single request of a few hundred kilobytes could name tens of thousands of them, and every run it
// produced fanned out into tens of thousands of goroutines and sockets at once. A schedule firing such a
// run does it on a timer without anybody asking again.
//
// The list has a limit, checked where it is written, so the shape is refused when it is stored rather
// than surviving as a run that will do this every time it finishes.
func TestANotificationListIsBounded(t *testing.T) {
	t.Parallel()

	target := run.NotifyTarget{Kind: run.NotifyWebhook, URL: "https://example.com/hook"}
	tests := []struct {
		// Name says how many targets the request carried.
		Name string
		// Count is how many.
		Count int
		// WantErr reports whether the list must be refused.
		WantErr bool
	}{
		{"none", 0, false},
		{"a handful", 5, false},
		{"exactly the limit", run.MaxNotifyTargets, false},
		{"one past the limit", run.MaxNotifyTargets + 1, true},
		{"a flood", 20_000, true},
	}

	for testNum, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			targets := make([]run.NotifyTarget, 0, test.Count)
			for i := 0; i < test.Count; i++ {
				targets = append(targets, target)
			}
			err := run.ValidateNotifyTargets(targets)
			if test.WantErr && err == nil {
				t.Errorf("test %d: %d notification targets were accepted, so every run this produces "+
					"fans out into %d goroutines and sockets at once", testNum, test.Count, test.Count)
			}
			if !test.WantErr && err != nil {
				t.Errorf("test %d: %d notification targets were refused: %v", testNum, test.Count, err)
			}
		})
	}

	// A target that names no destination is still refused, since the list check must not replace the
	// per-target check.
	t.Run("a target with no destination", func(t *testing.T) {
		t.Parallel()
		err := run.ValidateNotifyTargets([]run.NotifyTarget{{Kind: run.NotifyWebhook}})
		if err == nil {
			t.Error("a webhook target with no url was accepted, so it would reach nobody")
		}
	})

	// And the refusal says what the limit is, so somebody importing a long list can see why.
	t.Run("the refusal names the limit", func(t *testing.T) {
		t.Parallel()
		targets := make([]run.NotifyTarget, run.MaxNotifyTargets+1)
		for i := range targets {
			targets[i] = target
		}
		err := run.ValidateNotifyTargets(targets)
		if err == nil {
			t.Fatal("an over-long list was accepted")
		}
		if want := fmt.Sprintf("%d", run.MaxNotifyTargets); !contains(err.Error(), want) {
			t.Errorf("the refusal %q does not name the limit %s", err, want)
		}
	})
}

// contains reports whether s holds sub.
func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
