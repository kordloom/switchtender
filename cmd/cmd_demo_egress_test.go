package cmd

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/safedial"
)

// TestDemoTakesTheEgressProxyServeTakes pins that the demo reads SWITCHTENDER_EGRESS_PROXY the way
// serve and worker do. The demo anchors its seeded chain through the guarded timestamp client,
// which never reads the ambient HTTPS_PROXY, so without this setting a demo run behind a corporate
// proxy has no route to the timestamp authority and seeds unanchored. A malformed value is refused
// before anything is opened, as it is for serve.
//
// Each case stops at the span cadence check, after the proxy step and before any seeding.
func TestDemoTakesTheEgressProxyServeTakes(t *testing.T) {
	tests := []struct {
		// Proxy is the SWITCHTENDER_EGRESS_PROXY value the demo starts with.
		Proxy string
		// WantProxy is the egress proxy safedial holds once the demo has started, empty for none.
		WantProxy string
		// Want is the error the demo must return, nil when the run should reach the cadence check.
		Want error
		// WantWord is a word the returned error must contain.
		WantWord string
	}{{ // Test 0: A usable proxy is installed for the guarded clients.
		Proxy: "http://proxy.example.test:3128", WantProxy: "http://proxy.example.test:3128",
		WantWord: "span-cadence",
	}, { // Test 1: A proxy with an unusable scheme is refused as a usage error.
		Proxy: "ftp://proxy.example.test", Want: ErrUsage, WantWord: "egress proxy",
	}, { // Test 2: No proxy leaves the guarded clients dialing directly.
		WantWord: "span-cadence",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			// Not parallel: the demo command reads package-level flag variables and the
			// environment, and the egress proxy is process-wide safedial state.
			if err := safedial.SetEgressProxy(""); err != nil {
				t.Fatalf("SetEgressProxy() error = %v", err)
			}
			t.Cleanup(func() { _ = safedial.SetEgressProxy("") })
			t.Setenv(egressProxyEnv, test.Proxy)
			setString(t, &demoDB, tempDB(t))
			setBool(t, &demoNoSeed, true)
			setBool(t, &demoSeedOnly, false)
			setDuration(t, &demoSpanCadence, 500*time.Millisecond)
			setString(t, &demoAnchorTSA, "")

			err := runDemo(testCommand(), nil)
			if err == nil {
				t.Fatal("runDemo() = nil error, want a refusal")
			}
			if test.Want != nil && !errors.Is(err, test.Want) {
				t.Errorf("runDemo() error = %v, want %v", err, test.Want)
			}
			if test.Want == nil && errors.Is(err, ErrUsage) {
				t.Errorf("runDemo() error = %v, want the run to pass the proxy step", err)
			}
			if !strings.Contains(err.Error(), test.WantWord) {
				t.Errorf("runDemo() error = %v, want it to name %q", err, test.WantWord)
			}
			got := ""
			if u := safedial.EgressProxy(); u != nil {
				got = u.String()
			}
			if diff := cmp.Diff(test.WantProxy, got); diff != "" {
				t.Errorf("egress proxy mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
