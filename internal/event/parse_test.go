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

	play := "Railwarden smoke test"
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

func TestParseResultFields(t *testing.T) {
	t.Parallel()
	line := `{"type":"runner_failed","ts":1719000002,"play":"p","task":"t","host":"db01",` +
		`"message":"boom","stdout":"out","stderr":"err","rc":2,"diff":"-a\n+b","truncated":true}`

	got, err := Parse(strings.NewReader(line))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}

	e := got[0]
	if e.Message != "boom" || e.Stdout != "out" || e.Stderr != "err" {
		t.Errorf("message/stdout/stderr = %q/%q/%q", e.Message, e.Stdout, e.Stderr)
	}
	if e.Diff != "-a\n+b" || !e.Truncated {
		t.Errorf("diff = %q truncated = %v", e.Diff, e.Truncated)
	}
	if e.RC == nil || *e.RC != 2 {
		t.Errorf("rc = %v, want 2", e.RC)
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

func TestParseStatsOutputs(t *testing.T) {
	t.Parallel()
	line := `{"type":"stats","ts":1719000000,"stats":{"web01":{"ok":1}},` +
		`"outputs":{"version":"1.2.3","count":2}}` + "\n"
	events, err := Parse(strings.NewReader(line))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	got := events[0].Outputs
	if got["version"] != "1.2.3" || got["count"] != float64(2) {
		t.Errorf("Outputs = %v, want version 1.2.3 and count 2", got)
	}
}
