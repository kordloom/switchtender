package server

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/kordloom/switchtender/internal/event"
	"github.com/kordloom/switchtender/internal/run"
	"go.uber.org/zap"
)

// listRunsResponse wraps a run list. The envelope leaves room for pagination fields later.
type listRunsResponse struct {
	// Runs is the ordered list of runs.
	Runs []*run.Run `json:"runs"`
	// Count is the number of runs returned.
	Count int `json:"count"`
	// Summary is the run totals across every page, for the summary cards.
	Summary runSummary `json:"summary"`
	// HasMore reports whether another page follows this one.
	HasMore bool `json:"has_more"`
}

// runSummary is the per-status rollup of all top-level runs, shown as cards above the list.
type runSummary struct {
	// Total is the number of top-level runs.
	Total int `json:"total"`
	// Succeeded is how many finished successfully.
	Succeeded int `json:"succeeded"`
	// Failed is how many failed.
	Failed int `json:"failed"`
	// Active is how many are running or pending.
	Active int `json:"active"`
}

// summarize folds status counts into the summary the runs view shows.
func summarize(counts map[run.Status]int) runSummary {
	s := runSummary{}
	for status, n := range counts {
		s.Total += n
		switch status {
		case run.StatusSucceeded:
			s.Succeeded += n
		case run.StatusFailed:
			s.Failed += n
		case run.StatusRunning, run.StatusPending:
			s.Active += n
		}
	}
	return s
}

// eventsResponse wraps a run's structured events.
type eventsResponse struct {
	// Events is the ordered list of events.
	Events []event.Event `json:"events"`
	// Count is the number of events returned.
	Count int `json:"count"`
	// NextAfter is the sequence cursor to pass back as ?after= to page the events that
	// follow this batch. It is the last event's Seq, or the requested after when empty.
	NextAfter int64 `json:"next_after"`
}

// shardsResponse wraps a parent run's shard runs.
type shardsResponse struct {
	// Shards is the ordered list of shard runs.
	Shards []*run.Run `json:"shards"`
	// Count is the number of shards returned.
	Count int `json:"count"`
}

// stepsResponse wraps a pipeline run's step runs.
type stepsResponse struct {
	// Steps is the ordered list of step runs.
	Steps []*run.Run `json:"steps"`
	// Count is the number of steps returned.
	Count int `json:"count"`
}

