package sqlutil_test

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

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

// TestStringsRoundTrip covers storing free text that may hold commas, which an id list would split.
func TestStringsRoundTrip(t *testing.T) {
	t.Parallel()
	tests := []struct {
		In         []string
		WantStored string
	}{{ // Test 0: An empty list stores as empty.
		In: nil, WantStored: "",
	}, { // Test 1: Text holding commas and quotes survives whole.
		In:         []string{`site.yml: task "a, b" sets check_mode to false`, "two"},
		WantStored: `["site.yml: task \"a, b\" sets check_mode to false","two"]`,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			stored := sqlutil.JSONStrings(test.In)
			if diff := cmp.Diff(test.WantStored, stored); diff != "" {
				t.Errorf("JSONStrings() mismatch (-want +got):\n%s", diff)
			}
			got, err := sqlutil.ParseStrings(stored)
			if err != nil {
				t.Fatalf("ParseStrings() error = %v", err)
			}
			if diff := cmp.Diff(test.In, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("ParseStrings() round trip mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestParseStringsRefusesNonJSON covers a corrupted column, which is reported rather than read as
// an empty list.
func TestParseStringsRefusesNonJSON(t *testing.T) {
	t.Parallel()
	if _, err := sqlutil.ParseStrings("not json"); err == nil {
		t.Error("ParseStrings() accepted a column that is not JSON")
	}
}

func TestOrderConstants(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Col  string
		Want string
	}{
		{Col: "ran_at", Want: sqlutil.TimeOrder},          // Test 0: Run time.
		{Col: "gathered_at", Want: sqlutil.GatheredOrder}, // Test 1: Gather time.
	}
	for i, test := range tests {
		t.Run(fmt.Sprintf("test %d", i), func(t *testing.T) {
			t.Parallel()
			if got := sqlutil.Order(test.Col); got != test.Want {
				t.Errorf("Order(%q) = %q, want %q", test.Col, got, test.Want)
			}
		})
	}
	if got, want := sqlutil.OrderPG("ran_at"), `rtrim(ran_at, 'Z') COLLATE "C"`; got != want {
		t.Errorf("OrderPG() = %q, want %q", got, want)
	}
}

func TestOrderKeySortsChronologically(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 10, 19, 10, 0, 0, 0, time.UTC)
	offsets := []time.Duration{
		0,
		100 * time.Millisecond,
		100001 * time.Microsecond,
		500 * time.Millisecond,
		500001 * time.Microsecond,
		510 * time.Millisecond,
		750 * time.Millisecond,
		900 * time.Millisecond,
		999999 * time.Microsecond,
		999999999 * time.Nanosecond,
	}
	type stamp struct {
		Text string
		At   time.Time
	}
	var stamps []stamp
	for i, off := range offsets {
		at := base.Add(off)
		text := sqlutil.FormatTime(at)
		if i%2 == 1 && off%time.Microsecond == 0 {
			// Six-digit form as PostgreSQL's own clock writes it.
			text = at.Format("2006-01-02T15:04:05.000000Z")
		}
		stamps = append(stamps, stamp{Text: text, At: at})
	}
	key := func(s string) string { return strings.TrimSuffix(s, "Z") }

	tests := []struct {
		Name string
		Less func(a, b stamp) bool
	}{
		{Name: "key", Less: func(a, b stamp) bool { return key(a.Text) < key(b.Text) }},
	}
	for i, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", i, test.Name), func(t *testing.T) {
			t.Parallel()
			sorted := make([]stamp, 0, len(stamps))
			for k := len(stamps) - 1; k >= 0; k-- {
				sorted = append(sorted, stamps[k])
			}
			sort.SliceStable(sorted, func(x, y int) bool { return test.Less(sorted[x], sorted[y]) })
			for j := 1; j < len(sorted); j++ {
				if sorted[j].At.Before(sorted[j-1].At) {
					t.Errorf("%q sorted after %q but is earlier", sorted[j].Text, sorted[j-1].Text)
				}
			}
		})
	}

	for i, a := range stamps {
		for j, b := range stamps {
			want := a.At.Compare(b.At)
			got := strings.Compare(sqlutil.OrderBind(a.At), key(b.Text))
			if want == 0 {
				// A padded stored form is its own trimmed bind plus zeros, so it never sorts below it.
				if got > 0 {
					t.Errorf("OrderBind(%d) vs key(%d) = %d, want <= 0", i, j, got)
				}
				continue
			}
			if got != want {
				t.Errorf("OrderBind(%d) vs key(%d) = %d, want %d", i, j, got, want)
			}
		}
	}
}

