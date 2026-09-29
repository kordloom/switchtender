package event

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

// TestOutOfRangeTimestampCannotDropABatch pins that a timestamp no encoder can write is dropped on
// its own rather than taking the batch with it.
//
// Converting an out-of-range float to int64 is undefined in Go, and time.Unix builds whatever it is
// handed, so ts=1e18 produced the year 31688740476. Nothing noticed until the batch was marshaled
// for storage, where encoding/json refuses any year outside 0 to 9999 and the append failed, so one
// line silently cost a run its whole event history.
func TestOutOfRangeTimestampCannotDropABatch(t *testing.T) {
	t.Parallel()
	lines := []string{
		`{"type":"runner_ok","host":"a","task":"one","ts":1754000000}`,
		`{"type":"runner_ok","host":"b","task":"two","ts":1e18}`,
		`{"type":"runner_ok","host":"c","task":"three","ts":253402300800}`,
		`{"type":"runner_ok","host":"d","task":"four","ts":-1e18}`,
		`{"type":"runner_ok","host":"e","task":"five","ts":1754000001}`,
	}
	events, err := Parse(strings.NewReader(strings.Join(lines, "\n")))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if len(events) != len(lines) {
		t.Fatalf("Parse() returned %d events, want %d", len(events), len(lines))
	}
	// Everything must survive a round trip, because that is what every store does before insert.
	if _, err := json.Marshal(events); err != nil {
		t.Errorf("a parsed batch cannot be marshaled, so the whole batch is dropped: %v", err)
	}
	// The good lines kept their times; the impossible ones were zeroed rather than fabricated.
	if events[0].Time.IsZero() || events[4].Time.IsZero() {
		t.Error("an ordinary timestamp was discarded")
	}
	for _, i := range []int{1, 2, 3} {
		if !events[i].Time.IsZero() {
			t.Errorf("event %d kept an unwritable time %v", i, events[i].Time)
		}
	}
	// NaN and infinity are the same class of answer.
	for _, ts := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if got := unixFloat(ts); !got.IsZero() {
			t.Errorf("unixFloat(%v) = %v, want the zero time", ts, got)
		}
	}
}
