package relay

import "time"

// LogBatchDelayForTest exposes how long a partial log batch waits before it posts, so the external
// tests can pace writes against the delay flush without hard-coding it.
func LogBatchDelayForTest() time.Duration { return logBatchDelay }
