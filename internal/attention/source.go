package attention

import (
	"context"
	"fmt"
	"time"

	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/schedule"
	"github.com/kordloom/switchtender/internal/user"
)

// Source gathers what an evaluation reads from the stores, so the dashboard, the doctor, and the
// alert monitor read the same state the same way.
type Source struct {
	// Runs is the run store.
	Runs run.Store
	// Presence holds the workers' reports, nil when none are recorded, in which case every queued
	// run reads as having no worker.
	Presence Store
	// Schedules is the schedule store, nil when schedules are off.
	Schedules schedule.Store
	// Users resolves the account behind a run, nil when there are no accounts.
	Users user.Store
	// Config holds the thresholds, nil for the built-in ones.
	Config *Config
	// Timing is the dispatcher's lease timing.
	Timing Timing
}

// Snapshot evaluates everything that needs attention now, on the store's clock.
func (s *Source) Snapshot(ctx context.Context) (Snapshot, error) {
	if s == nil || s.Runs == nil {
		return Snapshot{Items: []Item{}}, nil
	}
	now, err := s.Runs.Now(ctx)
	if err != nil {
		return Snapshot{}, fmt.Errorf("%w: read the store clock: %w", ErrStore, err)
	}
	runs, err := s.Runs.NonTerminal(ctx)
	if err != nil {
		return Snapshot{}, fmt.Errorf("%w: list unfinished runs: %w", ErrStore, err)
	}
	in := Input{Now: now, Runs: runs, Config: s.Config, Timing: s.Timing}
	if qt, ok := s.Runs.(run.QueueTimes); ok {
		if in.Queued, err = qt.QueuedTimes(ctx); err != nil {
			return Snapshot{}, fmt.Errorf("%w: read queue times: %w", ErrStore, err)
		}
	}
	if s.Presence != nil {
		if in.Workers, err = s.Presence.Workers(ctx, now.Add(-WorkerRetention)); err != nil {
			return Snapshot{}, fmt.Errorf("%w: read worker reports: %w", ErrStore, err)
		}
	}
	if s.Schedules != nil {
		if in.Schedules, err = s.Schedules.List(ctx); err != nil {
			return Snapshot{}, fmt.Errorf("%w: list schedules: %w", ErrStore, err)
		}
	}
	if s.Users != nil {
		names := map[string]string{}
		in.Accounts = func(id string) string {
			if name, ok := names[id]; ok {
				return name
			}
			name := ""
			if u, uerr := s.Users.Get(ctx, id); uerr == nil {
				name = u.Username
			}
			names[id] = name
			return name
		}
	}
	in.NextSteps = func(parent, step *run.Run) ([]string, []string, bool) {
		state, serr := outcome.StepStateOf(ctx, s.Runs, parent, step)
		if serr != nil {
			return nil, nil, false
		}
		return state.OnApprove, state.OnDeny, true
	}
	return Evaluate(in), nil
}

// Limits returns the thresholds that apply to an item owned by orgID on queue from templateID.
func (s *Source) Limits(orgID, queue, templateID string) Limits {
	if s == nil {
		return (*Config)(nil).Limits(orgID, queue, templateID, 0)
	}
	return s.Config.Limits(orgID, queue, templateID, s.Timing.LeaseTTL)
}

// DefaultTiming returns timing for a lease of leaseTTL renewed every heartbeat and swept every
// sweep, with workers reporting every report. A worker counts as lost once three renewals in a row
// have not arrived, with half an interval of slack for a slow write, and as connected while its
// last report is within three report intervals.
func DefaultTiming(leaseTTL, heartbeat, sweep, report time.Duration) Timing {
	return Timing{
		LeaseTTL: leaseTTL, SweepInterval: sweep, LostAfter: 3*heartbeat + heartbeat/2,
		Fresh: 3 * report,
	}
}
