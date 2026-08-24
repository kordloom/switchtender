package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/ai"
	"github.com/kordloom/switchtender/internal/event"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/util"
)

// respondAIError maps a provider failure to an HTTP response, telling the user the model declined
// when a safety layer refused rather than that the provider is unreachable, and logs the detail
// server-side. A cloud model configured with a fallback retries a decline before it reaches here,
// so this covers the case where even the fallback declined.
func respondAIError(w http.ResponseWriter, log *zap.Logger, context string, err error) {
	log.Error("server: " + context + ": " + err.Error())
	if errors.Is(err, ai.ErrRefused) {
		respondError(w, log, http.StatusBadGateway, "the model declined this request")
		return
	}
	respondError(w, log, http.StatusBadGateway, "the ai provider did not respond")
}

// explainSystemPrompt frames the model as a triage assistant that only summarizes the input.
const explainSystemPrompt = "You are a site reliability engineer helping triage an automation run. " +
	"Given the run's tool, status, error, failed tasks, per host stats, and log tail, explain the " +
	"most likely cause of the outcome in two to four sentences and suggest one concrete next step. " +
	"Be specific and concise. Rely only on the details provided and do not invent hosts, files, or " +
	"errors that are not present."

// proposalSystemPrompt frames the model as a reviewer of a held reconcile proposal.
const proposalSystemPrompt = "You are a site reliability engineer reviewing a proposed reconcile " +
	"run before it is approved. Given the drift a check run observed and what the proposal will " +
	"execute, summarize in two to four sentences what drifted and what approving will change, and " +
	"point out anything risky. Rely only on the details provided and do not invent hosts, files, " +
	"or tasks that are not present."

// intentProposalSystemPrompt frames the model as a reviewer of a run proposed from a plain-language
// request, checking the generated run against what was asked.
const intentProposalSystemPrompt = "You are a site reliability engineer reviewing a run that was " +
	"proposed from a plain-language request, before it is approved. Given the request and the exact " +
	"run that was generated, say in two to four sentences what the run will do, whether it matches " +
	"the request, and call out anything risky or destructive an approver should weigh. Rely only on " +
	"the details provided."

// explainLogTail is how many trailing bytes of the run log to include: enough for context without
// overwhelming the model or the request.
const explainLogTail = 6000

// explainCommandCap is how many leading bytes of a step's script or command to include. A script
// body can be arbitrarily large, and its opening lines carry the interpreter and the intent.
const explainCommandCap = 2000

// explainEventWindow is how many trailing events to scan for failures and stats. Failures and the
// final recap cluster at the end of a run, and a fixed window bounds the read for huge runs.
const explainEventWindow = 500

// explainMaxEvents is how many failing task events to include, preferring the most recent.
const explainMaxEvents = 10

// explainEventBudget caps the failed-task section in bytes so a wide failure cannot bloat the
// prompt.
const explainEventBudget = 4000

// explainStatsHosts is how many hosts the stats recap lists, worst first.
const explainStatsHosts = 20

// explainCacheTTL is how long a computed explanation is reused. A terminal run's answer is stable,
// so the TTL is a memory bound, not a freshness requirement.
const explainCacheTTL = 15 * time.Minute

// explainCall tracks one in-flight or completed explanation for a run.
type explainCall struct {
	// done closes when the provider call finishes.
	done chan struct{}
	// text is the explanation, set before done closes.
	text string
	// err is the provider failure, set before done closes.
	err error
	// expires is when the result stops being served from cache, set before done closes.
	expires time.Time
}

// explainGroup deduplicates concurrent explain calls and caches successful answers keyed by run and
// status, so repeated clicks or a scripted loop cannot multiply provider spend at viewer privilege.
type explainGroup struct {
	// mu guards calls.
	mu sync.Mutex
	// calls maps a cache key, the run ID and its status, to its latest call.
	calls map[string]*explainCall
}

// do returns the cached or deduplicated explanation for key, invoking f at most once per expiry
// window across concurrent callers. The key includes the run's status, so an answer computed while
// a run was held for approval is not reused after it is approved and finishes. Failures are not
// cached, so a provider hiccup can be retried.
func (g *explainGroup) do(key string, f func() (string, error)) (string, error) {
	g.mu.Lock()
	if c, ok := g.calls[key]; ok {
		select {
		case <-c.done:
			if c.err == nil && time.Now().Before(c.expires) {
				g.mu.Unlock()
				return c.text, nil
			}
		default:
			g.mu.Unlock()
			<-c.done
			return c.text, c.err
		}
	}
	c := &explainCall{done: make(chan struct{})}
	g.calls[key] = c
	g.prune()
	g.mu.Unlock()

	c.text, c.err = f()
	c.expires = time.Now().Add(explainCacheTTL)
	close(c.done)
	return c.text, c.err
}

