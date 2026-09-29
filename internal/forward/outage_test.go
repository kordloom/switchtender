package forward

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// TestOutageReportsTheOutageNotEveryRetry pins what a sink outage writes to the log. Every retry was
// logged at error level with a stack trace, so an hour-long SIEM outage wrote dozens of identical
// multi-line entries.
func TestOutageReportsTheOutageNotEveryRetry(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 9, 29, 3, 0, 0, 0, time.UTC)
	down := errors.New("the collector is down")
	refused := errors.New("the collector refused the batch")
	type step struct {
		// After is how long after start the step happens.
		After time.Duration
		// Err is the delivery failure, nil for a success.
		Err error
		// WantLine is a fragment of the line logged, empty for no line.
		WantLine string
	}
	tests := [][]step{{ // Test 0: The first failure is reported and a repeat inside the reminder is not.
		{After: 0, Err: down, WantLine: "delivery failed and will be retried"},
		{After: time.Minute, Err: down},
		{After: 2 * time.Minute, Err: down},
	}, { // Test 1: A failure that changes is reported even inside the reminder.
		{After: 0, Err: down, WantLine: "delivery failed"},
		{After: time.Minute, Err: refused, WantLine: "still failing after 2 attempts over 1m0s: the collector refused"},
	}, { // Test 2: An unchanged failure is reported again once the reminder has passed.
		{After: 0, Err: down, WantLine: "delivery failed"},
		{After: 5 * time.Minute, Err: down},
		{After: outageReminder, Err: down, WantLine: "still failing after 3 attempts over 10m0s"},
	}, { // Test 3: Recovery is reported once, and a later failure starts a new outage.
		{After: 0, Err: down, WantLine: "delivery failed"},
		{After: time.Minute, Err: down},
		{After: 2 * time.Minute, WantLine: "resumed after 2 failed attempts over 2m0s"},
		{After: 3 * time.Minute},
		{After: 4 * time.Minute, Err: down, WantLine: "delivery failed and will be retried"},
	}, { // Test 4: Healthy deliveries log nothing.
		{After: 0}, {After: time.Minute},
	}}
	for testNum, steps := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			var o outage
			for i, s := range steps {
				now := start.Add(s.After)
				var got string
				if s.Err != nil {
					got = o.failed(now, s.Err)
				} else {
					got = o.recovered(now)
				}
				switch {
				case s.WantLine == "" && got != "":
					t.Errorf("step %d logged %q, want nothing", i, got)
				case s.WantLine != "" && !strings.Contains(got, s.WantLine):
					t.Errorf("step %d logged %q, want it to contain %q", i, got, s.WantLine)
				}
			}
		})
	}
}

// TestForwarderOutageLogsCarryNoStackTrace runs the real loop against a refusing sink with a logger
// that attaches stack traces at error level, as the production logger does, and checks the outage
// line carries none. The cause of a delivery failure is the sink, never the frames that noticed it.
func TestForwarderOutageLogsCarryNoStackTrace(t *testing.T) {
	t.Parallel()
	core, logs := observer.New(zap.InfoLevel)
	log := zap.New(core, zap.AddStacktrace(zap.ErrorLevel))
	sink := &captureSink{refuse: true}
	f := NewForwarder(seedAudits(t, 1), []Sink{sink}, filepath.Join(t.TempDir(), "cursor.json"),
		time.Second, log)
	if err := f.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer f.Close()
	deadline := time.Now().Add(5 * time.Second)
	for logs.FilterMessageSnippet("delivery failed").Len() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	failures := logs.FilterMessageSnippet("delivery failed").All()
	if len(failures) != 1 {
		t.Fatalf("outage lines = %d, want 1", len(failures))
	}
	if failures[0].Stack != "" {
		t.Errorf("the outage line carries a stack trace:\n%s", failures[0].Stack)
	}
}
