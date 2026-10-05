package server

import (
	"context"
	"net/http"
	"slices"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/attention"
	"github.com/kordloom/switchtender/internal/grant"
	"github.com/kordloom/switchtender/internal/run"
)

// WithAttention serves the dashboard's Needs attention view from src and adds what has been waiting
// past its alert threshold to the doctor. A server without one answers the view with nothing
// waiting.
func WithAttention(src *attention.Source) Option {
	return func(s *Server) { s.attention = src }
}

// attentionThresholds are the install-wide thresholds the view measures against, in seconds, so a
// reader can tell what "past its threshold" means. A queue or template override shows on the item
// it applies to, as that item's own alert threshold. Zero means the alert is off.
type attentionThresholds struct {
	// BlockedAfterSeconds is how long a run waits before it shows as blocked.
	BlockedAfterSeconds int64 `json:"blocked_after_seconds"`
	// AlertNoWorkerSeconds is how long a run waits with no worker before it alerts.
	AlertNoWorkerSeconds int64 `json:"alert_no_worker_seconds"`
	// AlertBlockedSeconds is how long a run stays blocked before it alerts.
	AlertBlockedSeconds int64 `json:"alert_blocked_seconds"`
	// AlertWorkerLostSeconds is how long after a lost worker last reported its unreclaimed run alerts.
	AlertWorkerLostSeconds int64 `json:"alert_worker_lost_seconds"`
	// AlertApprovalSeconds is how long an approval waits before it alerts.
	AlertApprovalSeconds int64 `json:"alert_approval_seconds"`
}

// attentionResponse is the GET /v1/attention envelope.
type attentionResponse struct {
	// Counts are how many items each main blocker stops, among the items the caller may see.
	Counts attention.Counts `json:"counts"`
	// Items are the items, filtered to one main blocker when the request names one.
	Items []attention.Item `json:"items"`
	// Count is how many items were returned.
	Count int `json:"count"`
	// Total is how many items the caller may see, before the blocker filter.
	Total int `json:"total"`
	// Blocker is the main blocker the items were filtered to, empty for all of them.
	Blocker string `json:"blocker,omitempty"`
	// Thresholds are the install-wide thresholds.
	Thresholds attentionThresholds `json:"thresholds"`
	// GeneratedAt is when the view was evaluated, on the store's clock.
	GeneratedAt time.Time `json:"generated_at"`
}

// attentionHandler answers what is stopping the work the caller may see: the four counts, and each
// item with its main blocker, its other conditions, how long it has been in its current blocker,
// who can act, and what happens next. ?blocker= narrows the items to one main blocker, and the
// counts always cover them all. Reading it is a viewer read, the same as the run list it is drawn
// from.
func attentionHandler(src *attention.Source, authz *authorizer, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		blocker := attention.Blocker(r.URL.Query().Get("blocker"))
		if blocker != "" && !slices.Contains(attention.Order, blocker) {
			respondError(w, log, http.StatusBadRequest, "blocker must be worker_lost, no_worker, "+
				"approval_needed, or blocked")
			return
		}
		limits := src.Limits("", "", "")
		resp := attentionResponse{
			Items: []attention.Item{}, Blocker: string(blocker),
			Thresholds: attentionThresholds{
				BlockedAfterSeconds:    int64(limits.BlockedAfter / time.Second),
				AlertNoWorkerSeconds:   int64(limits.AlertNoWorker / time.Second),
				AlertBlockedSeconds:    int64(limits.AlertBlocked / time.Second),
				AlertWorkerLostSeconds: int64(limits.AlertWorkerLost / time.Second),
				AlertApprovalSeconds:   int64(limits.AlertApproval / time.Second),
			},
		}
		snap, err := src.Snapshot(r.Context())
		if err != nil {
			log.Error("server: evaluate attention: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not read what needs attention")
			return
		}
		visible, err := visibleAttention(r.Context(), authz, snap.Items)
		if err != nil {
			log.Error("server: filter attention: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not read what needs attention")
			return
		}
		resp.Counts = attention.CountItems(visible)
		resp.Total = len(visible)
		resp.GeneratedAt = snap.GeneratedAt
		for _, it := range visible {
			if blocker == "" || it.Blocker == blocker {
				resp.Items = append(resp.Items, it)
			}
		}
		resp.Count = len(resp.Items)
		respondJSON(w, log, http.StatusOK, resp, wantsPretty(r))
	}
}

// visibleAttention keeps the items the caller may see: a run item when the caller may read the run,
// under the same rule the run list applies, and a schedule item when the caller may use the
// schedule, under the rule the schedule list applies. A caller who could not open the run or the
// schedule must not learn from this view that it exists or what is stopping it.
func visibleAttention(ctx context.Context, authz *authorizer,
	items []attention.Item) ([]attention.Item, error) {
	var runs []*run.Run
	for _, it := range items {
		if it.Schedule == nil && it.Run != nil {
			runs = append(runs, it.Run)
		}
	}
	readable, err := readableRuns(ctx, authz, runs)
	if err != nil {
		return nil, err
	}
	canRead := map[string]bool{}
	for _, rn := range readable {
		canRead[rn.ID] = true
	}
	out := []attention.Item{}
	for _, it := range items {
		switch {
		case it.Schedule != nil:
			if authz.authorizeSchedule(ctx, grant.AccessUse, it.Schedule) == nil {
				out = append(out, it)
			}
		case it.Run != nil && canRead[it.Run.ID]:
			out = append(out, it)
		}
	}
	return out, nil
}

// attentionCheck is the doctor check that reports work needing attention past its alert threshold.
func (s *Server) attentionCheck(ctx context.Context) (doctorCheckResult, error) {
	found, err := s.attentionFindings(ctx)
	return doctorCheckResult{Findings: found}, err
}

// attentionFindings reports, for the doctor, everything that has needed attention past its alert
// threshold. The doctor is an admin read, so nothing is filtered. A server without a source reports
// nothing.
func (s *Server) attentionFindings(ctx context.Context) ([]doctorFinding, error) {
	if s.attention == nil {
		return nil, nil
	}
	snap, err := s.attention.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	var out []doctorFinding
	for _, it := range snap.Items {
		if !it.Alerting {
			continue
		}
		note := it.Note(snap.GeneratedAt)
		finding := doctorFinding{
			Severity: "warning", ObjectType: "run", ObjectID: it.RunID, ObjectName: it.Name,
			Problem: note.Summary + " " + it.Main.WhoCanAct,
			FixPath: "/ui/?attention=" + string(it.Blocker),
		}
		if it.Schedule != nil {
			finding.ObjectType, finding.ObjectID = "schedule", it.ScheduleID
		}
		out = append(out, finding)
	}
	return out, nil
}
