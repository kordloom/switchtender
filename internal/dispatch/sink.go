package dispatch

import (
	"context"

	"go.uber.org/zap"

	"github.com/dcadolph/switchtender/internal/run"
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
}

// Write appends p to the run's log and publishes it, redacting any known secret first so a value a
// tool echoes never reaches the stored log or a live viewer. A store failure is logged but reported
// as a successful write so a transient logging fault does not tear down the running process. The
// original length is returned because the whole chunk was consumed.
func (s *logSink) Write(p []byte) (int, error) {
	out := p
	if s.mask != nil {
		out = s.mask.redact(p)
	}
	if err := s.store.AppendLog(context.Background(), s.id, out); err != nil {
		s.log.Error("dispatch: append log: "+err.Error(), zap.String("run_id", s.id))
	}
	s.publisher.PublishLog(s.id, out)
	return len(p), nil
}
