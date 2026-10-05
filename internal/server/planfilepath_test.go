package server

import (
	"encoding/json"
	"fmt"
	"math"
	"testing"

	"github.com/kordloom/switchtender/internal/relay"
)

// TestOnlyThePlanFileRouteTakesTheLargerBody pins the one relay route allowed a body past the
// ordinary cap: the route a worker hands its saved plan file to, which a plan of an ordinary
// configuration outgrows a megabyte with. Nothing that only resembles it, by a traversal or by case,
// and no other relay route, gets the larger cap.
func TestOnlyThePlanFileRouteTakesTheLargerBody(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Path is the request path.
		Path string
		// Want reports whether the larger cap applies.
		Want bool
	}{{ // Test 0: The route itself.
		Path: "/relay/v1/runs/run_123/propose-apply", Want: true,
	}, { // Test 1: Another relay route on the same run.
		Path: "/relay/v1/runs/run_123/save", Want: false,
	}, { // Test 2: A traversal that lands elsewhere.
		Path: "/relay/v1/runs/run_123/propose-apply/../save", Want: false,
	}, { // Test 3: No run named.
		Path: "/relay/v1/runs//propose-apply", Want: false,
	}, { // Test 4: An API route.
		Path: "/v1/runs/run_123/approve", Want: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := planFilePath(test.Path); got != test.Want {
				t.Errorf("planFilePath(%q) = %v, want %v", test.Path, got, test.Want)
			}
		})
	}
}

// TestThePlanFileRouteCarriesAPlanFileAtTheRelayLimit ties the limit the relay states to the cap
// this route takes. A propose request carrying a plan file of relay.MaxPlanFileBytes, base64
// encoded, fits under the upload cap, so the limit a refusal states is the one a worker meets.
func TestThePlanFileRouteCarriesAPlanFileAtTheRelayLimit(t *testing.T) {
	t.Parallel()
	body, err := json.Marshal(map[string]any{
		"destroys": math.MaxInt64, "read": true, "plan_file": make([]byte, relay.MaxPlanFileBytes),
	})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if len(body) > maxUploadBodyBytes {
		t.Errorf("a propose request at the plan file limit is %d bytes, past the route's cap of %d",
			len(body), maxUploadBodyBytes)
	}
}
