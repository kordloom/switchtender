package server

import (
	"net/http"
	"sort"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/run"
)

// changeResponse is one change: the intent somebody had, and every run that served it.
type changeResponse struct {
	// Change is the change's name, the value of its label.
	Change string `json:"change"`
	// Outcome is derived from the member runs rather than declared: in_progress while any run is
	// still live, then succeeded, failed, or mixed.
	Outcome string `json:"outcome"`
	// OpenedAt is when the earliest member run was created.
	OpenedAt time.Time `json:"opened_at"`
	// ClosedAt is when the last member run reached a terminal state, zero while any is still live.
	ClosedAt time.Time `json:"closed_at,omitempty"`
	// Actors names everyone who fired a run in this change, ordered.
	Actors []string `json:"actors,omitempty"`
	// Runs are the member runs, newest first. Omitted by the index, which carries a summary per
	// change and would otherwise make one request as large as the whole history. The single change
	// view always has members, because a change with none is a 404 rather than an empty answer.
	Runs []*run.Run `json:"runs,omitempty"`
	// Total is how many member runs there are before the response was capped.
	Total int `json:"total"`
	// Truncated reports that Runs holds fewer than Total.
	Truncated bool `json:"truncated,omitempty"`
	// Withheld counts member runs left out because the caller may not read them. Reported rather
	// than left silent, so a partial change is never mistaken for the whole one: an outcome derived
	// from half a change can say succeeded about work that failed.
	Withheld int `json:"withheld,omitempty"`
}

// Change outcomes, derived from the member runs.
const (
	// changeInProgress is a change with at least one run that has not finished.
	changeInProgress = "in_progress"
	// changeSucceeded is a change whose every run succeeded.
	changeSucceeded = "succeeded"
	// changeFailed is a change whose every run failed.
	changeFailed = "failed"
	// changeMixed is a finished change that both succeeded and failed, which is what a rollback or
	// a fix-forward looks like from outside and the shape a single run can never express.
	changeMixed = "mixed"
)

// changeHandler answers what one change was: the runs that served a single intent, as one thing.
//
// Every tool in this category models the run as the atom, because the run is what an executor
// produces. It is not what a person means. "We rolled out the fix" is four runs, a rollback and a
// fix-forward across three days, and an auditor asks about that, never about run 4471. Until this
// existed the arc had no representation and lived in a ticket somewhere else.
//
// A change is a label rather than a new object on purpose. Runs already carry labels, already
// filter on them, and already put them in the receipt, so a change costs no schema, no migration,
// and no change to what a receipt commits to. What this adds is the reading.
//
// The outcome is computed from the member runs rather than declared. A declared outcome is somebody
// typing what they believe happened; this one cannot disagree with the runs.
func changeHandler(store run.Store, authz *authorizer, log *zap.Logger) http.HandlerFunc {
	if store == nil {
		panic("server: changeHandler: Store required")
	}
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("change")
		if name == "" {
			respondError(w, log, http.StatusBadRequest, "a change name is required")
			return
		}
		filter := run.ListFilter{LabelKey: run.ChangeLabel, LabelValue: name}
		runs, err := store.ListPage(r.Context(), filter, maxListRows+1, 0)
		if err != nil {
			log.Error("server: change: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not read the change")
			return
		}
		// The same read filter every run list applies. A change is a view over runs, so it may not
		// show a caller a run they could not have listed directly.
		keep, _, ferr := derivedReadFilter(r.Context(), authz, store)
		if ferr != nil {
			log.Error("server: read filter: " + ferr.Error())
			respondError(w, log, http.StatusInternalServerError, "could not read the change")
			return
		}
		visible := make([]*run.Run, 0, len(runs))
		withheld := 0
		for _, rn := range runs {
			if keep(rn.ID) {
				visible = append(visible, rn)
				continue
			}
			withheld++
		}
		if len(visible) == 0 {
			respondError(w, log, http.StatusNotFound, "no runs carry this change")
			return
		}
		shown, total := cappedList(visible)
		resp := summarizeChange(name, visible)
		resp.Runs, resp.Total, resp.Truncated = shown, total, len(shown) < total
		resp.Withheld = withheld
		respondJSON(w, log, http.StatusOK, resp, wantsPretty(r))
	}
}

