package relay

import (
	"context"
	"io"
	"time"
)

// DecodeCappedForTest exposes the streaming array decoder so the external tests can prove the cap is
// enforced during the decode, not after the whole array is already in memory.
func DecodeCappedForTest[T any](r io.Reader, max int) ([]T, error) { return decodeCapped[T](r, max) }

// ErrTooManyElements is the sentinel decodeCapped returns once the body passes the element cap.
var ErrTooManyElements = errTooManyElements

// MaxRelayElementsForTest is the per-call element cap the relay handlers enforce.
func MaxRelayElementsForTest() int { return maxRelayElements }

// LogBatchDelayForTest exposes how long a partial log batch waits before it posts, so the external
// tests can pace writes against the delay flush without hard-coding it.
func LogBatchDelayForTest() time.Duration { return logBatchDelay }

// FlushLogForTest posts whatever a run has buffered, so a test can drive the recovery path without
// building a full run save.
func (t *httpTransport) FlushLogForTest(ctx context.Context, id string) error {
	return t.flushLog(ctx, id)
}
