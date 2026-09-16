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

// scalarText is a string that also accepts the other JSON scalars.
//
// A play or task name is not always written as text. YAML reads an unquoted True, No, Off, or 1.0
// as a boolean or a number, so a task written as "- name: No" reaches the callback named with a
// boolean and is emitted as one. Decoding that into a string failed, and because a decode failure
// rejects the whole line, every event for that task was discarded: the run lost those rows from its
// host-by-task matrix and the only trace was a parse error in the server's log.
//
// The plugin now renders names as text at the source, so this is the belt to that suspenders. It
// matters because the failure is silent and total, and because the plugin materialized on a host is
// not guaranteed to be the one this build embeds.
type scalarText string

// UnmarshalJSON accepts a JSON string, number, boolean, or null, and renders it as text.
func (s *scalarText) UnmarshalJSON(raw []byte) error {
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		*s = ""
		return nil
	}
	if raw[0] == '"' {
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			return err
		}
		*s = scalarText(text)
		return nil
	}
	// A bare true, false, or number is rendered as written. An object or an array is not a name
	// under any reading, so it is still refused.
	if raw[0] == '{' || raw[0] == '[' {
		return fmt.Errorf("name is a %s, not a scalar", string(raw[:1]))
	}
	*s = scalarText(raw)
	return nil
}

// wireEvent is the on disk shape the callback plugin writes, one JSON object per line.
type wireEvent struct {
	// Type identifies the event.
	Type string `json:"type"`
	// Ts is the event time as fractional Unix seconds.
	Ts float64 `json:"ts"`
	// Play is the play name.
	Play scalarText `json:"play"`
	// Task is the task name.
	Task scalarText `json:"task"`
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
		Play:      string(w.Play),
		Task:      string(w.Task),
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
