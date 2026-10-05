package schedule

import (
	"context"
	"errors"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/util"
)

// defaultInFlightGrace is how long an occurrence stays marked in flight before a scheduler takes it
// up again. It is far longer than a fire takes, so only an occurrence whose server stopped between
// the claim and the record of its fire is taken up, and its owner, if still working, has long
// finished.
const defaultInFlightGrace = 2 * time.Minute

// RunByKey returns the id of the run saved under an idempotency key, or the empty string when no
// run is.
type RunByKey func(ctx context.Context, key string) (string, error)

// RunKeyReader is the part of a run store that finds a run by its idempotency key.
type RunKeyReader interface {
	// ByIdempotencyKey returns the run that holds key, or run.ErrNotFound.
	ByIdempotencyKey(ctx context.Context, key string) (*run.Run, error)
}

// KeyedIn returns the RunByKey a server wires, reading the run store.
func KeyedIn(runs RunKeyReader) RunByKey {
	return func(ctx context.Context, key string) (string, error) {
		r, err := runs.ByIdempotencyKey(ctx, key)
		if errors.Is(err, run.ErrNotFound) {
			return "", nil
		}
		if err != nil {
			return "", err
		}
		return r.ID, nil
	}
}

// WithRunByKey lets the sweep of in-flight occurrences find the run an interrupted fire created
// before deciding to fire the occurrence again, so a fire that did create its run is recorded
// rather than repeated. Without it the sweep fires again, and the occurrence's idempotency key
// finds a run that landed instead of starting a second one.
func WithRunByKey(find RunByKey) SchedulerOption {
	return func(s *Scheduler) { s.runByKey = find }
}

// sweepInFlight takes up every occurrence marked in flight past the grace: one whose server
// stopped after the claim and before the fire was recorded, which would otherwise never run. Every
// server's scheduler sweeps, and the store hands each occurrence to one of them.
func (s *Scheduler) sweepInFlight(now time.Time) {
	ctx, cancel := s.settleContext()
	taken, err := s.store.TakeInFlight(ctx, s.inFlightGrace)
	cancel()
	if err != nil {
		if s.ctx.Err() == nil {
			s.log.Error("schedule: take in-flight fires: " + err.Error())
		}
		return
	}
	for _, o := range taken {
		if s.ctx.Err() != nil {
			return
		}
		s.takeUp(o, now)
	}
}

// takeUp accounts for one occurrence a stopped server left in flight. When the occurrence's run
// exists the server stopped after creating it, and only the record is missing, so the fire is
// recorded with that run. When it does not, the occurrence is fired again under the same
// idempotency key, so a run created meanwhile is found rather than duplicated, unless the schedule
// was disabled since the claim, which records why the occurrence did not run. A schedule deleted
// since has nothing to fire. A read that fails, and a fire that a stop cuts short again, leave the
// occurrence marked for a later sweep.
func (s *Scheduler) takeUp(o InFlight, now time.Time) {
	ctx, cancel := s.settleContext()
	defer cancel()
	sc, err := s.store.Get(ctx, o.ScheduleID)
	if errors.Is(err, ErrNotFound) {
		return
	}
	if err != nil {
		s.log.Error("schedule: read a schedule with a fire in flight: "+err.Error(),
			zap.String("schedule_id", o.ScheduleID))
		return
	}
	runID := ""
	if s.runByKey != nil {
		if runID, err = s.runByKey(ctx, run.ScheduleKey(sc.ID, o.Occurrence)); err != nil {
			s.log.Error("schedule: find the run of a fire in flight: "+err.Error(),
				zap.String("schedule_id", sc.ID))
			return
		}
	}
	s.log.Warn("schedule: a fire a stopped server left unrecorded is taken up again",
		zap.String("schedule_id", sc.ID), zap.Time("occurrence", o.Occurrence),
		zap.String("run_id", runID))
	switch {
	case runID != "":
		s.recordFire(sc.ID, o.Occurrence, now, runID, nil)
		return
	case !sc.Enabled:
		reason := "the server stopped while this fire was starting its run, and the schedule was " +
			"disabled before the fire was taken up again, so this occurrence did not run"
		if err := s.store.RecordFire(ctx, sc.ID, now, "", util.Clip(reason, maxFailure)); err != nil {
			s.log.Error("schedule: record fire: "+err.Error(), zap.String("schedule_id", sc.ID))
			return
		}
		s.settleInFlight(sc.ID, o.Occurrence)
		return
	}
	occurrence := sc.Clone()
	at := o.Occurrence
	occurrence.NextRunAt = &at
	id, err := s.fire(s.ctx, occurrence)
	switch {
	case skipped(err):
		s.skip(occurrence, now)
		s.settleInFlight(sc.ID, o.Occurrence)
	case err != nil && id == "" && s.ctx.Err() != nil:
		s.log.Warn("schedule: a stop cut short a fire taken up again, so it stays in flight: "+
			err.Error(), zap.String("schedule_id", sc.ID))
	default:
		s.recordFire(sc.ID, o.Occurrence, now, id, err)
	}
}
