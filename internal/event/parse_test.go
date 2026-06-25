package event

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

func TestParseFixture(t *testing.T) {
	t.Parallel()
	f, err := os.Open("testdata/events.ndjson")
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer func() { _ = f.Close() }()

	got, err := Parse(f)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}

	play := "Yardmaster smoke test"
	want := []Event{
		{Type: TypePlayStart, Time: time.Unix(1719000000, 0).UTC(), Play: play},
		{Type: TypeTaskStart, Time: time.Unix(1719000000, 5e8).UTC(), Play: play, Task: "Say hello"},
		{
			Type: TypeRunnerOK, Time: time.Unix(1719000001, 0).UTC(),
			Play: play, Task: "Say hello", Host: "localhost",
		},
		{
			Type: TypeStats, Time: time.Unix(1719000001, 5e8).UTC(),
			Stats: map[string]HostStats{"localhost": {OK: 1}},
		},
	}
	if diff := cmp.Diff(want, got, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("Parse() mismatch (-want +got):\n%s", diff)
	}
}

func TestParse(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name      string
		In        string
		WantCount int
		Want      error
	}{
		{ // Test 0: Blank lines are ignored.
			Name: "blank lines", In: "\n   \n{\"type\":\"play_start\",\"ts\":1}\n\n", WantCount: 1,
		},
		{ // Test 1: Empty input yields no events.
			Name: "empty", In: "", WantCount: 0,
		},
		{ // Test 2: A malformed line is an error.
			Name: "malformed", In: "{\"type\":\"play_start\"}\n{bad", Want: ErrParse,
		},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got, err := Parse(strings.NewReader(test.In))
			if test.Want != nil {
				if !errors.Is(err, test.Want) {
					t.Fatalf("Parse() error = %v, want %v", err, test.Want)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse() error = %v", err)
			}
			if len(got) != test.WantCount {
				t.Errorf("len = %d, want %d", len(got), test.WantCount)
			}
		})
	}
}
