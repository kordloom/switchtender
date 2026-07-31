package relay

import (
	"context"
	"time"
)

// LogBatchDelayForTest exposes how long a partial log batch waits before it posts, so the external
// tests can pace writes against the delay flush without hard-coding it.
func LogBatchDelayForTest() time.Duration { return logBatchDelay }

// FlushLogForTest posts whatever a run has buffered, so a test can drive the recovery path without
// building a full run save.
func (t *httpTransport) FlushLogForTest(ctx context.Context, id string) error {
	return t.flushLog(ctx, id)
}
