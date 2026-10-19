package relay

import (
	"bytes"
	"context"
	"testing"
)

// TestSettleLogCountsWhatTheCapDropped pins that the bytes capBuffer removes while a relay is down
// are counted and reported, since they are the one loss nothing else can see: once the relay returns
// the surviving window lands and every post succeeds.
func TestSettleLogCountsWhatTheCapDropped(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tr, flaky, backing := newFlakyTransport(t, "run_settle_capped")
	flaky.down.Store(true)

	const chunks = 12
	for i := range chunks {
		if err := tr.AppendLog(ctx, "run_settle_capped",
			bytes.Repeat([]byte{byte('0' + i%10)}, logBatchBytes)); err != nil {
			t.Fatalf("AppendLog(%d) error = %v", i, err)
		}
	}
	if err := tr.flushLog(ctx, "run_settle_capped"); err == nil {
		t.Fatal("flushLog() answered nil with the relay down")
	}

	flaky.down.Store(false)
	got := tr.SettleLog(ctx, "run_settle_capped")
	if want := int64(chunks*logBatchBytes - logBatchLimit); got != want {
		t.Errorf("SettleLog() = %d bytes lost, want %d, the output past the cap", got, want)
	}
	stored := waitForLog(t, backing, "run_settle_capped", logBatchLimit)
	if len(stored) != logBatchLimit {
		t.Errorf("the store holds %d bytes, want the last %d the cap kept", len(stored), logBatchLimit)
	}
	if n := bufferedBytes(t, tr, "run_settle_capped"); n != 0 {
		t.Errorf("buffered = %d bytes after the settle, want none", n)
	}
}

// TestSettleLogAbandonsWhatItCouldNotDeliver pins that the settle's answer is final: what is still
// held when it gives up is counted and dropped, so a later flush cannot deliver bytes the run's
// record says were lost. The window is cut short by a canceled context rather than waited out.
func TestSettleLogAbandonsWhatItCouldNotDeliver(t *testing.T) {
	t.Parallel()
	tr, flaky, backing := newFlakyTransport(t, "run_settle_abandoned")
	flaky.down.Store(true)

	const tail = "PLAY RECAP *** failed=1\n"
	if err := tr.AppendLog(context.Background(), "run_settle_abandoned", []byte(tail)); err != nil {
		t.Fatalf("AppendLog() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := tr.SettleLog(ctx, "run_settle_abandoned"); got != int64(len(tail)) {
		t.Errorf("SettleLog() = %d bytes lost, want %d, the tail it could not deliver", got, len(tail))
	}
	if n := bufferedBytes(t, tr, "run_settle_abandoned"); n != 0 {
		t.Errorf("buffered = %d bytes after the settle abandoned the tail, want none", n)
	}
	flaky.down.Store(false)
	if err := tr.flushLog(context.Background(), "run_settle_abandoned"); err != nil {
		t.Fatalf("flushLog() error = %v once the relay returned", err)
	}
	stored, err := backing.Log(context.Background(), "run_settle_abandoned")
	if err != nil {
		t.Fatalf("Log() error = %v", err)
	}
	if len(stored) != 0 {
		t.Errorf("the store holds %d bytes of a tail the settle reported lost, so the run's record "+
			"contradicts its log", len(stored))
	}
}
