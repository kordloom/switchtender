package dispatch

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/run"
)

// logHoldDelay bounds how long output may sit withheld waiting to see whether a secret continues in
// the next write.
//
// The masker holds back as many bytes as the longest secret, and a whole SSH private key is
// registered as one secret, so a run with a key credential withheld well over a kilobyte. A run that
// produces output slowly showed a blank live log for its entire duration. Releasing after a quiet
// interval bounds that: while output is flowing the hold is complete, and a secret can only slip
// past by straddling a pause of this length, which a process writing a secret does not do.
const logHoldDelay = 400 * time.Millisecond

// logSink is an io.Writer that appends process output to a run's stored log and publishes it live.
type logSink struct {
	// store receives appended output.
	store run.Store
	// id is the run whose log is being written.
	id string
	// log records append failures.
	log *zap.Logger
	// publisher receives each chunk for live streaming.
	publisher Publisher
	// mask redacts known secret values before output is stored or streamed, nil when off.
	mask *masker
	// mu guards stream and hold, which the writing goroutine and the hold timer both touch.
	mu sync.Mutex
	// stream carries the redaction state between writes, so a secret split across two chunks is
	// caught rather than passing through in halves. Nil until the first write.
	stream *streamMasker
	// hold releases withheld output once a run goes quiet, so a slow run's log is not blank.
	hold *time.Timer
}

// Write appends p to the run's log and publishes it, redacting any known secret first so a value a
// tool echoes never reaches the stored log or a live viewer. A store failure is retried briefly,
// since a busy single writer under load must not silently drop stored output, then logged and
// reported as a successful write so a persistent logging fault does not tear down the running
// process. The original length is returned because the whole chunk was consumed.
func (s *logSink) Write(p []byte) (int, error) {
	out := p
	if s.mask != nil {
		s.mu.Lock()
		if s.stream == nil {
			s.stream = &streamMasker{mask: s.mask}
		}
		out = s.stream.next(p)
		// Arm the release so withheld bytes do not sit unseen through a quiet stretch.
		if s.hold == nil {
			s.hold = time.AfterFunc(logHoldDelay, s.releaseHeld)
		} else {
			s.hold.Reset(logHoldDelay)
		}
		s.mu.Unlock()
	}
	s.emit(out)
	// The whole chunk was consumed even when part of it is being held back for the next write.
	return len(p), nil
}

// releaseHeld emits whatever the masker is still withholding after a quiet interval, so a run that
// writes slowly is not left with a blank log until it produces enough output to fill the hold.
func (s *logSink) releaseHeld() {
	s.mu.Lock()
	var out []byte
	if s.stream != nil {
		out = s.stream.flush()
	}
	s.mu.Unlock()
	s.emit(out)
}

// flush releases whatever the masker withheld from the last write. It is called once the process has
// finished writing, when no further output can complete a secret that straddles a chunk boundary.
func (s *logSink) flush() {
	s.mu.Lock()
	if s.hold != nil {
		s.hold.Stop()
	}
	var out []byte
	if s.stream != nil {
		out = s.stream.flush()
	}
	s.mu.Unlock()
	s.emit(out)
}

// emit stores and publishes a redacted chunk. A store failure is retried briefly, since a busy single
// writer under load must not silently drop stored output, then logged rather than surfaced, so a
// persistent logging fault does not tear down the running process.
func (s *logSink) emit(out []byte) {
	if len(out) == 0 {
		return
	}
	if err := withRetries(func() error {
		return s.store.AppendLog(context.Background(), s.id, out)
	}); err != nil {
		s.log.Error("dispatch: append log: "+err.Error(), zap.String("run_id", s.id))
	}
	s.publisher.PublishLog(s.id, out)
}
