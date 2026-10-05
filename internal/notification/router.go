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
	// TemplateOf returns the template id behind the run.
	TemplateOf(ctx context.Context, r *run.Run) string
}

// LineageFunc adapts a function to a Lineage.
type LineageFunc func(ctx context.Context, r *run.Run) string

// TemplateOf calls f.
func (f LineageFunc) TemplateOf(ctx context.Context, r *run.Run) string { return f(ctx, r) }

// RunGetter reads a run by id. The run store satisfies it.
type RunGetter interface {
	// Get returns the run with the given id.
	Get(ctx context.Context, id string) (*run.Run, error)
}

// SourceLineage returns the Lineage a server wires: a run names what fired it in its source, and
// the template is that source when it is a template, the template a schedule or a trigger fires
// when it is one of those, and the origin run's template for a rerun or a relaunch. Any of the
// three stores may be nil, and a lookup that fails resolves to no template rather than an error,
// since a notification is told about what can be found and the run itself is unaffected.
//
// A run does not record its template directly. Asking the source is what lets an attachment on a
// template reach the runs its schedules and triggers fire, which is the case AWX users rely on.
func SourceLineage(schedules schedule.Store, triggers trigger.Store, runs RunGetter) Lineage {
	return LineageFunc(func(ctx context.Context, r *run.Run) string {
		for range maxLineage {
			if r == nil {
				return ""
			}
			switch r.Source {
			case "template":
				return r.SourceID
			case "schedule":
				if schedules == nil {
					return ""
				}
				sc, err := schedules.Get(ctx, r.SourceID)
				if err != nil {
					return ""
				}
				return sc.TemplateID
			case "trigger":
				if triggers == nil {
					return ""
				}
				tg, err := triggers.Get(ctx, r.SourceID)
				if err != nil {
					return ""
				}
				return tg.TemplateID
			case "rerun", "relaunch":
				if runs == nil || r.SourceID == "" {
					return ""
				}
				origin, err := runs.Get(ctx, r.SourceID)
				if err != nil {
					return ""
				}
				r = origin
			default:
				return ""
			}
		}
		return ""
	})
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

// TemplateOrgFunc returns the organization that owns a template, or the empty string when none does
// or it cannot be read.
type TemplateOrgFunc func(ctx context.Context, templateID string) string

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
	ids, err := rt.attached(ctx, r)
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
// be readable would be counted as told while the others never hear it.
func (rt *Router) Recipients(ctx context.Context, r *run.Run) ([]Recipient, error) {
	ids, err := rt.attached(ctx, r)
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

// attached returns the ids of the targets attached, for the event the run is at, to the template,
// the schedule, the project, or the organization the run came from, each once, in the order found.
// The organizations are the one the run is stamped with and the one that owns its template. An
// object whose attachments cannot be read is passed over and named in the error, beside the ids
// the others gave.
func (rt *Router) attached(ctx context.Context, r *run.Run) ([]string, error) {
	event := EventOf(r)
	if event == "" {
		return nil, nil
	}
	type ref struct {
		// kind and id name one object a run came from.
		kind, id string
	}
	objects := []ref{{KindProject, r.ProjectID}, {KindOrg, r.OrgID}}
	if rt.lineage != nil {
		tpl := rt.lineage.TemplateOf(ctx, r)
		objects = append(objects, ref{KindTemplate, tpl})
		if rt.templateOrg != nil && tpl != "" {
			if owner := rt.templateOrg(ctx, tpl); owner != r.OrgID {
				objects = append(objects, ref{KindOrg, owner})
			}
		}
	}
	if r.Source == "schedule" {
		objects = append(objects, ref{KindSchedule, r.SourceID})
	}
	seen := map[string]bool{}
	var out []string
	var errs []error
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