// storeTimeColumn matches a bare stored time column, optionally table-qualified.
var storeTimeColumn = regexp.MustCompile(`^(\w+\.)?(\w+_at|last_seen|first_seen)$`)

// storeOrderBy finds the start of each ORDER BY clause.
var storeOrderBy = regexp.MustCompile(`(?i)ORDER BY\s+`)

// storeClauseEnd matches what follows the last ORDER BY term in a clause.
var storeClauseEnd = regexp.MustCompile(`(?is)\s+(LIMIT|OFFSET|FOR)\b.*$`)

// storeRawCompare matches a stored time column compared with a bind value or spliced text without
// the order key.
var storeRawCompare = regexp.MustCompile("(^|[^\\w(.'])((\\w+\\.)?(\\w+_at|last_seen|first_seen))\\s*(<=|>=|<|>)" +
	"\\s*(\\?|\\$\\d|\\$%d|\"\\s*\\+|`\\s*\\+)")

// storeOrderSuffix matches the direction and collation that may follow an ORDER BY term.
var storeOrderSuffix = regexp.MustCompile(`(?i)(\s+COLLATE\s+\\?"C\\?")?(\s+(ASC|DESC))?(\s+NULLS\s+(FIRST|LAST))?$`)

// TestStoresUseOrderKey reads each SQL store's query text and fails on any ORDER BY term or range
// comparison on a stored time that does not go through the order key. The raw text sorts a whole
// second after its own fractions, so one such query puts rows out of time order without failing.
func TestStoresUseOrderKey(t *testing.T) {
	t.Parallel()
	// exempt names query text whose time column is not stored text. PostgreSQL keeps a credential
	// type's created_at as UnixNano in a BIGINT, which already sorts in time order and has no Z.
	exempt := map[string][]string{
		"pgstore": {"FROM credential_types ORDER BY created_at, id"},
	}
	for _, dir := range []string{"sqlitestore", "pgstore"} {
		t.Run(dir, func(t *testing.T) {
			t.Parallel()
			files, err := filepath.Glob(filepath.Join("..", dir, "*.go"))
			if err != nil {
				t.Fatalf("glob %s: %v", dir, err)
			}
			if len(files) == 0 {
				t.Fatalf("no Go files in %s", dir)
			}
			for _, file := range files {
				if strings.HasSuffix(file, "_test.go") {
					continue
				}
				src, err := os.ReadFile(file)
				if err != nil {
					t.Fatalf("read %s: %v", file, err)
				}
				text := string(src)
				for _, ok := range exempt[dir] {
					text = strings.ReplaceAll(text, ok, "")
				}
				for _, bad := range rawTimeUses(text) {
					t.Errorf("%s: %q does not use the order key, see sqlutil.Order", file, bad)
				}
			}
		})
	}
}

// rawTimeUses returns each ORDER BY term and range comparison in src that uses a stored time
// column bare. Comment lines are skipped, since they quote SQL to explain it.
func rawTimeUses(src string) []string {
	var code []string
	for _, line := range strings.Split(src, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		code = append(code, line)
	}
	text := strings.Join(code, "\n")
	var bad []string
	for _, loc := range storeOrderBy.FindAllStringIndex(text, -1) {
		for _, term := range orderTerms(text[loc[1]:]) {
			term = storeOrderSuffix.ReplaceAllString(strings.TrimSpace(term), "")
			if storeTimeColumn.MatchString(term) {
				bad = append(bad, "ORDER BY "+term)
			}
		}
	}
	for _, m := range storeRawCompare.FindAllStringSubmatch(text, -1) {
		bad = append(bad, strings.TrimSpace(m[0]))
	}
	return bad
}

// orderTerms splits the ORDER BY clause at the start of text into its terms. The clause ends at the
// end of its line or its query text, or at a parenthesis it did not open, and commas inside a
// term's own parentheses do not split it.
func orderTerms(text string) []string {
	var terms []string
	depth, start := 0, 0
	end := len(text)
scan:
	for i, r := range text {
		switch r {
		case '(':
			depth++
		case ')':
			if depth == 0 {
				end = i
				break scan
			}
			depth--
		case ',':
			if depth == 0 {
				terms = append(terms, text[start:i])
				start = i + 1
			}
		case '\n', '`', '"':
			end = i
			break scan
		}
	}
	if start < end {
		terms = append(terms, text[start:end])
	}
	if n := len(terms); n > 0 {
		terms[n-1] = storeClauseEnd.ReplaceAllString(terms[n-1], "")
	}
	return terms
}
