package server

import (
	"errors"
	"net/http"
	"strings"

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

// explainRunHandler asks the configured AI provider to explain a run from its status, error, and log
// tail. It is advisory and read-only: it never changes the run or starts anything, and the log it
// sends is already masked of secrets when the run is recorded.
func explainRunHandler(store run.Store, provider ai.Provider, log *zap.Logger) http.HandlerFunc {
	if store == nil {
		panic("server: explainRunHandler: Store required")
	}
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
		body, _ := store.Log(r.Context(), id) // best effort: a run may have produced no log yet
		answer, err := provider.Complete(r.Context(), explainSystemPrompt, buildExplainPrompt(rn, body))
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
		b.WriteString(rn.Command)
	}
	if rn.Error != "" {
		b.WriteString("\nError: ")
		b.WriteString(rn.Error)
	}
	tail := logBytes
	if len(tail) > explainLogTail {
		tail = tail[len(tail)-explainLogTail:]
	}
	if len(tail) > 0 {
		b.WriteString("\n\nLog tail:\n")
		b.Write(tail)
	}
	return b.String()
}
