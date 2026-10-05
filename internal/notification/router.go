package notification

import (
	"context"
	"errors"
	"fmt"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/schedule"
	"github.com/kordloom/switchtender/internal/trigger"
)

// maxLineage bounds how many reruns the lineage follows back to the template a run came from, so a
// chain of reruns cannot turn one notification into an unbounded walk of the run store.
const maxLineage = 8

// Lineage resolves the template a run was launched from, or the empty string when it came from
// none.
type Lineage interface {
	// TemplateOf returns the template id behind the run. A template that can no longer be found,
	// because it or the schedule, trigger, or run that names it no longer exists, is an error
	// wrapping ErrTemplateGone, and a read that fails is an error of its own.
	TemplateOf(ctx context.Context, r *run.Run) (string, error)
}

// LineageFunc adapts a function to a Lineage.
type LineageFunc func(ctx context.Context, r *run.Run) (string, error)

// TemplateOf calls f.
func (f LineageFunc) TemplateOf(ctx context.Context, r *run.Run) (string, error) { return f(ctx, r) }

// RunGetter reads a run by id. The run store satisfies it.
type RunGetter interface {
	// Get returns the run with the given id.
	Get(ctx context.Context, id string) (*run.Run, error)
}

// SourceLineage returns the Lineage a server wires: a run names what fired it in its source, and
// the template is that source when it is a template, the template a schedule or a trigger fires
// when it is one of those, and the origin run's template for a rerun or a relaunch. Any of the
// three stores may be nil, which resolves to no template. A schedule, trigger, or origin run that
// no longer exists is ErrTemplateGone, and a read that fails is returned as the error it is, so the
// event is recorded again once the store answers rather than recorded without the template's
// targets.
//
// A run does not record its template directly. Asking the source is what lets an attachment on a
// template reach the runs its schedules and triggers fire, which is the case AWX users rely on.
func SourceLineage(schedules schedule.Store, triggers trigger.Store, runs RunGetter) Lineage {
	return LineageFunc(func(ctx context.Context, r *run.Run) (string, error) {
		for range maxLineage {
			if r == nil {
				return "", nil
			}
			switch r.Source {
			case "template":
				return r.SourceID, nil
			case "schedule":
				if schedules == nil {
					return "", nil
				}
				sc, err := schedules.Get(ctx, r.SourceID)
				if err != nil {
					return "", lineageErr("schedule", r.SourceID, err,
						errors.Is(err, schedule.ErrNotFound))
				}
				return sc.TemplateID, nil
			case "trigger":
				if triggers == nil {
					return "", nil
				}
				tg, err := triggers.Get(ctx, r.SourceID)
				if err != nil {
					return "", lineageErr("trigger", r.SourceID, err,
						errors.Is(err, trigger.ErrNotFound))
				}
				return tg.TemplateID, nil
			case "rerun", "relaunch":
				if runs == nil || r.SourceID == "" {
					return "", nil
				}
				origin, err := runs.Get(ctx, r.SourceID)
				if err != nil {
					return "", lineageErr("run", r.SourceID, err, errors.Is(err, run.ErrNotFound))
				}
				r = origin
			default:
				return "", nil
			}
		}
		return "", nil
	})
}

// lineageErr describes a failed read of the object named kind and id on the way to a run's
// template, as ErrTemplateGone when the object no longer exists.
func lineageErr(kind, id string, err error, gone bool) error {
	if gone {
		return fmt.Errorf("%w: %s %s no longer exists", ErrTemplateGone, kind, id)
	}
	return fmt.Errorf("read %s %s: %w", kind, id, err)
}

// Router finds the named targets attached to what a run came from, for the event the run is at.
type Router struct {
	// store reads targets and their attachments.
	store Store
	// sealer opens each target's sealed secrets.
	sealer Sealer
	// lineage resolves the template behind a run.
	lineage Lineage
	// templateOrg resolves the organization that owns a template, nil when organizations are not
	// read.
	templateOrg TemplateOrgFunc
	// log records a target that could not be delivered to and why, without its secrets.
	log *zap.Logger
}

// TemplateOrgFunc returns the organization that owns a template, or the empty string when none
// does. A template that no longer exists is an error wrapping ErrTemplateGone, and a read that
// fails is an error of its own.
type TemplateOrgFunc func(ctx context.Context, templateID string) (string, error)

// RouterOption configures a Router.
type RouterOption func(*Router)

// WithTemplateOrgs makes a target attached to an organization hear about the runs of every template
// that organization owns, whoever launched them, the way AWX tells an organization's notification
// templates about every job its templates run. Without it an organization's targets hear only the
// runs stamped with that organization, which a launch by somebody outside it is not.
func WithTemplateOrgs(f TemplateOrgFunc) RouterOption {
	return func(rt *Router) { rt.templateOrg = f }
}

// NewRouter returns a Router. It panics on a nil store, which is a wiring mistake; a nil lineage
// reaches only the project and organization a run names and the schedule that fired it, and a nil
// logger is a no-op.
func NewRouter(store Store, sealer Sealer, lineage Lineage, log *zap.Logger, opts ...RouterOption) *Router {
	if store == nil {
		panic("notification: Store required")
	}
	if log == nil {
		log = zap.NewNop()
	}
	rt := &Router{store: store, sealer: sealer, lineage: lineage, log: log}
	for _, opt := range opts {
		opt(rt)
	}
	return rt
}

