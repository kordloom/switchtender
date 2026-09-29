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

// FormatTime renders a time as a UTC string for storage.
//
// The fractional second keeps RFC 3339's trimming rather than being padded to a fixed width. Padding
// was tried and reverted: it changed the stored form of every timestamp without migrating the rows
// already written, and ClaimDue compares next_run_at for exact text equality, so on upgrade every
// existing schedule became unclaimable and stopped firing, silently and permanently.
//
// Text comparison of two different widths can disagree with time comparison, but only for instants
// inside the same second, and every place that relies on the order tolerates a sub-second tie. The
// one comparison where being wrong mattered, the PostgreSQL lease sweep, casts to a real timestamp
// and does not depend on the text form at all.
func FormatTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
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
