package dispatch

import (
	"context"

	"go.uber.org/zap"

	"github.com/dcadolph/yardmaster/internal/run"
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
}

// Write appends p to the run's log and publishes it. A store failure is logged but reported as a
// successful write so a transient logging fault does not tear down the running process.
func (s *logSink) Write(p []byte) (int, error) {
	if err := s.store.AppendLog(context.Background(), s.id, p); err != nil {
		s.log.Error("dispatch: append log: "+err.Error(), zap.String("run_id", s.id))
	}
	s.publisher.PublishLog(s.id, p)
	return len(p), nil
}
