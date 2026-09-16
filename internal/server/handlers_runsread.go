package server

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/event"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/user"
	"io"
	"os"
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
	// NextOffset is where the next page starts in the store's own ordering. It advances by the
	// page the store returned, before the read filter thinned it: a strict-grants caller advancing
	// by the rows it could see re-read the rows it could not, and Load more repeated the page.
	NextOffset int `json:"next_offset"`
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
	// AwaitingApproval is how many are held at the approval gate, the number the overview leads
	// with: it is the governance story in one figure.
	AwaitingApproval int `json:"awaiting_approval"`
	// Scope says what these numbers cover: "install" for every run on the install, "visible" when
	// grants restrict this caller and the counts cover only the runs on this page. A caller who is
	// shown a subset must not read it as a total, and the interface labels the cards from this.
	Scope string `json:"scope,omitempty"`
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
		case run.StatusPendingApproval:
			s.AwaitingApproval += n
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
// source:, actor:, host:, task:, worker:, and held_by: fill their filters, label:key=value matches a run
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
		case "task":
			// The task a run ran, resolved through its stored task summaries. Task names are not
			// on the run row, so free text cannot reach them: the Task trends page linked a plain
			// search and got nothing back on every row.
			filter.Task = value
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
		nextOffset := offset + len(runs)
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
		// The status totals are an install-wide aggregate, so they go only to a caller grants place
		// no read restriction on.
		//
		// Withholding them from a caller who can read NOTHING was the old rule, and it left the
		// leak it was written to close: a viewer restricted to one organization could read some runs,
		// which satisfied the test, and then received counts covering every organization on the
		// install. One number is enough to publish another tenant's volume.
		//
		// The probe is the same one derivedReadFilter opens with, so this costs nothing: it asks
		// whether grants restrict this caller at all, rather than walking rows through a filter,
		// which at a thousand rows and ten thousand grants is the cost the filter's own comment
		// warns about.
		unrestricted, ferr := unrestrictedReader(r.Context(), authz)
		if ferr != nil {
			log.Error("server: read filter: " + ferr.Error())
			respondError(w, log, http.StatusInternalServerError, "could not list runs")
			return
		}
		summary := runSummary{}
		switch {
		case unrestricted:
			summary = summarize(counts)
			summary.Scope = "install"
		case len(runs) > 0:
			// Restricted, but reading something. The cards stay populated from what this caller can
			// actually see, rather than going blank or quoting the install's totals.
			visible := make(map[run.Status]int, len(runs))
			for _, rn := range runs {
				visible[rn.Status]++
			}
			summary = summarize(visible)
			summary.Scope = "visible"
		}
		respondJSON(w, log, http.StatusOK, listRunsResponse{
			Runs:       scrubbedRuns(r.Context(), maskRuns(runs)),
			Count:      len(runs),
			Summary:    summary,
			HasMore:    storeFullPage,
			NextOffset: nextOffset,
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
		// Grade the run's blast radius so an approver sees the risk without opening the log, and
		// grade whether it can be taken back, which risk does not answer.
		risk := run.AssessRisk(got)
		got.Risk = &risk
		undo := run.AssessReversibilityFrom(got, reversibilityEvidence(r.Context(), store, got))
		got.Reversibility = &undo
		respondJSON(w, log, http.StatusOK, scrubbedRun(r.Context(), maskRun(got)), wantsPretty(r))
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
			shardsResponse{Shards: scrubbedRuns(r.Context(), maskRuns(shards)), Count: len(shards)},
			wantsPretty(r))
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
			stepsResponse{Steps: scrubbedRuns(r.Context(), maskRuns(steps)), Count: len(steps)},
			wantsPretty(r))
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
		// A caller that only wants the end of the log says so, and only the end crosses the network.
		//
		// The run detail pane shows the last 256 KB and got there by downloading the whole log into
		// the browser and slicing it: a 213 MB log meant 213 MB over the wire and through the tab to
		// display a quarter of a megabyte of it. The tail is accumulated in a bounded buffer here, so
		// the control plane's memory stays bounded too, which is the property the chunked read was
		// written for in the first place.
		tail := tailBytes(r.URL.Query().Get("tail"))
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if tail == 0 {
			w.WriteHeader(http.StatusOK)
		}
		var (
			after     int64
			atLineEnd = true
			ring      *tailBuffer
		)
		if tail > 0 {
			ring = newTailBuffer(tail)
		}
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
				if ring != nil {
					ring.write(c.Data)
				} else if _, err := w.Write(c.Data); err != nil {
					log.Error("server: write run log: " + err.Error())
					return
				}
				if len(c.Data) > 0 {
					atLineEnd = c.Data[len(c.Data)-1] == '\n'
				}
			}
			if len(chunks) < streamBatch {
				if ring != nil {
					// The reader is told what they are looking at rather than left to assume the
					// log begins where the pane does.
					if omitted := ring.omitted(); omitted > 0 {
						w.Header().Set("Switchtender-Log-Truncated", "1")
						w.Header().Set("Switchtender-Log-Omitted-Bytes", strconv.FormatInt(omitted, 10))
					}
					w.WriteHeader(http.StatusOK)
					if _, err := w.Write(ring.bytes()); err != nil {
						log.Error("server: write run log tail: " + err.Error())
					}
				}
				return
			}
		}
	}
}

