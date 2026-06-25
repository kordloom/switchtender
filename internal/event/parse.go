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
	// Stats holds per host recap totals on stats events.
	Stats map[string]HostStats `json:"stats"`
}

// event converts a wireEvent into an Event.
func (w wireEvent) event() Event {
	return Event{
		Type:    Type(w.Type),
		Time:    unixFloat(w.Ts),
		Play:    w.Play,
		Task:    w.Task,
		Host:    w.Host,
		Changed: w.Changed,
		Stats:   w.Stats,
	}
}

// unixFloat converts fractional Unix seconds into a time.Time in UTC.
func unixFloat(ts float64) time.Time {
	if ts == 0 {
		return time.Time{}
	}
	sec, frac := math.Modf(ts)
	return time.Unix(int64(sec), int64(frac*float64(time.Second))).UTC()
}

// Parse reads newline delimited JSON events from r and returns them in order. Blank lines are
// ignored. A malformed line returns ErrParse wrapped with the line number.
func Parse(r io.Reader) ([]Event, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var events []Event
	for line := 1; scanner.Scan(); line++ {
		raw := bytes.TrimSpace(scanner.Bytes())
		if len(raw) == 0 {
			continue
		}
		var w wireEvent
		if err := json.Unmarshal(raw, &w); err != nil {
			return nil, fmt.Errorf("%w: line %d: %w", ErrParse, line, err)
		}
		events = append(events, w.event())
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrParse, err)
	}
	return events, nil
}