// prune drops expired completed entries. The caller holds mu.
func (g *explainGroup) prune() {
	now := time.Now()
	for id, c := range g.calls {
		select {
		case <-c.done:
			if now.After(c.expires) {
				delete(g.calls, id)
			}
		default:
		}
	}
}

// explainRunHandler asks the configured AI provider to explain a run from its status, error, and log
// tail. It is advisory and read-only: it never changes the run or starts anything, and the log it
// sends is already masked of secrets when the run is recorded. Only terminal runs and held
// proposals are explainable, since a partial log produces confident but stale triage, and answers
// are cached per run and status so repeated requests do not multiply provider spend.
func explainRunHandler(store run.Store, provider ai.Provider, authz *authorizer, log *zap.Logger) http.HandlerFunc {
	if store == nil {
		panic("server: explainRunHandler: Store required")
	}
	group := &explainGroup{calls: make(map[string]*explainCall)}
	return func(w http.ResponseWriter, r *http.Request) {
		if provider == nil {
			respondError(w, log, http.StatusNotFound, "ai is not enabled")
			return
		}
		id := r.PathValue("id")
		rn, err := store.Get(r.Context(), id)
		if errors.Is(err, run.ErrNotFound) {
			respondError(w, log, http.StatusNotFound, "run not found")
			return
		}
		if err != nil {
			log.Error("server: explain run get: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not read run")
			return
		}
		if authorizeRunAccess(w, r, authz, log, rn) {
			return
		}
		reconcileProposal := rn.ProposedFrom != "" && rn.Status == run.StatusPendingApproval
		intentProposal := rn.Intent != "" && rn.Status == run.StatusPendingApproval
		if !rn.Status.Terminal() && !reconcileProposal && !intentProposal {
			respondError(w, log, http.StatusConflict, "run is not finished")
			return
		}
		system := explainSystemPrompt
		var prompt string
		switch {
		case reconcileProposal:
			source, err := store.Get(r.Context(), rn.ProposedFrom)
			if err != nil {
				log.Error("server: explain proposal source: " + err.Error())
				respondError(w, log, http.StatusInternalServerError, "could not read the check run")
				return
			}
			system = proposalSystemPrompt
			prompt = buildProposalPrompt(rn, source, explainEvents(r.Context(), store, rn.ProposedFrom))
		case intentProposal:
			system = intentProposalSystemPrompt
			prompt = buildIntentProposalPrompt(rn)
		default:
			// Only the tail is wanted, so only the tail is read. Reading the whole log to slice the end
			// off meant a run that failed after a gigabyte of output, which is exactly the run somebody
			// asks about, allocated that gigabyte to use six kilobytes of it, on a path a viewer may
			// call.
			prompt = buildExplainPrompt(rn, logTail(r.Context(), store, id, explainLogTail),
				explainEvents(r.Context(), store, id))
		}
		answer, err := group.do(id+"|"+string(rn.Status), func() (string, error) {
			return provider.Complete(r.Context(), system, prompt)
		})
		if err != nil {
			respondAIError(w, log, "explain run", err)
			return
		}
		respondJSON(w, log, http.StatusOK,
			map[string]string{"explanation": strings.TrimSpace(answer)}, wantsPretty(r))
	}
}

// tailChunkBatch is how many stored log chunks a tail read walks back at a time. A chunk is one flush
// from the executor, so a handful covers the wanted bytes on any real run, and the walk continues when
// they do not.
const tailChunkBatch = 16

// logTail returns at most want trailing bytes of a run's log, reading only as far back as it needs.
//
// A run's log is stored as appended chunks with a rising sequence, so the end can be read directly. This
// walks back from the newest sequence, widening the window until it holds enough bytes, and each read
// replaces the last rather than adding to it: the two stores number log chunks differently, one by chunk
// and one by byte offset, and re-reading from an earlier point is correct under both. It stops as soon as
// it has the wanted bytes, so the cost tracks the size of the tail rather than the size of the log.
//
// Reading the whole log to slice the end off it meant a run that failed after a gigabyte of output, which
// is exactly the run somebody asks about, allocated that gigabyte on the control node to use six kilobytes
// of it, on a path a viewer may call.
//
// It is best effort throughout. A run may have produced no log, and an explanation without a tail is
// better than no explanation.
func logTail(ctx context.Context, store run.Store, id string, want int) []byte {
	last, err := store.LastLogSeq(ctx, id)
	if err != nil || last <= 0 {
		return nil
	}
	var held []byte
	for step := int64(tailChunkBatch); ; step *= 4 {
		start := last - step
		if start < 0 {
			start = 0
		}
		chunks, cerr := store.LogAfter(ctx, id, start, 0)
		if cerr != nil {
			break
		}
		held = held[:0]
		for _, c := range chunks {
			held = append(held, c.Data...)
		}
		if len(held) >= want || start == 0 {
			break
		}
	}
	if len(held) > want {
		held = held[len(held)-want:]
	}
	return held
}

