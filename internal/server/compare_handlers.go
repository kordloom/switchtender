package server

import (
	"context"
	"errors"
	"net/http"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/run"
)

// runCompareHandler answers what changed between a run and a baseline: host by host, task by
// task, and in wall clock. with= names the baseline run, or "prev" for the most recent earlier
// run fired by the same source, which is the comparison an operator reaches for when a run that
// worked yesterday failed today.
func runCompareHandler(store run.Store, authz *authorizer, log *zap.Logger) http.HandlerFunc {
	if store == nil {
		panic("server: runCompareHandler: Store required")
	}
	return func(w http.ResponseWriter, r *http.Request) {
		a, err := store.Get(r.Context(), r.PathValue("id"))
		if errors.Is(err, run.ErrNotFound) {
			respondError(w, log, http.StatusNotFound, "run not found")
			return
		}
		if err != nil {
			log.Error("server: compare: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not compare runs")
			return
		}
		if authorizeRunAccess(w, r, authz, log, a) {
			return
		}

		withID := r.URL.Query().Get("with")
		if withID == "" || withID == "prev" {
			prev, err := previousRun(r.Context(), store, a)
			if err != nil {
				log.Error("server: compare: " + err.Error())
				respondError(w, log, http.StatusInternalServerError, "could not compare runs")
				return
			}
			if prev == "" {
				respondError(w, log, http.StatusNotFound,
					"no earlier run of the same source to compare against")
				return
			}
			withID = prev
		}
		if withID == a.ID {
			respondError(w, log, http.StatusBadRequest, "a run compared with itself shows nothing")
			return
		}
		b, err := store.Get(r.Context(), withID)
		if errors.Is(err, run.ErrNotFound) {
			respondError(w, log, http.StatusNotFound, "baseline run not found")
			return
		}
		if err != nil {
			log.Error("server: compare: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not compare runs")
			return
		}
		// The baseline is authorized like the run itself: a comparison quotes both runs, and a
		// caller must not read through it what they could not read directly.
		if authorizeRunAccess(w, r, authz, log, b) {
			return
		}

		hostsA, err := store.RunHostSummaries(r.Context(), a.ID)
		if err == nil {
			var hostsB []run.HostSummary
			var tasksA, tasksB []run.TaskSummary
			if hostsB, err = store.RunHostSummaries(r.Context(), b.ID); err == nil {
				if tasksA, err = store.RunTaskSummaries(r.Context(), a.ID); err == nil {
					if tasksB, err = store.RunTaskSummaries(r.Context(), b.ID); err == nil {
						respondJSON(w, log, http.StatusOK,
							run.Compare(a, b, hostsA, hostsB, tasksA, tasksB), wantsPretty(r))
						return
					}
				}
			}
		}
		log.Error("server: compare: " + err.Error())
		respondError(w, log, http.StatusInternalServerError, "could not compare runs")
	}
}

// previousRun returns the most recent run fired by the same source before a, or empty when there
// is none. A run with no source falls back to the newest earlier run of the same playbook and
// tool, scanning a bounded page rather than all of history.
func previousRun(ctx context.Context, store run.Store, a *run.Run) (string, error) {
	if a.SourceID != "" {
		page, err := store.ListPage(ctx, run.ListFilter{
			Source: a.Source, SourceID: a.SourceID, Before: a.CreatedAt,
		}, 1, 0)
		if err != nil {
			return "", err
		}
		if len(page) > 0 {
			return page[0].ID, nil
		}
		return "", nil
	}
	page, err := store.ListPage(ctx, run.ListFilter{Before: a.CreatedAt}, 200, 0)
	if err != nil {
		return "", err
	}
	for _, candidate := range page {
		if candidate.Playbook == a.Playbook && candidate.Tool == a.Tool && candidate.ID != a.ID {
			return candidate.ID, nil
		}
	}
	return "", nil
}
