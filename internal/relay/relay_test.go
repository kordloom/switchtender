package relay

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/kordloom/switchtender/internal/license"
)

// TestTheCommunityEngineRefusesNamingTeam pins the Community relay: no relay code is in the build,
// so every way in refuses with the tier refusal naming Team, and nothing is registered.
func TestTheCommunityEngineRefusesNamingTeam(t *testing.T) {
	t.Parallel()
	engine := CurrentEngine()
	tests := []struct {
		Call func() error
	}{{ // Test 0: Loading a pool file.
		Call: func() error { _, err := engine.LoadPools("pools.yaml"); return err },
	}, { // Test 1: A lone worker token.
		Call: func() error { _, err := engine.SinglePool("token"); return err },
	}, { // Test 2: The control node's endpoints.
		Call: func() error { _, err := engine.Handler(HandlerConfig{}); return err },
	}, { // Test 3: A worker dialing its control node.
		Call: func() error { _, err := engine.Dial(DialConfig{Server: "https://x"}); return err },
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			err := test.Call()
			if !errors.Is(err, license.ErrRequiresTier) {
				t.Fatalf("error = %v, want license.ErrRequiresTier", err)
			}
			if !strings.Contains(err.Error(), "A Team license is required") {
				t.Errorf("error %q does not name Team", err)
			}
		})
	}
	if Registered() {
		t.Error("Registered() = true in a build without the paid module")
	}
}
