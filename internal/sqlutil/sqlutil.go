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
// inside the same second. Queries that need the real order sort on [TimeOrder] instead of the raw
// column. The one comparison where being wrong mattered, the PostgreSQL lease sweep, casts to a real
// timestamp and does not depend on the text form at all.
func FormatTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

// TimeOrder is the SQL expression that sorts a stored ran_at column in true chronological order,
// for the SQLite and PostgreSQL stores alike.
//
// Sorting the raw column is wrong below the second. FormatTime trims the fractional second, so an
// instant on a whole second has no fraction at all, and the trailing "Z" then outranks the "." of a
// later instant in the same second: "10:00:00Z" sorts above "10:00:00.5Z". The same inversion hits
// two fractions where one is a prefix of the other, since "Z" also outranks a digit, so
// "10:00:00.1Z" sorts above "10:00:00.100001Z". Both put a later run ahead of an earlier one, which
// reorders host history and moves the wrong rows inside a ROW_NUMBER window.
//
// Dropping the trailing "Z" removes the character that outranks "." and the digits. What is left is
// compared position by position, and because a trimmed fraction never ends in a zero, a string that
// is a prefix of another is always the smaller instant. Ordering is therefore fixed without
// rewriting a single stored row, which matters because the stored format is load bearing: ClaimDue
// compares next_run_at for exact text equality, and a past attempt to change the format silently
// stopped every existing schedule from firing.
const TimeOrder = `rtrim(ran_at, 'Z')`

// GatheredOrder is [TimeOrder] for a gathered_at column, and it exists for the same reason: the
// host state history is ordered on it to decide which snapshots survive a depth cap, and sorting
// the raw text puts a later instant ahead of an earlier one inside the same second. Trimming a
// snapshot that is actually the newest is not recoverable, so this ordering is load-bearing.
const GatheredOrder = `rtrim(gathered_at, 'Z')`

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

// ParseTimeOrAbsent parses a stored time and treats one it cannot read as absent, returning the zero
// time.
//
// It exists for a stamp on a row that is listed alongside others, where refusing the row refuses
// every row with it. A scan error becomes a query error, so one unreadable stamp took down the whole
// listing: one schedule with a damaged next_run_at made the schedules page error and the scheduler
// enumerate nothing, so no schedule in the install fired, not just the damaged one. The migration
// that normalizes these stamps deliberately leaves one it cannot parse in place, reasoning that the
// row is already broken, and this is what keeps the cost where that reasoning assumes it is.
//
// It is for stamps and for nothing else. A stamp says when, and a reader that does not know when is
// a reader missing a detail; the fields that say what a row does are not eligible, because guessing
// at those could run work nobody asked for. Audit chain stamps are not eligible either, for the
// opposite reason: they are covered by the chain digest, so reading a damaged one as absent would
// change what verification computes over.
func ParseTimeOrAbsent(s string) time.Time {
	t, err := ParseTime(s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// ParseNullTimeOrAbsent parses an optional stored time and treats one it cannot read as absent,
// returning nil. It carries the same reasoning and the same limits as ParseTimeOrAbsent.
func ParseNullTimeOrAbsent(s sql.NullString) *time.Time {
	t, err := ParseNullTime(s)
	if err != nil {
		return nil
	}
	return t
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