// scrubbedRun masks secret-shaped assignments in a run's command and launch variables for any caller
// below admin, the same rule the inventory list already follows.
//
// A run carried both verbatim on every read. The receipt scrubs them, the dossier scrubs them, the
// change register scrubs them, the webhook notification scrubs them, and the inventory list scrubs
// its own equivalents, so an operator reasonably concluded the product scrubs inline secrets. The
// run record did not, which made the read-only viewer role, the one an outside auditor is given,
// the single surface that showed a password in the clear.
//
// Only the assignments are masked, not the command, so an operator still reads what a run did. An
// admin sees the original: they hold every credential on the install already, and redacting for the
// person who maintains it only obstructs them, which is the reasoning redactInventories records.
func scrubbedRun(ctx context.Context, rn *run.Run) *run.Run {
	if actor, ok := actorFrom(ctx); ok && actor.Role == user.RoleAdmin {
		return rn
	}
	return redactRunCommand(rn)
}

// scrubbedRuns applies scrubbedRun across a list response.
func scrubbedRuns(ctx context.Context, list []*run.Run) []*run.Run {
	out := make([]*run.Run, len(list))
	for i, rn := range list {
		out[i] = scrubbedRun(ctx, rn)
	}
	return out
}

// maxLogTail bounds what one request may ask to keep in memory while it finds the end of a log.
const maxLogTail = 4 << 20

// tailBytes reads the tail parameter, returning zero for a request that wants the whole log.
func tailBytes(raw string) int {
	if raw == "" {
		return 0
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 0
	}
	return min(n, maxLogTail)
}

// tailBuffer keeps the last n bytes written to it and counts what it dropped, so a caller asking
// for the end of a very long log never holds more than n bytes anywhere.
type tailBuffer struct {
	// buf holds at most n bytes, the most recent ones.
	buf []byte
	// n is the cap.
	n int
	// dropped counts bytes discarded from the front.
	dropped int64
}

// newTailBuffer returns a buffer keeping the last n bytes.
func newTailBuffer(n int) *tailBuffer {
	return &tailBuffer{buf: make([]byte, 0, n), n: n}
}

// write appends data, discarding from the front once the cap is reached.
func (t *tailBuffer) write(data []byte) {
	if len(data) >= t.n {
		t.dropped += int64(len(t.buf)) + int64(len(data)-t.n)
		t.buf = append(t.buf[:0], data[len(data)-t.n:]...)
		return
	}
	if over := len(t.buf) + len(data) - t.n; over > 0 {
		t.dropped += int64(over)
		t.buf = append(t.buf[:0], t.buf[over:]...)
	}
	t.buf = append(t.buf, data...)
}

// bytes returns the kept tail.
func (t *tailBuffer) bytes() []byte { return t.buf }

// omitted reports how many bytes were dropped from the front.
func (t *tailBuffer) omitted() int64 { return t.dropped }

// maxPlaybookScanBytes caps how much of a playbook is read to grade a run. A playbook this large is
// not one a person wrote, and the grade is a convenience on a read path rather than a reason to
// pull an arbitrary amount of a file into memory on every view of a run.
const maxPlaybookScanBytes = 1 << 20

// reversibilityEvidence gathers what can be known about whether a run can be taken back.
//
// Two sources, both optional. The per host outcome of a finished run answers whether anything
// actually changed, which no prediction can. The playbook's own text answers what an Ansible run
// would do, which the command line cannot because an Ansible run has no command.
//
// Reading the playbook is not a new capability: the server already hands that exact path to
// ansible-playbook when the run executes. Every failure here is silent on purpose, because a grade
// is an aid to an approver and a missing file is a reason to grade with less rather than to refuse
// to answer.
func reversibilityEvidence(ctx context.Context, store run.Store, r *run.Run) run.ReversibilityEvidence {
	var ev run.ReversibilityEvidence
	if r.Status.Terminal() {
		if hosts, err := store.RunHostSummaries(ctx, r.ID); err == nil {
			ev.Hosts = hosts
		}
	}
	if run.NormalizeTool(r.Tool) == run.ToolAnsible && r.Playbook != "" {
		if f, err := os.Open(r.Playbook); err == nil {
			defer func() { _ = f.Close() }()
			if content, rerr := io.ReadAll(io.LimitReader(f, maxPlaybookScanBytes)); rerr == nil {
				ev.Playbook = content
			}
		}
	}
	return ev
}