// buildExplainPrompt assembles a compact triage prompt from a run, its structured events, and the
// tail of its log. Events are already masked at ingest with the same masker as the log.
func buildExplainPrompt(rn *run.Run, logBytes []byte, events []event.Event) string {
	var b strings.Builder
	b.WriteString("Tool: ")
	b.WriteString(run.NormalizeTool(rn.Tool))
	b.WriteString("\nStatus: ")
	b.WriteString(string(rn.Status))
	if rn.Playbook != "" {
		b.WriteString("\nPlaybook: ")
		b.WriteString(rn.Playbook)
	}
	if rn.Command != "" {
		b.WriteString("\nCommand: ")
		b.WriteString(promptCommand(rn.Command, explainCommandCap))
	}
	if rn.Error != "" {
		b.WriteString("\nError: ")
		b.WriteString(rn.Error)
	}
	if section := failedTaskSection(events); section != "" {
		b.WriteString("\n\nFailed tasks:\n")
		b.WriteString(section)
	}
	if recap := statsSection(events); recap != "" {
		b.WriteString("\nHost stats:\n")
		b.WriteString(recap)
	}
	tail := logBytes
	if len(tail) > explainLogTail {
		tail = tail[len(tail)-explainLogTail:]
		for len(tail) > 0 && !utf8.RuneStart(tail[0]) {
			tail = tail[1:]
		}
	}
	if len(tail) > 0 {
		b.WriteString("\n\nLog tail:\n")
		b.Write(tail)
	}
	return b.String()
}

// buildProposalPrompt assembles the review prompt for a held reconcile proposal: what the proposal
// will execute and the drift the source check run observed on the target host. Events are already
// masked at ingest with the same masker as the log.
func buildProposalPrompt(rn, source *run.Run, events []event.Event) string {
	var b strings.Builder
	b.WriteString("Proposal: run playbook ")
	b.WriteString(source.Playbook)
	b.WriteString(" for real, limited to host ")
	b.WriteString(rn.Limit)
	b.WriteString(".\nObserved by check run ")
	b.WriteString(source.ID)
	b.WriteString(" with status ")
	b.WriteString(string(source.Status))
	b.WriteString(".")
	if section := driftedTaskSection(events, rn.Limit); section != "" {
		b.WriteString("\n\nDrifted tasks, which the check would change:\n")
		b.WriteString(section)
	}
	return b.String()
}

// buildIntentProposalPrompt assembles the review prompt for a run proposed from a plain-language
// request: the request itself and the exact run that was generated from it.
func buildIntentProposalPrompt(rn *run.Run) string {
	var b strings.Builder
	b.WriteString("Request: ")
	b.WriteString(rn.Intent)
	b.WriteString("\n\nGenerated run:\nTool: ")
	b.WriteString(run.NormalizeTool(rn.Tool))
	if rn.Playbook != "" {
		b.WriteString("\nPlaybook: ")
		b.WriteString(rn.Playbook)
	}
	if rn.Command != "" {
		b.WriteString("\nCommand: ")
		b.WriteString(promptCommand(rn.Command, explainCommandCap))
	}
	if rn.Limit != "" {
		b.WriteString("\nLimited to hosts: ")
		b.WriteString(rn.Limit)
	}
	if rn.DryRun {
		b.WriteString("\nMode: check mode, no changes")
	} else {
		b.WriteString("\nMode: real, applies changes")
	}
	return b.String()
}

// driftedTaskSection renders the tasks the check run would change on the host, one line each,
// within the event byte budget.
func driftedTaskSection(events []event.Event, host string) string {
	var b strings.Builder
	shown, total := 0, 0
	for _, e := range events {
		if !e.Changed || (host != "" && e.Host != host) {
			continue
		}
		total++
		if shown >= explainMaxEvents {
			continue
		}
		line := "- " + e.Play + " / " + e.Task
		if msg := strings.TrimSpace(e.Message); msg != "" {
			line += ": " + util.Clip(msg, 300)
		}
		if b.Len()+len(line) > explainEventBudget {
			continue
		}
		b.WriteString(line)
		b.WriteString("\n")
		shown++
	}
	if total > shown && shown > 0 {
		fmt.Fprintf(&b, "- and %d more drifted tasks\n", total-shown)
	}
	return b.String()
}