// fieldedTokens splits a query into terms, keeping a double-quoted value together with the key it
// belongs to. Splitting on spaces alone made every multi-word value unaddressable: an approval
// rule is named in prose, so held_by:"prod terraform destroy" fell apart into free text and the
// deep link from a policy row could never say which rule it meant.
func fieldedTokens(q string) []string {
	var tokens []string
	var cur strings.Builder
	inQuote := false
	for _, r := range q {
		switch {
		case r == '"':
			inQuote = !inQuote
		case r == ' ' && !inQuote:
			if cur.Len() > 0 {
				tokens = append(tokens, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		tokens = append(tokens, cur.String())
	}
	return tokens
}

// parseFieldedQuery splits a search string into fielded terms and free text. status:, tool:,
// source:, actor:, host:, worker:, and held_by: fill their filters, label:key=value matches a run
// label, and everything else stays free text. A value holding spaces is double-quoted. Explicit
// query parameters win over fielded terms.
func parseFieldedQuery(q string, filter *run.ListFilter) {
	var free []string
	for _, token := range fieldedTokens(q) {
		key, value, ok := strings.Cut(token, ":")
		if !ok || value == "" {
			free = append(free, token)
			continue
		}
		switch strings.ToLower(key) {
		case "status":
			if filter.Status == "" {
				filter.Status = strings.ToLower(value)
			}
		case "tool":
			if filter.Tool == "" {
				filter.Tool = run.NormalizeTool(value)
			}
		case "source":
			filter.Source = strings.ToLower(value)
		case "actor":
			filter.Actor = value
		case "from":
			// The object that fired the run: a template or schedule id.
			filter.SourceID = value
		case "host":
			filter.Host = value
		case "worker":
			// The executor that claimed the run, so a worker's row opens the work it did.
			filter.ClaimedBy = value
		case "held_by":
			// The approval rule that held the run. The stored field is historical, so pair it with
			// status:pending_approval to see only what the rule is holding now.
			filter.HeldBy = value
		case "label":
			if lk, lv, ok := strings.Cut(value, "="); ok && lk != "" {
				filter.LabelKey, filter.LabelValue = lk, lv
			} else {
				free = append(free, token)
			}
		default:
			free = append(free, token)
		}
	}
	filter.Query = strings.Join(free, " ")
}

// defaultRunsPage is the page size when a runs listing names none, and maxRunsPage is the largest
// page a caller can request, so one request can never materialize the whole run history.
const (
	defaultRunsPage = 200
	maxRunsPage     = 1000
)

// listRunsHandler returns a page of runs newest first, bounded even when no limit is given.
//
// The page is filtered to what the caller may read. Fetching one run already checked that, but the
// list did not, so under strict grants a caller who was refused a run by id could still read it, and
// everything on it, by listing. A run carries extra vars, a command line, and credential ids, so the
// list leaked more than the object it was listing.
func listRunsHandler(store run.Store, authz *authorizer, log *zap.Logger) http.HandlerFunc {
	if store == nil {
		panic("server: listRunsHandler: Store required")
	}
	return func(w http.ResponseWriter, r *http.Request) {
		limit := queryInt(r, "limit")
		if limit <= 0 {
			// An explicit zero asks for everything, capped at the hard page bound so the promise
			// stays honest; an absent limit gets the smaller default.
			limit = defaultRunsPage
			if r.URL.Query().Get("limit") != "" {
				limit = maxRunsPage
			}
		}
		limit = min(limit, maxRunsPage)
		offset := queryInt(r, "offset")
		filter := run.ListFilter{
			// Normalized like the fielded status: term; a mixed-case value silently matched nothing.
			Status:      strings.ToLower(r.URL.Query().Get("status")),
			OldestFirst: r.URL.Query().Get("order") == "oldest",
		}
		parseFieldedQuery(r.URL.Query().Get("q"), &filter)
		if tool := r.URL.Query().Get("tool"); tool != "" {
			filter.Tool = run.NormalizeTool(tool)
		}
		if after, err := time.Parse(time.RFC3339, r.URL.Query().Get("after")); err == nil {
			filter.After = after
		}
		if before, err := time.Parse(time.RFC3339, r.URL.Query().Get("before")); err == nil {
			filter.Before = before
		}
		runs, err := store.ListPage(r.Context(), filter, limit, offset)
		if err != nil {
			log.Error("server: list runs: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not list runs")
			return
		}
		// Whether another page follows is decided by what the store returned, before the read filter
		// thins it. Computing it from the trimmed page reported no more whenever the filter dropped a
		// row from a full page, so later readable runs never paged in.
		storeFullPage := len(runs) == limit
		runs, err = readableRuns(r.Context(), authz, runs)
		if err != nil {
			log.Error("server: filter runs: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not list runs")
			return
		}
		counts, err := store.RunStatusCounts(r.Context())
		if err != nil {
			log.Error("server: run status counts: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not list runs")
			return
		}
		// The status totals are an install-wide aggregate, so they are withheld from a caller who may
		// read no runs at all, the same aggregate-withholding the drift and task views do. Otherwise
		// a strict-grants viewer refused every run by name still learned how much activity the install
		// had. A visible run on this page already proves the caller reads something, so the scan only
		// runs when the page is empty of readable runs.
		anyReadable := len(runs) > 0
		if !anyReadable {
			_, ar, ferr := derivedReadFilter(r.Context(), authz, store)
			if ferr != nil {
				log.Error("server: read filter: " + ferr.Error())
				respondError(w, log, http.StatusInternalServerError, "could not list runs")
				return
			}
			anyReadable = ar
		}
		summary := runSummary{}
		if anyReadable {
			summary = summarize(counts)
		}
		respondJSON(w, log, http.StatusOK, listRunsResponse{
			Runs:    maskRuns(runs),
			Count:   len(runs),
			Summary: summary,
			HasMore: storeFullPage,
		}, wantsPretty(r))
	}
}

// getRunHandler returns a single run by id.
func getRunHandler(store run.Store, authz *authorizer, log *zap.Logger) http.HandlerFunc {
	if store == nil {
		panic("server: getRunHandler: Store required")
	}
	return func(w http.ResponseWriter, r *http.Request) {
		got, err := store.Get(r.Context(), r.PathValue("id"))
		if err != nil {
			if errors.Is(err, run.ErrNotFound) {
				respondError(w, log, http.StatusNotFound, "run not found")
				return
			}
			log.Error("server: get run: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not get run")
			return
		}
		if authorizeRunAccess(w, r, authz, log, got) {
			return
		}
		// Grade the run's blast radius so an approver sees the risk without opening the log.
		risk := run.AssessRisk(got)
		got.Risk = &risk
		respondJSON(w, log, http.StatusOK, maskRun(got), wantsPretty(r))
	}
}

// runShardsHandler returns the shard runs of a parent run.
func runShardsHandler(store run.Store, authz *authorizer, log *zap.Logger) http.HandlerFunc {
	if store == nil {
		panic("server: runShardsHandler: Store required")
	}
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		rn, err := store.Get(r.Context(), id)
		if errors.Is(err, run.ErrNotFound) {
			respondError(w, log, http.StatusNotFound, "run not found")
			return
		}
		if err != nil {
			log.Error("server: list shards: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not list shards")
			return
		}
		if authorizeRunAccess(w, r, authz, log, rn) {
			return
		}
		shards, err := store.Shards(r.Context(), id)
		if err != nil {
			log.Error("server: list shards: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not list shards")
			return
		}
		respondJSON(w, log, http.StatusOK,
			shardsResponse{Shards: maskRuns(shards), Count: len(shards)}, wantsPretty(r))
	}
}

// runStepsHandler returns the step runs of a pipeline run.
func runStepsHandler(store run.Store, authz *authorizer, log *zap.Logger) http.HandlerFunc {
	if store == nil {
		panic("server: runStepsHandler: Store required")
	}
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		rn, err := store.Get(r.Context(), id)
		if errors.Is(err, run.ErrNotFound) {
			respondError(w, log, http.StatusNotFound, "run not found")
			return
		}
		if err != nil {
			log.Error("server: list steps: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not list steps")
			return
		}
		if authorizeRunAccess(w, r, authz, log, rn) {
			return
		}
		steps, err := store.Steps(r.Context(), id)
		if err != nil {
			log.Error("server: list steps: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not list steps")
			return
		}
		respondJSON(w, log, http.StatusOK,
			stepsResponse{Steps: maskRuns(steps), Count: len(steps)}, wantsPretty(r))
	}
}

// runLogsHandler returns a run's captured output as plain text.
func runLogsHandler(store run.Store, authz *authorizer, log *zap.Logger) http.HandlerFunc {
	if store == nil {
		panic("server: runLogsHandler: Store required")
	}
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		rn, gerr := store.Get(r.Context(), id)
		if errors.Is(gerr, run.ErrNotFound) {
			respondError(w, log, http.StatusNotFound, "run not found")
			return
		}
		if gerr != nil {
			log.Error("server: get run log: " + gerr.Error())
			respondError(w, log, http.StatusInternalServerError, "could not get run log")
			return
		}
		if authorizeRunAccess(w, r, authz, log, rn) {
			return
		}
		// The log streams to the client in chunk pages, so a multi-gigabyte log download never
		// materializes in the control plane's memory.
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		var (
			after     int64
			atLineEnd = true
		)
		for {
			chunks, err := store.LogAfter(r.Context(), id, after, streamBatch)
			if err != nil {
				// The status line is already out, so a reader would otherwise receive a short log
				// that reads like the whole one. The log has a recorded digest to check a copy
				// against, which the event export does not, but the download should still say so
				// itself. The marker takes a line of its own so it is never read as part of
				// whatever the playbook was printing when the store went away.
				log.Error("server: get run log: " + err.Error())
				if !atLineEnd {
					if _, werr := w.Write([]byte("\n")); werr != nil {
						return
					}
				}
				writeExportSentinel(w, log, "the log store failed part way through this download")
				return
			}
			for _, c := range chunks {
				after = c.Seq
				if _, err := w.Write(c.Data); err != nil {
					log.Error("server: write run log: " + err.Error())
					return
				}
				if len(c.Data) > 0 {
					atLineEnd = c.Data[len(c.Data)-1] == '\n'
				}
			}
			if len(chunks) < streamBatch {
				return
			}
		}
	}
}