// summarizeChange derives a change's span, actors, and outcome from its member runs.
func summarizeChange(name string, runs []*run.Run) changeResponse {
	out := changeResponse{Change: name, Outcome: changeSucceeded}
	seen := map[string]bool{}
	var anyLive, anyFailed, anyOK bool
	for _, rn := range runs {
		if out.OpenedAt.IsZero() || rn.CreatedAt.Before(out.OpenedAt) {
			out.OpenedAt = rn.CreatedAt
		}
		if !rn.Status.Terminal() {
			anyLive = true
		} else {
			if rn.EndedAt != nil && rn.EndedAt.After(out.ClosedAt) {
				out.ClosedAt = *rn.EndedAt
			}
			if rn.Status == run.StatusSucceeded {
				anyOK = true
			} else {
				anyFailed = true
			}
		}
		if rn.Actor != "" && !seen[rn.Actor] {
			seen[rn.Actor] = true
			out.Actors = append(out.Actors, rn.Actor)
		}
	}
	sort.Strings(out.Actors)
	switch {
	case anyLive:
		// Still open, so it has no close time yet even though some members have finished.
		out.Outcome, out.ClosedAt = changeInProgress, time.Time{}
	case anyOK && anyFailed:
		out.Outcome = changeMixed
	case anyFailed:
		out.Outcome = changeFailed
	}
	return out
}

// changeListResponse is every change the caller can see, newest first.
type changeListResponse struct {
	// Changes are the changes, each summarized without its member runs.
	Changes []changeResponse `json:"changes"`
	// Total is how many changes were found before the response was capped.
	Total int `json:"total"`
	// Truncated reports that Changes holds fewer than Total.
	Truncated bool `json:"truncated,omitempty"`
	// Scanned is how many runs were read to build this, and Partial reports that the scan hit its
	// own cap. A change whose every run is older than that cap is not listed, and saying so is the
	// difference between "these are your changes" and "these are the changes we could see".
	Scanned int `json:"scanned"`
	// Partial reports that the run scan was capped, so older changes may be missing.
	Partial bool `json:"partial,omitempty"`
}

// changesHandler lists the changes the caller can see.
//
// Built by scanning recent runs for the change label rather than by asking the store for distinct
// values, which no store can do. That has a real consequence and the response states it: the scan
// is capped, so a change whose every run predates the cap does not appear. For a browsing surface
// that is the right trade, and the period change register is the instrument for an exhaustive
// answer over a date range.
func changesHandler(store run.Store, authz *authorizer, log *zap.Logger) http.HandlerFunc {
	if store == nil {
		panic("server: changesHandler: Store required")
	}
	return func(w http.ResponseWriter, r *http.Request) {
		const scanCap = maxListRows * 2
		runs, err := store.ListPage(r.Context(), run.ListFilter{LabelKey: run.ChangeLabel},
			scanCap+1, 0)
		if err != nil {
			log.Error("server: changes: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not read changes")
			return
		}
		partial := len(runs) > scanCap
		if partial {
			runs = runs[:scanCap]
		}
		keep, _, ferr := derivedReadFilter(r.Context(), authz, store)
		if ferr != nil {
			log.Error("server: read filter: " + ferr.Error())
			respondError(w, log, http.StatusInternalServerError, "could not read changes")
			return
		}

		// Grouped in the order the runs arrive, which is newest first, so the listing leads with
		// what somebody is most likely looking for.
		members := map[string][]*run.Run{}
		var order []string
		for _, rn := range runs {
			if !keep(rn.ID) {
				continue
			}
			name := rn.Labels[run.ChangeLabel]
			if name == "" {
				continue
			}
			if _, seen := members[name]; !seen {
				order = append(order, name)
			}
			members[name] = append(members[name], rn)
		}

		out := make([]changeResponse, 0, len(order))
		for _, name := range order {
			summary := summarizeChange(name, members[name])
			// The members themselves are left out: this is an index, and carrying every run of
			// every change would make one request as large as the whole history.
			summary.Total = len(members[name])
			out = append(out, summary)
		}
		shown, total := cappedList(out)
		respondJSON(w, log, http.StatusOK, changeListResponse{
			Changes: shown, Total: total, Truncated: len(shown) < total,
			Scanned: len(runs), Partial: partial,
		}, wantsPretty(r))
	}
}
