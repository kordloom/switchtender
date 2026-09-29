// Package schedule holds recurring run schedules and the logic that fires them on a cron cadence.
// A schedule fires a single run, a split, or a pipeline through the same submit paths a client uses,
// so scheduled work is indistinguishable from manual work once it lands.
package schedule

import (
	"errors"
	"fmt"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/kordloom/switchtender/internal/idgen"
	"github.com/kordloom/switchtender/internal/run"
)

// NewID returns a random schedule identifier prefixed with "sch_".
func NewID() string {
	return idgen.New("sch_", 8)
}

var (
	// ErrBadCron is returned when a cron expression cannot be parsed.
	ErrBadCron = errors.New("bad cron")
	// ErrNotFound is returned when a schedule does not exist.
	ErrNotFound = errors.New("schedule not found")
	// ErrNoTarget is returned when a schedule names neither a playbook nor pipeline steps.
	ErrNoTarget = errors.New("no playbook or steps")
)

// Schedule is a recurring run definition. It fires a pipeline when Steps is set, a split when Shards
// is two or more, and otherwise a single run.
type Schedule struct {
	// ID is the unique schedule identifier.
	ID string `json:"id"`
	// Name identifies the schedule.
	Name string `json:"name"`
	// Cron is the cron expression that sets the cadence.
	Cron string `json:"cron"`
	// Playbook is the playbook to run for a single or split schedule.
	Playbook string `json:"playbook,omitempty"`
	// Inventory is the inventory to target.
	Inventory string `json:"inventory,omitempty"`
	// Shards, when two or more, fires a split across that many inventory slices.
	Shards int `json:"shards,omitempty"`
	// Steps, when set, fires a pipeline of these steps.
	Steps []run.PipelineStep `json:"steps,omitempty"`
	// TemplateID, when set, fires a stored job template instead of the inline fields.
	TemplateID string `json:"template_id,omitempty"`
	// Enabled reports whether the schedule fires.
	Enabled bool `json:"enabled"`
	// CreatedAt is when the schedule was created.
	CreatedAt time.Time `json:"created_at"`
	// NextRunAt is when the schedule fires next.
	NextRunAt *time.Time `json:"next_run_at,omitempty"`
	// LastRunAt is when the schedule last fired.
	LastRunAt *time.Time `json:"last_run_at,omitempty"`
	// LastRunID is the run created by the most recent fire.
	LastRunID string `json:"last_run_id,omitempty"`
}

// Clone returns a deep copy so callers cannot mutate stored state through shared pointers.
func (s *Schedule) Clone() *Schedule {
	if s == nil {
		return nil
	}
	out := *s
	if s.NextRunAt != nil {
		t := *s.NextRunAt
		out.NextRunAt = &t
	}
	if s.LastRunAt != nil {
		t := *s.LastRunAt
		out.LastRunAt = &t
	}
	if len(s.Steps) > 0 {
		out.Steps = make([]run.PipelineStep, len(s.Steps))
		copy(out.Steps, s.Steps)
	}
	return &out
}

// Validate reports whether the schedule has a parseable cron and a target to run.
func (s *Schedule) Validate() error {
	if _, err := NextFire(s.Cron, time.Now()); err != nil {
		return err
	}
	if s.Playbook == "" && len(s.Steps) == 0 && s.TemplateID == "" {
		return ErrNoTarget
	}
	return nil
}

// NextFire returns the next time the cron expression fires after the given time.
func NextFire(spec string, after time.Time) (time.Time, error) {
	sched, err := cron.ParseStandard(spec)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: %w", ErrBadCron, err)
	}
	return sched.Next(after), nil
}
