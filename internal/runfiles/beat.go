package runfiles

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// BeatInterval is how often the process that owns a run directory increments its heartbeat counter.
const BeatInterval = 30 * time.Second

// beatName is the heartbeat counter file inside every run directory.
const beatName = ".beat"

// heartbeat increments a run directory's counter on a timer for as long as its owner holds the
// directory. It counts on a timer and never on activity, so a run whose tool is quiet for an hour
// still advances it.
//
// The counter is never a liveness oracle: the lock is. A sweep that finds the lock free still
// requires the counter to have stood still across two observations before it deletes anything, so
// the counter can only hold a deletion back, never cause one.
type heartbeat struct {
	// file is the open counter file, rewritten in place on every tick.
	file *os.File
	// stop is closed to end the ticking goroutine.
	stop chan struct{}
	// done is closed by the ticking goroutine as it exits.
	done chan struct{}
}

// startHeartbeat creates the counter file in dir at zero and increments it every interval until
// halt.
func startHeartbeat(dir string, every time.Duration) (*heartbeat, error) {
	f, err := os.OpenFile(filepath.Join(dir, beatName), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create heartbeat: %w", err)
	}
	if _, err := f.WriteAt(encodeBeat(0), 0); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("write heartbeat: %w", err)
	}
	h := &heartbeat{file: f, stop: make(chan struct{}), done: make(chan struct{})}
	go h.tick(every)
	return h, nil
}

// tick increments the counter every interval until stop is closed.
func (h *heartbeat) tick(every time.Duration) {
	defer close(h.done)
	t := time.NewTicker(every)
	defer t.Stop()
	var n uint64
	for {
		select {
		case <-h.stop:
			return
		case <-t.C:
		}
		n++
		// A write that fails leaves the count where it was, which can only defer a sweep, so it is
		// not worth stopping a run over.
		_, _ = h.file.WriteAt(encodeBeat(n), 0)
	}
}

// halt stops the counter and closes its file, waiting for the ticking goroutine so nothing writes
// into the directory once its removal starts. It is safe on a nil receiver.
func (h *heartbeat) halt() {
	if h == nil {
		return
	}
	close(h.stop)
	<-h.done
	_ = h.file.Close()
}

// encodeBeat renders a count at a fixed width, so each rewrite covers the one before it whole.
func encodeBeat(n uint64) []byte {
	return []byte(fmt.Sprintf("%020d\n", n))
}
