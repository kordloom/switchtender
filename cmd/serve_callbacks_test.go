package cmd

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// TestCallbackFlagsRefuseALimitBelowOne pins that both callback limits are positive. A limit of
// zero would refuse every callback rather than bound them, so it is refused when the server
// starts, naming the flag, and the defaults pass.
func TestCallbackFlagsRefuseALimitBelowOne(t *testing.T) {
	tests := []struct {
		WantFlag  string
		Rate      int
		KeyErrors int
	}{{ // Test 0: The defaults.
		Rate: 30, KeyErrors: 10,
	}, { // Test 1: No callbacks at all is not a limit.
		Rate: 0, KeyErrors: 10, WantFlag: "--callback-rate-limit",
	}, { // Test 2: No wrong key at all is not a limit either.
		Rate: 30, KeyErrors: -1, WantFlag: "--callback-key-failure-limit",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			// Not parallel: the check reads package-level flag variables.
			rate, failures := serveCallbackRate, serveCallbackKeyFailures
			t.Cleanup(func() { serveCallbackRate, serveCallbackKeyFailures = rate, failures })
			serveCallbackRate, serveCallbackKeyFailures = test.Rate, test.KeyErrors
			err := checkCallbackFlags()
			if test.WantFlag == "" {
				if err != nil {
					t.Errorf("checkCallbackFlags() = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, ErrUsage) || !strings.Contains(err.Error(), test.WantFlag) {
				t.Errorf("checkCallbackFlags() = %v, want ErrUsage naming %s", err, test.WantFlag)
			}
		})
	}
}

// TestCallbackFlagDefaultsAreTheServers pins that the flags default to the limits the server
// applies, so a server started without them and one started with the documented defaults agree.
func TestCallbackFlagDefaultsAreTheServers(t *testing.T) {
	t.Parallel()
	for flag, want := range map[string]string{
		"callback-rate-limit": "30", "callback-key-failure-limit": "10",
		"fact-cache-admin-only": "false",
	} {
		f := serveCmd.Flags().Lookup(flag)
		if f == nil || f.DefValue != want {
			t.Errorf("--%s default = %v, want %s", flag, f, want)
		}
	}
}
