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

	"github.com/dcadolph/yardmaster/internal/ai"
	"github.com/dcadolph/yardmaster/internal/event"
	"github.com/dcadolph/yardmaster/internal/run"
)

// explainSystemPrompt frames the model as a triage assistant that only summarizes the input.
const explainSystemPrompt = "You are a site reliability engineer helping triage an automation run. " +
	"Given the run's tool, status, error, failed tasks, per host stats, and log tail, explain the " +
	"most likely cause of the outcome in two to four sentences and suggest one concrete next step. " +
	"Be specific and concise. Rely only on the details provided and do not invent hosts, files, or " +
	"errors that are not present."

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

// explainGroup deduplicates concurrent explain calls per run and caches successful answers, so
// repeated clicks or a scripted loop cannot multiply provider spend at viewer privilege.
type explainGroup struct {
	// mu guards calls.
	mu sync.Mutex
	// calls maps run ID to its latest call.
	calls map[string]*explainCall
}

// do returns the cached or deduplicated explanation for id, invoking f at most once per expiry
// window across concurrent callers. Failures are not cached, so a provider hiccup can be retried.
func (g *explainGroup) do(id string, f func() (string, error)) (string, error) {
	g.mu.Lock()
	if c, ok := g.calls[id]; ok {
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
	g.calls[id] = c
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
// sends is already masked of secrets when the run is recorded. Only terminal runs are explainable,
// since a partial log produces confident but stale triage, and answers are cached per run so
// repeated requests do not multiply provider spend.
func explainRunHandler(store run.Store, provider ai.Provider, log *zap.Logger) http.HandlerFunc {
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
		if !rn.Status.Terminal() {
			respondError(w, log, http.StatusConflict, "run is not finished")
			return
		}
		body, _ := store.Log(r.Context(), id) // best effort: a run may have produced no log
		events := explainEvents(r.Context(), store, id)
		answer, err := group.do(id, func() (string, error) {
			return provider.Complete(r.Context(), explainSystemPrompt, buildExplainPrompt(rn, body, events))
		})
		if err != nil {
			log.Error("server: explain run: " + err.Error())
			respondError(w, log, http.StatusBadGateway, "the ai provider did not respond")
			return
		}
		respondJSON(w, log, http.StatusOK,
			map[string]string{"explanation": strings.TrimSpace(answer)}, wantsPretty(r))
	}
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
		b.WriteString(headBytes(rn.Command, explainCommandCap))
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

// explainEvents returns the trailing window of a run's events. It is best effort and returns nil
// on any error, since events are optional context for the prompt.
func explainEvents(ctx context.Context, store run.Store, id string) []event.Event {
	last, err := store.LastEventSeq(ctx, id)
	if err != nil || last <= 0 {
		return nil
	}
	after := last - explainEventWindow
	if after < 0 {
		after = 0
	}
	events, err := store.EventsAfter(ctx, id, after, explainEventWindow)
	if err != nil {
		return nil
	}
	return events
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
			line += ": " + clip(msg, 300)
		}
		if errOut := strings.TrimSpace(e.Stderr); errOut != "" {
			line += "\n  stderr: " + clip(errOut, 300)
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

// clip returns up to limit leading bytes of s without splitting a multibyte rune, appending an
// ellipsis when the value was cut.
func clip(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "..."
}

// headBytes returns up to limit leading bytes of s without splitting a multibyte rune, appending a
// truncation note when the value was cut.
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