// Targets returns the channel configuration of every target attached, for the event the run is at,
// to the template, the schedule, the project, or the organization the run came from. A target
// attached at more than one of them is returned once, and the list is bounded the same way a run's
// own list is.
func (rt *Router) Targets(ctx context.Context, r *run.Run) []run.NotifyTarget {
	ids, err := rt.attached(ctx, r, EventOf(r))
	if err != nil {
		rt.log.Error("notification: read attachments: "+err.Error(), zap.String("run_id", r.ID))
	}
	var out []run.NotifyTarget
	for _, id := range ids {
		if t, ok := rt.target(ctx, r.ID, id); ok {
			out = append(out, t)
		}
	}
	if len(out) > run.MaxNotifyTargets {
		rt.log.Warn("notification: attached targets truncated to the limit",
			zap.String("run_id", r.ID), zap.Int("attached", len(out)),
			zap.Int("delivered", run.MaxNotifyTargets))
		out = out[:run.MaxNotifyTargets]
	}
	return out
}

// Recipients returns every target attached for the event the run is at, the same set Targets
// finds, without opening a secret: what an event is recorded for, to be opened only when it is
// delivered. A target still waiting for its secret is returned marked to be skipped, so the run's
// record says why that target was not told rather than leaving it out. A store that cannot be read
// is an error rather than a shorter list: an event recorded for only the targets that happened to
// be readable would be counted as told while the others never hear it. That includes the template
// the run came from and the organization that owns it: a read of either that fails is an error,
// while a template that no longer exists has nothing left to find, so its targets are passed over
// and the router logs which run it was.
func (rt *Router) Recipients(ctx context.Context, r *run.Run) ([]Recipient, error) {
	return rt.recipientsFor(ctx, r, EventOf(r))
}

// recipientsFor is Recipients for the targets attached for event, which need not be the event the
// run is at now.
func (rt *Router) recipientsFor(ctx context.Context, r *run.Run, event string) ([]Recipient, error) {
	ids, err := rt.attached(ctx, r, event)
	if err != nil {
		return nil, err
	}
	var out []Recipient
	for _, id := range ids {
		n, err := rt.store.Get(ctx, id)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read target %s: %w", id, err)
		}
		rc := Recipient{NotificationID: n.ID, Name: n.Name, Kind: n.Kind}
		if n.NeedsSecret {
			rc.Skip = "not sent: the target is waiting for its secret to be entered"
		}
		out = append(out, rc)
	}
	if len(out) > run.MaxNotifyTargets {
		rt.log.Warn("notification: attached targets truncated to the limit",
			zap.String("run_id", r.ID), zap.Int("attached", len(out)),
			zap.Int("recorded", run.MaxNotifyTargets))
		out = out[:run.MaxNotifyTargets]
	}
	return out, nil
}

// attached returns the ids of the targets attached, for event, to the template, the schedule, the
// project, or the organization the run came from, each once, in the order found. The organizations
// are the one the run is stamped with and the one that owns its template. An object whose
// attachments cannot be read, and a template or owner that cannot be read, is passed over and named
// in the error, beside the ids the others gave. A template that no longer exists is passed over
// without an error and logged, since there is nothing left to read again.
func (rt *Router) attached(ctx context.Context, r *run.Run, event string) ([]string, error) {
	if event == "" {
		return nil, nil
	}
	type ref struct {
		// kind and id name one object a run came from.
		kind, id string
	}
	objects := []ref{{KindProject, r.ProjectID}, {KindOrg, r.OrgID}}
	var errs []error
	if rt.lineage != nil {
		tpl, err := rt.lineage.TemplateOf(ctx, r)
		if err = rt.passOver(r, err); err != nil {
			errs = append(errs, fmt.Errorf("find the template run %s came from: %w", r.ID, err))
		}
		objects = append(objects, ref{KindTemplate, tpl})
		if rt.templateOrg != nil && tpl != "" {
			owner, err := rt.templateOrg(ctx, tpl)
			if err = rt.passOver(r, err); err != nil {
				errs = append(errs, fmt.Errorf("find the organization that owns template %s: %w",
					tpl, err))
			}
			if owner != r.OrgID {
				objects = append(objects, ref{KindOrg, owner})
			}
		}
	}
	if r.Source == "schedule" {
		objects = append(objects, ref{KindSchedule, r.SourceID})
	}
	seen := map[string]bool{}
	var out []string
	for _, o := range objects {
		if o.id == "" {
			continue
		}
		attached, err := rt.store.AttachedTo(ctx, o.kind, o.id)
		if err != nil {
			errs = append(errs, fmt.Errorf("read the attachments on %s %s: %w", o.kind, o.id, err))
			continue
		}
		for _, a := range attached {
			if !hears(a, event) || seen[a.NotificationID] {
				continue
			}
			seen[a.NotificationID] = true
			out = append(out, a.NotificationID)
		}
	}
	return out, errors.Join(errs...)
}

// passOver logs a template that no longer exists, whose targets routing passes over, and returns
// nil for it, returning any other error unchanged.
func (rt *Router) passOver(r *run.Run, err error) error {
	if !errors.Is(err, ErrTemplateGone) {
		return err
	}
	rt.log.Warn("notification: targets passed over: "+err.Error(), zap.String("run_id", r.ID))
	return nil
}

// target opens one notification target for delivery, logging and skipping one that cannot be.
func (rt *Router) target(ctx context.Context, runID, id string) (run.NotifyTarget, bool) {
	n, err := rt.store.Get(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return run.NotifyTarget{}, false
	}
	if err != nil {
		rt.log.Error("notification: read target: "+err.Error(),
			zap.String("run_id", runID), zap.String("notification_id", id))
		return run.NotifyTarget{}, false
	}
	t, err := n.Target(rt.sealer)
	if err != nil {
		rt.log.Warn("notification: target skipped: "+err.Error(),
			zap.String("run_id", runID), zap.String("notification_id", id))
		return run.NotifyTarget{}, false
	}
	return t, true
}
