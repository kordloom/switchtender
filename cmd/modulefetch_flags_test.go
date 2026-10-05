package cmd

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

// TestModuleFetchFlagsAreValidated pins the bounds the module download flags accept. A value
// outside them is refused at startup and named, rather than leaving a gate that gives a download no
// time, or one that waits on a submission for hours.
func TestModuleFetchFlagsAreValidated(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Timeout is the --module-fetch-timeout value.
		Timeout time.Duration
		// MaxMiB is the --module-fetch-max-mib value.
		MaxMiB int
		// WantFlag names the flag refused, empty when both are accepted.
		WantFlag string
	}{{ // Test 0: The defaults.
		Timeout: 2 * time.Minute, MaxMiB: 512,
	}, { // Test 1: The shortest time and the smallest size accepted.
		Timeout: time.Second, MaxMiB: 1,
	}, { // Test 2: The longest time and the largest size accepted.
		Timeout: time.Hour, MaxMiB: 64 << 10,
	}, { // Test 3: No time at all.
		Timeout: 0, MaxMiB: 512, WantFlag: "--module-fetch-timeout",
	}, { // Test 4: Under a second.
		Timeout: 999 * time.Millisecond, MaxMiB: 512, WantFlag: "--module-fetch-timeout",
	}, { // Test 5: Past an hour.
		Timeout: time.Hour + time.Second, MaxMiB: 512, WantFlag: "--module-fetch-timeout",
	}, { // Test 6: No size at all.
		Timeout: time.Minute, MaxMiB: 0, WantFlag: "--module-fetch-max-mib",
	}, { // Test 7: Past the largest size.
		Timeout: time.Minute, MaxMiB: 64<<10 + 1, WantFlag: "--module-fetch-max-mib",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			err := checkModuleFetchLimits(test.Timeout, test.MaxMiB)
			if test.WantFlag == "" {
				if err != nil {
					t.Errorf("checkModuleFetchLimits() = %v, want accepted", err)
				}
				return
			}
			if !errors.Is(err, ErrUsage) || !strings.Contains(err.Error(), test.WantFlag) {
				t.Errorf("checkModuleFetchLimits() = %v, want ErrUsage naming %s", err, test.WantFlag)
			}
		})
	}
}

// TestModuleFetchFlagsCarryTheDefaults pins that serve and worker both take the flags, with the
// bounds the gate used before they existed, so an install that sets neither behaves as it did.
func TestModuleFetchFlagsCarryTheDefaults(t *testing.T) {
	t.Parallel()
	want := map[string]string{"module-fetch-timeout": "2m0s", "module-fetch-max-mib": "512"}
	for _, c := range []string{"serve", "worker"} {
		cmd := serveCmd
		if c == "worker" {
			cmd = workerCmd
		}
		got := map[string]string{}
		for name := range want {
			if f := cmd.Flags().Lookup(name); f != nil {
				got[name] = f.DefValue
			}
		}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("%s module download flags mismatch (-want +got):\n%s", c, diff)
		}
	}
}

// TestModuleKeepFlagsAreValidated pins the bounds the module keep flags accept. A value outside
// them is refused at startup and named, rather than keeping nothing a held plan can execute, or
// filling the disk with trees no run will ask for again.
func TestModuleKeepFlagsAreValidated(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// KeepFor is the --module-keep-for value.
		KeepFor time.Duration
		// MaxMiB is the --module-keep-max-mib value.
		MaxMiB int
		// WantFlag names the flag refused, empty when both are accepted.
		WantFlag string
	}{{ // Test 0: The defaults.
		KeepFor: 7 * 24 * time.Hour, MaxMiB: 2048,
	}, { // Test 1: The shortest time and the smallest size accepted.
		KeepFor: time.Hour, MaxMiB: 1,
	}, { // Test 2: The longest time and the largest size accepted.
		KeepFor: 90 * 24 * time.Hour, MaxMiB: 1 << 20,
	}, { // Test 3: No time at all.
		KeepFor: 0, MaxMiB: 2048, WantFlag: "--module-keep-for",
	}, { // Test 4: Under an hour.
		KeepFor: time.Hour - time.Second, MaxMiB: 2048, WantFlag: "--module-keep-for",
	}, { // Test 5: Past ninety days.
		KeepFor: 90*24*time.Hour + time.Second, MaxMiB: 2048, WantFlag: "--module-keep-for",
	}, { // Test 6: No space at all.
		KeepFor: time.Hour, MaxMiB: 0, WantFlag: "--module-keep-max-mib",
	}, { // Test 7: Past the most space.
		KeepFor: time.Hour, MaxMiB: 1<<20 + 1, WantFlag: "--module-keep-max-mib",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			err := checkModuleKeep(test.KeepFor, test.MaxMiB)
			if test.WantFlag == "" {
				if err != nil {
					t.Errorf("checkModuleKeep() = %v, want accepted", err)
				}
				return
			}
			if !errors.Is(err, ErrUsage) || !strings.Contains(err.Error(), test.WantFlag) {
				t.Errorf("checkModuleKeep() = %v, want ErrUsage naming %s", err, test.WantFlag)
			}
		})
	}
}

// TestModuleKeepFlagsCarryTheDefaults pins that serve and worker both take the keep flags, with the
// retention the gate kept before they existed, so an install that sets neither behaves as it did.
func TestModuleKeepFlagsCarryTheDefaults(t *testing.T) {
	t.Parallel()
	want := map[string]string{"module-keep-for": "168h0m0s", "module-keep-max-mib": "2048"}
	for _, c := range []string{"serve", "worker"} {
		cmd := serveCmd
		if c == "worker" {
			cmd = workerCmd
		}
		got := map[string]string{}
		for name := range want {
			if f := cmd.Flags().Lookup(name); f != nil {
				got[name] = f.DefValue
			}
		}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("%s module keep flags mismatch (-want +got):\n%s", c, diff)
		}
	}
}
