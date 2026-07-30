package sqlutil_test

import (
	"database/sql"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/sqlutil"
)

func TestIDsRoundTrip(t *testing.T) {
	t.Parallel()
	tests := []struct {
		In   []string
		Want string
	}{
		{In: nil, Want: ""},                          // Test 0: Empty list stores as empty.
		{In: []string{"a"}, Want: "a"},               // Test 1: One id.
		{In: []string{"a", "b", "c"}, Want: "a,b,c"}, // Test 2: Several ids.
	}
	for i, test := range tests {
		joined := sqlutil.JoinIDs(test.In)
		if joined != test.Want {
			t.Errorf("test %d: JoinIDs() = %q, want %q", i, joined, test.Want)
		}
		if diff := cmp.Diff(test.In, sqlutil.SplitIDs(joined)); diff != "" {
			t.Errorf("test %d: SplitIDs round trip mismatch (-want +got):\n%s", i, diff)
		}
	}
}

func TestBoolToInt(t *testing.T) {
	t.Parallel()
	if sqlutil.BoolToInt(true) != 1 || sqlutil.BoolToInt(false) != 0 {
		t.Error("BoolToInt mapping wrong")
	}
}

func TestMapRoundTrip(t *testing.T) {
	t.Parallel()
	if sqlutil.JSONMap(nil) != "" {
		t.Error("JSONMap(nil) should store empty")
	}
	stored := sqlutil.JSONMap(map[string]any{"env": "prod"})
	m, err := sqlutil.ParseMap(stored)
	if err != nil || m["env"] != "prod" {
		t.Errorf("ParseMap(%q) = %v, %v", stored, m, err)
	}
	if m, err := sqlutil.ParseMap(""); err != nil || m != nil {
		t.Errorf("ParseMap(empty) = %v, %v, want nil, nil", m, err)
	}
	if _, err := sqlutil.ParseMap("{broken"); err == nil {
		t.Error("ParseMap of invalid JSON should fail")
	}
}

func TestTimeRoundTrip(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 7, 27, 1, 2, 3, 456789000, time.UTC)
	got, err := sqlutil.ParseTime(sqlutil.FormatTime(at))
	if err != nil || !got.Equal(at) {
		t.Errorf("time round trip = %v, %v, want %v", got, err, at)
	}
}

func TestNullables(t *testing.T) {
	t.Parallel()
	if sqlutil.NullInt(nil) != nil || sqlutil.NullString(nil) != nil || sqlutil.NullTime(nil) != nil {
		t.Error("nil optionals should map to nil")
	}
	n, s := 7, "x"
	if sqlutil.NullInt(&n) != 7 || sqlutil.NullString(&s) != "x" {
		t.Error("set optionals should map to their values")
	}
	at := time.Date(2026, 7, 27, 0, 0, 0, 0, time.UTC)
	if sqlutil.NullTime(&at) != sqlutil.FormatTime(at) {
		t.Error("set optional time should map to its stored form")
	}

	got, err := sqlutil.ParseNullTime(sql.NullString{Valid: true, String: sqlutil.FormatTime(at)})
	if err != nil || got == nil || !got.Equal(at) {
		t.Errorf("ParseNullTime(valid) = %v, %v", got, err)
	}
	if got, err := sqlutil.ParseNullTime(sql.NullString{}); err != nil || got != nil {
		t.Errorf("ParseNullTime(null) = %v, %v, want nil, nil", got, err)
	}
}

func TestQueuePlaceholders(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name      string
		Queues    []string
		Style     string
		First     int
		WantList  string
		WantCount int
	}{{ // Test 0: No queues still binds the default queue.
		Name: "default only", Queues: nil, Style: "?", WantList: "?", WantCount: 1,
	}, { // Test 1: SQLite style repeats question marks and ignores the offset.
		Name: "sqlite", Queues: []string{"", "gpu"}, Style: "?", First: 7, WantList: "?, ?", WantCount: 2,
	}, { // Test 2: Postgres style numbers from the offset the caller gives.
		Name: "postgres", Queues: []string{"", "gpu"}, Style: "$", First: 2, WantList: "$2, $3", WantCount: 2,
	}, { // Test 3: A different offset renumbers the whole list.
		Name: "postgres offset", Queues: []string{"a"}, Style: "$", First: 5, WantList: "$5", WantCount: 1,
	}}
	for i, test := range tests {
		list, args := sqlutil.QueuePlaceholders(test.Queues, test.Style, test.First)
		if list != test.WantList || len(args) != test.WantCount {
			t.Errorf("test %d (%s): QueuePlaceholders() = %q with %d args, want %q with %d",
				i, test.Name, list, len(args), test.WantList, test.WantCount)
		}
	}
}
