package server

import (
	"strings"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/license"
)

// TestANamedQueueIsRefusedWithoutAWorkerLicense covers a field the product sells as Team and did not
// gate.
//
// A queue restricts a run to workers serving that name, and every worker is Team. On Community the
// field saved cleanly, the run was accepted with 202, and then nothing could ever claim it: it sat
// pending forever with no error on any surface to say why. Every other gate in this product refuses
// and names the tier; this one produced a silently stranded run, which is the worst shape a gate can
// take because the operator has no way to learn what happened.
func TestANamedQueueIsRefusedWithoutAWorkerLicense(t *testing.T) {
	tests := []struct {
		// Name says which install and queue is being tested.
		Name string
		// Licensed is whether a Team license is present.
		Licensed bool
		// Queue is the requested queue.
		Queue string
		// WantRefused is whether the request should be refused.
		WantRefused bool
	}{{ // Test 0: Community plus a named queue is the stranding case.
		Name: "community named queue", Licensed: false, Queue: "prod", WantRefused: true,
	}, { // Test 1: The default queue is the server's own pool and needs no worker.
		Name: "community default queue", Licensed: false, Queue: "", WantRefused: false,
	}, { // Test 2: Whitespace is the default queue, not a name.
		Name: "community blank queue", Licensed: false, Queue: "   ", WantRefused: false,
	}, { // Test 3: With Team, a named queue is exactly what was bought.
		Name: "team named queue", Licensed: true, Queue: "prod", WantRefused: false,
	}}

	for _, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			t.Cleanup(func() { license.Set(nil) })
			if test.Licensed {
				lic := &license.License{}
				lic.Claims.Tier = "team"
				lic.Claims.Org = "acme"
				lic.Claims.Expires = time.Now().Add(24 * time.Hour).Format(time.RFC3339)
				license.Set(lic)
			} else {
				license.Set(nil)
			}

			err := allowQueue(test.Queue)
			if (err != nil) != test.WantRefused {
				t.Fatalf("%s: allowQueue(%q) error = %v, want refused: %v",
					test.Name, test.Queue, err, test.WantRefused)
			}
			// A refusal has to name the tier and where to go, the same as every other gate, rather
			// than leaving the operator to discover a run that never ran.
			if test.WantRefused && !strings.Contains(err.Error(), "switchtender.com/pricing") {
				t.Errorf("%s: refusal does not point anywhere: %v", test.Name, err)
			}
		})
	}
}
