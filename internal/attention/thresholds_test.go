package attention

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

// lease is the lease period the tests resolve the worker-lost default against.
const lease = 30 * time.Second

// TestParseConfig pins what a thresholds file may say: durations and off, at the default, an
// organization, a queue, or a template, and nothing it cannot act on.
func TestParseConfig(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Doc        string
		Org        string
		Queue      string
		Template   string
		WantLimits Limits
		Want       error
	}{{ // Test 0: No file says nothing, so the built-in thresholds apply.
		Doc: "",
		WantLimits: Limits{BlockedAfter: 15 * time.Minute, AlertNoWorker: 15 * time.Minute,
			AlertBlocked: 15 * time.Minute, AlertWorkerLost: 2 * lease},
	}, { // Test 1: The organization's defaults replace the built-in ones they state.
		Doc: "defaults:\n  alert_no_worker: 5m\n  alert_approval: 4h\n",
		WantLimits: Limits{BlockedAfter: 15 * time.Minute, AlertNoWorker: 5 * time.Minute,
			AlertBlocked: 15 * time.Minute, AlertWorkerLost: 2 * lease, AlertApproval: 4 * time.Hour},
	}, { // Test 2: Off turns an alert off, and never says the same.
		Doc:        "defaults:\n  alert_no_worker: off\n  alert_blocked: never\n",
		WantLimits: Limits{BlockedAfter: 15 * time.Minute, AlertWorkerLost: 2 * lease},
	}, { // Test 3: A template override wins over a queue override, which wins over the defaults.
		Doc: "defaults:\n  alert_no_worker: 20m\nqueues:\n  prod:\n    alert_no_worker: 2m\n" +
			"templates:\n  tpl_a:\n    alert_no_worker: 1m\n",
		Queue: "prod", Template: "tpl_a",
		WantLimits: Limits{BlockedAfter: 15 * time.Minute, AlertNoWorker: time.Minute,
			AlertBlocked: 15 * time.Minute, AlertWorkerLost: 2 * lease},
	}, { // Test 4: A queue override applies to that queue only.
		Doc:   "queues:\n  prod:\n    alert_no_worker: 2m\n",
		Queue: "dmz",
		WantLimits: Limits{BlockedAfter: 15 * time.Minute, AlertNoWorker: 15 * time.Minute,
			AlertBlocked: 15 * time.Minute, AlertWorkerLost: 2 * lease},
	}, { // Test 5: An organization override applies between the defaults and a queue.
		Doc: "orgs:\n  org_a:\n    alert_approval: 1h\n    alert_blocked: 30m\n" +
			"queues:\n  \"\":\n    alert_blocked: 45m\n",
		Org: "org_a", Queue: "",
		WantLimits: Limits{BlockedAfter: 15 * time.Minute, AlertNoWorker: 15 * time.Minute,
			AlertBlocked: 45 * time.Minute, AlertWorkerLost: 2 * lease, AlertApproval: time.Hour},
	}, { // Test 6: A misspelled threshold is refused rather than ignored.
		Doc:  "defaults:\n  alert_no_wroker: 5m\n",
		Want: ErrConfig,
	}, { // Test 7: Zero is refused rather than read as off.
		Doc:  "defaults:\n  alert_blocked: 0\n",
		Want: ErrConfig,
	}, { // Test 8: Text that is not a duration is refused.
		Doc:  "defaults:\n  alert_blocked: soon\n",
		Want: ErrConfig,
	}, { // Test 9: blocked_after cannot be off, since it decides when a blocked run shows at all.
		Doc:  "queues:\n  prod:\n    blocked_after: off\n",
		Want: ErrConfig,
	}, { // Test 10: A negative duration is refused.
		Doc:  "defaults:\n  alert_no_worker: -5m\n",
		Want: ErrConfig,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			cfg, err := ParseConfig([]byte(test.Doc))
			if !errors.Is(err, test.Want) {
				t.Fatalf("ParseConfig() error = %v, want %v", err, test.Want)
			}
			if test.Want != nil {
				return
			}
			got := cfg.Limits(test.Org, test.Queue, test.Template, lease)
			if diff := cmp.Diff(test.WantLimits, got); diff != "" {
				t.Errorf("Limits() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestLoadConfig pins that a file is read, and that one that is missing is an error rather than the
// built-in thresholds, so a mistyped path never silently alerts on defaults nobody chose.
func TestLoadConfig(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	good := filepath.Join(dir, "attention.yml")
	if err := os.WriteFile(good, []byte("defaults:\n  alert_approval: 2h\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	tests := []struct {
		Path         string
		WantApproval time.Duration
		Want         error
	}{{ // Test 0: A readable file loads.
		Path: good, WantApproval: 2 * time.Hour,
	}, { // Test 1: A missing file is an error.
		Path: filepath.Join(dir, "missing.yml"), Want: ErrConfig,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			cfg, err := LoadConfig(test.Path)
			if !errors.Is(err, test.Want) {
				t.Fatalf("LoadConfig() error = %v, want %v", err, test.Want)
			}
			if test.Want != nil {
				return
			}
			got := cfg.Limits("", "", "", lease).AlertApproval
			if diff := cmp.Diff(test.WantApproval, got); diff != "" {
				t.Errorf("approval alert mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
