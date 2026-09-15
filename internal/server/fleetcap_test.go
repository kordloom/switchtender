package server

import (
	"testing"
)

// TestFleetAndDriftAreBounded covers a response whose size was the size of the estate.
//
// The window parameter bounds how many recent runs are summarized PER HOST, and it was mistaken for
// a bound on the response: neither the fleet nor the drift endpoint capped the hosts. A ten thousand
// host estate serialized ten thousand records on every front-page load, and raising window
// multiplied the work behind each one rather than the number of them. Both views are ranked worst
// first, so a bounded prefix is the part anybody reads and the rest is weight on every request.
func TestFleetAndDriftAreBounded(t *testing.T) {
	t.Parallel()

	// A ranking longer than the cap is cut, and the full length is still reported so the reader can
	// be told which part of their estate they are looking at.
	big := make([]int, maxFleetHosts*3)
	shown, total := cappedHosts(big)
	if len(shown) != maxFleetHosts {
		t.Errorf("shown = %d hosts, want the cap of %d", len(shown), maxFleetHosts)
	}
	if total != len(big) {
		t.Errorf("total = %d, want %d: a reader shown a prefix has to be told of what",
			total, len(big))
	}

	// At or under the cap nothing is cut and the two agree, which is every ordinary install.
	small := make([]int, 7)
	shown, total = cappedHosts(small)
	if len(shown) != len(small) || total != len(small) {
		t.Errorf("small estate: shown %d total %d, want %d and %d",
			len(shown), total, len(small), len(small))
	}

	// Exactly at the cap is not truncated, so no install is told it is missing rows it is not.
	exact := make([]int, maxFleetHosts)
	shown, total = cappedHosts(exact)
	if len(shown) != maxFleetHosts || total != maxFleetHosts {
		t.Errorf("at the cap: shown %d total %d, want %d and %d",
			len(shown), total, maxFleetHosts, maxFleetHosts)
	}
}
