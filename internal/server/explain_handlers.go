package server

import (
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"go.uber.org/zap"

	"github.com/dcadolph/yardmaster/internal/ai"
	"github.com/dcadolph/yardmaster/internal/run"
)

// explainSystemPrompt frames the model as a triage assistant that only summarizes the input.
const explainSystemPrompt = "You are a site reliability engineer helping triage an automation run. " +
	"Given the run's tool, status, error, and log tail, explain the most likely cause of the outcome " +
	"in two to four sentences and suggest one concrete next step. Be specific and concise. Rely only " +
	"on the details provided and do not invent hosts, files, or errors that are not present."

// explainLogTail is how many trailing bytes of the run log to include: enough for context without
// overwhelming the model or the request.
const explainLogTail = 6000

// explainCommandCap is how many leading bytes of a step's script or command to include. A script
// body can be arbitrarily large, and its opening lines carry the interpreter and the intent.
const explainCommandCap = 2000

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
		answer, err := group.do(id, func() (string, error) {
			return provider.Complete(r.Context(), explainSystemPrompt, buildExplainPrompt(rn, body))
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

// buildExplainPrompt assembles a compact triage prompt from a run and the tail of its log.
func buildExplainPrompt(rn *run.Run, logBytes []byte) string {
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
