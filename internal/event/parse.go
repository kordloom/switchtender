package event

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"time"
)

// wireEvent is the on disk shape the callback plugin writes, one JSON object per line.
type wireEvent struct {
	// Type identifies the event.
	Type string `json:"type"`
	// Ts is the event time as fractional Unix seconds.
	Ts float64 `json:"ts"`
	// Play is the play name.
	Play string `json:"play"`
	// Task is the task name.
	Task string `json:"task"`
	// Host is the target host.
	Host string `json:"host"`
	// Changed reports a state change on the host.
	Changed bool `json:"changed"`
	// Message is the task result message.
	Message string `json:"message"`
	// Stdout is captured standard output.
	Stdout string `json:"stdout"`
	// Stderr is captured standard error.
	Stderr string `json:"stderr"`
	// RC is the module return code.
	RC *int `json:"rc"`
	// Diff is a captured change diff.
	Diff string `json:"diff"`
	// Truncated reports that captured fields were cut.
	Truncated bool `json:"truncated"`
	// Facts holds a host's gathered system facts on a facts event.
	Facts map[string]string `json:"facts"`
	// Stats holds per host recap totals on stats events.
	Stats map[string]HostStats `json:"stats"`
	// Outputs holds set_stats values published by the playbook.
	Outputs map[string]any `json:"outputs"`
}

// event converts a wireEvent into an Event.
func (w wireEvent) event() Event {
	return Event{
		Type:      Type(w.Type),
		Time:      unixFloat(w.Ts),
		Play:      w.Play,
		Task:      w.Task,
		Host:      w.Host,
		Changed:   w.Changed,
		Message:   w.Message,
		Stdout:    w.Stdout,
		Stderr:    w.Stderr,
		RC:        w.RC,
		Diff:      w.Diff,
		Truncated: w.Truncated,
		Stats:     w.Stats,
		Outputs:   w.Outputs,
		Facts:     w.Facts,
	}
}

// unixFloat converts fractional Unix seconds into a time.Time in UTC, refusing a value that cannot
// be written back out.
//
// Converting an out-of-range float to int64 is undefined in Go and time.Unix builds whatever it is
// handed, so a timestamp of 1e18 produced the year 31688740476. Nothing rejects it until the batch
// is marshaled for storage, where encoding/json refuses any year outside 0 to 9999 and the whole
// batch is dropped with a log line. One bad line silently cost a run its history, which is the
// wrong way for anything in this product to fail.
func unixFloat(ts float64) time.Time {
	if ts == 0 || math.IsNaN(ts) || math.IsInf(ts, 0) {
		return time.Time{}
	}
	if ts < minEventUnix || ts > maxEventUnix {
		return time.Time{}
	}
	sec, frac := math.Modf(ts)
	return time.Unix(int64(sec), int64(frac*float64(time.Second))).UTC()
}

// minEventUnix and maxEventUnix bound a timestamp to years a JSON encoder can write, which is what
// every store marshals through. Outside them the value is dropped rather than carried to a failure
// that takes the batch with it.
const (
	minEventUnix = -62135596800 // 0001-01-01
	maxEventUnix = 253402300799 // 9999-12-31
)

// Parse reads newline delimited JSON events from r and returns them in order. Blank lines are
// ignored. A malformed line returns ErrParse wrapped with the line number.
func Parse(r io.Reader) ([]Event, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var events []Event
	for line := 1; scanner.Scan(); line++ {
		e, ok, err := ParseLine(scanner.Bytes())
		if err != nil {
			return nil, fmt.Errorf("%w: line %d", err, line)
		}
		if !ok {
			continue
		}
		events = append(events, e)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrParse, err)
	}
	return events, nil
}

// ParseLine parses a single newline delimited JSON event line. It reports ok false for a blank
// line and ErrParse for a malformed one, so a live tailer can skip one damaged line without
// dropping the rest of its batch, and without the per-line scanner buffer Parse allocates.
func ParseLine(raw []byte) (Event, bool, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return Event{}, false, nil
	}
	var w wireEvent
	if err := json.Unmarshal(raw, &w); err != nil {
		return Event{}, false, fmt.Errorf("%w: %w", ErrParse, err)
	}
	return w.event(), true, nil
}