// explainEvents returns the trailing window of a run's events. It pages the run with the cursor the
// store hands back rather than subtracting the window from the last sequence: event sequences are
// global, so on a busy install that subtraction skips the run's own earlier events, its failures
// among them, because other runs' events sit between them. It is best effort and returns nil on any
// error, since events are optional context for the prompt.
func explainEvents(ctx context.Context, store run.Store, id string) []event.Event {
	const page = 1000
	var window []event.Event
	var after int64
	for {
		batch, err := store.EventsAfter(ctx, id, after, page)
		if err != nil || len(batch) == 0 {
			break
		}
		window = append(window, batch...)
		if len(window) > explainEventWindow {
			window = window[len(window)-explainEventWindow:]
		}
		after = batch[len(batch)-1].Seq
		if len(batch) < page {
			break
		}
	}
	return window
}

// failedTaskSection renders the most recent failing task events, one entry each, within a byte
// budget. It keeps a short message and stderr excerpt per event and skips bulky stdout, diff, and
// published outputs entirely.
func failedTaskSection(events []event.Event) string {
	var failed []event.Event
	for _, e := range events {
		if e.Type == event.TypeRunnerFailed || e.Type == event.TypeRunnerUnreachable {
			failed = append(failed, e)
		}
	}
	if len(failed) > explainMaxEvents {
		failed = failed[len(failed)-explainMaxEvents:]
	}
	var b strings.Builder
	for _, e := range failed {
		line := "- " + e.Play + " / " + e.Task + " on " + e.Host
		if e.RC != nil {
			line += fmt.Sprintf(" (rc=%d)", *e.RC)
		}
		if msg := strings.TrimSpace(e.Message); msg != "" {
			line += ": " + util.Clip(msg, 300)
		}
		if errOut := strings.TrimSpace(e.Stderr); errOut != "" {
			line += "\n  stderr: " + util.Clip(errOut, 300)
		}
		if b.Len()+len(line) > explainEventBudget {
			break
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}

// statsSection renders the per host recap from the final stats event, worst hosts first. The recap
// is integer counts only, so it carries no output content.
func statsSection(events []event.Event) string {
	for i := len(events) - 1; i >= 0; i-- {
		e := events[i]
		if e.Type != event.TypeStats || len(e.Stats) == 0 {
			continue
		}
		hosts := make([]string, 0, len(e.Stats))
		for h := range e.Stats {
			hosts = append(hosts, h)
		}
		sort.Slice(hosts, func(a, b int) bool {
			sa, sb := e.Stats[hosts[a]], e.Stats[hosts[b]]
			ba, bb := sa.Failures+sa.Unreachable, sb.Failures+sb.Unreachable
			if ba != bb {
				return ba > bb
			}
			return hosts[a] < hosts[b]
		})
		shown := hosts
		if len(shown) > explainStatsHosts {
			shown = shown[:explainStatsHosts]
		}
		var b strings.Builder
		for _, h := range shown {
			s := e.Stats[h]
			fmt.Fprintf(&b, "- %s: ok=%d changed=%d failed=%d unreachable=%d skipped=%d\n",
				h, s.OK, s.Changed, s.Failures, s.Unreachable, s.Skipped)
		}
		if len(hosts) > len(shown) {
			fmt.Fprintf(&b, "- and %d more hosts\n", len(hosts)-len(shown))
		}
		return b.String()
	}
	return ""
}

// headBytes returns up to limit leading bytes of s without splitting a multibyte rune, appending a
// truncation note when the value was cut.
// promptCommand returns a run's script trimmed to limit and with inline secrets removed, for the one
// place the raw script leaves the host.
//
// redactForExternal blanks Command from every webhook and plugin notification, saying plainly that it
// "holds the raw script body of a bash, python, powershell, or go run, which can embed inline secrets
// or sensitive arguments". The AI prompts wrote that same field straight into a payload POSTed to
// api.openai.com or api.anthropic.com, so a bash run whose script carried a bearer token or a
// postgres URL handed it to a third party the moment somebody clicked Explain. The log tail beside it
// in the same prompt was already masked, which is what made this the one unredacted field in the
// payload.
//
// Blanking it outright would gut the feature, since the script is the thing being explained. The
// assignment scrub is what the audit chain and the inventory reader already use: a value under a
// secret-sounding name goes, everything else stays readable.
func promptCommand(command string, limit int) string {
	redacted, _ := util.RedactAssignments(command, "[redacted]")
	return headBytes(redacted, limit)
}

func headBytes(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "\n[truncated]"
}
