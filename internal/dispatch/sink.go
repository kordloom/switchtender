package dispatch

import (
	"context"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/run"
)

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
	// stream carries the redaction state between writes, so a secret split across two chunks is
	// caught rather than passing through in halves. Nil until the first write.
	stream *streamMasker
}

// Write appends p to the run's log and publishes it, redacting any known secret first so a value a
// tool echoes never reaches the stored log or a live viewer. A store failure is retried briefly,
// since a busy single writer under load must not silently drop stored output, then logged and
// reported as a successful write so a persistent logging fault does not tear down the running
// process. The original length is returned because the whole chunk was consumed.
func (s *logSink) Write(p []byte) (int, error) {
	out := p
	if s.mask != nil {
		if s.stream == nil {
			s.stream = &streamMasker{mask: s.mask}
		}
		out = s.stream.next(p)
	}
	s.emit(out)
	// The whole chunk was consumed even when part of it is being held back for the next write.
	return len(p), nil
}

// flush releases whatever the masker withheld from the last write. It is called once the process has
// finished writing, when no further output can complete a secret that straddles a chunk boundary.
func (s *logSink) flush() {
	if s.stream == nil {
		return
	}
	s.emit(s.stream.flush())
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
