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

		hostsA, tasksA, err := rolledSummaries(r.Context(), store, a)
		if err == nil {
			var hostsB []run.HostSummary
			var tasksB []run.TaskSummary
			if hostsB, tasksB, err = rolledSummaries(r.Context(), store, b); err == nil {
				respondJSON(w, log, http.StatusOK,
					run.Compare(a, b, hostsA, hostsB, tasksA, tasksB), wantsPretty(r))
				return
			}
		}
		log.Error("server: compare: " + err.Error())
		respondError(w, log, http.StatusInternalServerError, "could not compare runs")
	}
}

// rolledSummaries returns a run's per host and per task summaries, gathering them from its children
// when the run is a split or a pipeline.
//
// A parent run stores no summaries of its own: the work happened in its shards or steps, and each
// child holds the rows for the hosts it covered. Reading the parent alone returned nothing, so a
// comparison against a split baseline reported every host as new in this run and every task with a
// dash for its baseline, on the newest run in the demo, two clicks from the front page. The run
// detail page never had the bug because it builds its matrix by merging the children's events.
func rolledSummaries(ctx context.Context, store run.Store, r *run.Run) ([]run.HostSummary,
	[]run.TaskSummary, error) {
	hosts, err := store.RunHostSummaries(ctx, r.ID)
	if err != nil {
		return nil, nil, err
	}
	tasks, err := store.RunTaskSummaries(ctx, r.ID)
	if err != nil {
		return nil, nil, err
	}
	children, err := childRuns(ctx, store, r)
	if err != nil {
		return nil, nil, err
	}
	for _, child := range children {
		ch, err := store.RunHostSummaries(ctx, child.ID)
		if err != nil {
			return nil, nil, err
		}
		hosts = append(hosts, ch...)
		ct, err := store.RunTaskSummaries(ctx, child.ID)
		if err != nil {
			return nil, nil, err
		}
		tasks = append(tasks, ct...)
	}
	return hosts, mergeTasks(tasks), nil
}

// childRuns returns the shards or steps of a parent run, and nothing for a plain run.
func childRuns(ctx context.Context, store run.Store, r *run.Run) ([]*run.Run, error) {
	switch r.Kind {
	case run.KindSplit:
		return store.Shards(ctx, r.ID)
	case run.KindPipeline:
		return store.Steps(ctx, r.ID)
	default:
		return nil, nil
	}
}

// mergeTasks folds task rows carrying the same name into one, summing their seconds.
//
// Shards run the same tasks against different hosts, so a three way split reports each task three
// times. Left unmerged the comparison listed one task per shard and its timing read as a third of
// the work. Hosts need no such fold: a host belongs to exactly one shard.
func mergeTasks(tasks []run.TaskSummary) []run.TaskSummary {
	if len(tasks) < 2 {
		return tasks
	}
	order := make([]string, 0, len(tasks))
	byTask := make(map[string]run.TaskSummary, len(tasks))
	for _, t := range tasks {
		existing, seen := byTask[t.Task]
		if !seen {
			order = append(order, t.Task)
			byTask[t.Task] = t
			continue
		}
		existing.Seconds += t.Seconds
		if t.RanAt.After(existing.RanAt) {
			existing.RanAt = t.RanAt
		}
		byTask[t.Task] = existing
	}
	out := make([]run.TaskSummary, 0, len(order))
	for _, name := range order {
		out = append(out, byTask[name])
	}
	return out
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
