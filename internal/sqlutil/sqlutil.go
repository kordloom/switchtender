// Package sqlutil holds the value-mapping helpers the SQLite and PostgreSQL stores share: id lists,
// stored times, optional columns, and queue placeholder lists. One implementation instead of a copy
// per backend.
package sqlutil

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// JoinIDs renders an id list for storage, empty string for none.
func JoinIDs(ids []string) string {
	return strings.Join(ids, ",")
}

// SplitIDs parses a stored id list, nil for an empty string.
func SplitIDs(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

// BoolToInt maps a bool to a database integer.
func BoolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// JSONMap renders a map as JSON for storage, empty string for an empty map.
func JSONMap(m map[string]any) string {
	if len(m) == 0 {
		return ""
	}
	data, err := json.Marshal(m)
	if err != nil {
		return ""
	}
	return string(data)
}

// ParseMap parses a stored JSON map, nil for an empty string.
func ParseMap(s string) (map[string]any, error) {
	if s == "" {
		return nil, nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return nil, fmt.Errorf("parse stored map: %w", err)
	}
	return m, nil
}

// storedTimeLayout is the fixed width UTC layout every stored timestamp uses.
//
// The width is the point. RFC 3339 trims trailing zeros from the fractional second, so a time on an
// exact second renders shorter than one just after it and compares as the larger string: comparing
// stored timestamps as text then disagreed with comparing them as times. Columns are text and are
// compared and ordered as text, so a lease could look older than a cutoff it was actually newer
// than, and the janitor interrupted healthy runs.
//
// Nine fractional digits, always present, preserves the nanosecond precision callers round-trip
// through the stores. The PostgreSQL store pads its own stamps to the same width, so a value written
// by SQL and one written by Go land in a column the same width and sort together.
const storedTimeLayout = "2006-01-02T15:04:05.000000000Z"

// FormatTime renders a time as a fixed width, lexicographically sortable UTC string.
func FormatTime(t time.Time) string {
	return t.UTC().Format(storedTimeLayout)
}

// ParseTime parses a stored time string.
func ParseTime(s string) (time.Time, error) {
	return time.Parse(time.RFC3339Nano, strings.TrimSpace(s))
}

// NullInt maps an optional int to a database value.
func NullInt(v *int) any {
	if v == nil {
		return nil
	}
	return *v
}

// NullString maps an optional string to a database value.
func NullString(v *string) any {
	if v == nil {
		return nil
	}
	return *v
}

// NullTime maps an optional time to a database value.
func NullTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return FormatTime(*t)
}

// ParseNullTime parses an optional stored time.
func ParseNullTime(s sql.NullString) (*time.Time, error) {
	if !s.Valid || s.String == "" {
		return nil, nil
	}
	t, err := ParseTime(s.String)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// QueuePlaceholders builds a comma separated placeholder list and the matching queue args. The
// default queue is always included so an executor never overlooks unqueued work when it serves
// named queues. Style "?" emits SQLite placeholders and ignores first; anything else emits
// PostgreSQL $n placeholders numbered from first, which the caller sets to one past its own
// parameters. It is a parameter rather than a constant because a caller that stops passing a value
// of its own would otherwise silently produce a query numbered for arguments it no longer sends.
func QueuePlaceholders(queues []string, style string, first int) (string, []any) {
	if len(queues) == 0 {
		queues = []string{""}
	}
	parts := make([]string, len(queues))
	args := make([]any, len(queues))
	for i, q := range queues {
		if style == "?" {
			parts[i] = "?"
		} else {
			parts[i] = fmt.Sprintf("$%d", i+first)
		}
		args[i] = q
	}
	return strings.Join(parts, ", "), args
}
