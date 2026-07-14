package server

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"go.uber.org/zap"

	"github.com/dcadolph/railwarden/internal/ai"
)

// draftSystemPrompt frames the model as a script author whose output a human reviews and edits.
const draftSystemPrompt = "You write one script for an automation step. Reply with only the raw " +
	"script body for the requested tool: no prose, no markdown fences, no explanations. Keep it " +
	"short and idempotent where the task allows, and add brief comments only where intent is not " +
	"obvious."

// draftPromptCap bounds the description so a huge paste cannot bloat the provider request.
const draftPromptCap = 2000

// draftBodyCap bounds the request body read.
const draftBodyCap = 1 << 16

// draftTools are the tools whose step input is an inline script a draft can fill. Ansible steps
// reference a playbook path and terraform steps reference a directory, so neither takes a draft.
var draftTools = map[string]bool{"bash": true, "python": true, "powershell": true, "go": true}

// draftRequest is the body of POST /ai/draft.
type draftRequest struct {
	// Tool is the step tool the script targets: bash, python, powershell, or go.
	Tool string `json:"tool"`
	// Prompt describes what the step should do.
	Prompt string `json:"prompt"`
}

// draftStepHandler asks the configured AI provider for a step script from a plain description. It
// is advisory: the draft lands in an editor for a human to review, change, and save, and nothing
// executes from here. The route requires the operator role, since drafts feed execution
// configuration.
func draftStepHandler(provider ai.Provider, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if provider == nil {
			respondError(w, log, http.StatusNotFound, "ai is not enabled")
			return
		}
		var req draftRequest
		if err := json.NewDecoder(io.LimitReader(r.Body, draftBodyCap)).Decode(&req); err != nil {
			respondError(w, log, http.StatusBadRequest, "invalid json body")
			return
		}
		tool := strings.ToLower(strings.TrimSpace(req.Tool))
		if !draftTools[tool] {
			respondError(w, log, http.StatusBadRequest, "tool must be bash, python, powershell, or go")
			return
		}
		prompt := strings.TrimSpace(req.Prompt)
		if prompt == "" {
			respondError(w, log, http.StatusBadRequest, "a description is required")
			return
		}
		prompt = clip(prompt, draftPromptCap)
		answer, err := provider.Complete(r.Context(), draftSystemPrompt, "Tool: "+tool+"\nTask: "+prompt)
		if err != nil {
			respondAIError(w, log, "draft step", err)
			return
		}
		respondJSON(w, log, http.StatusOK,
			map[string]string{"draft": stripFences(answer)}, wantsPretty(r))
	}
}

// stripFences removes a wrapping markdown code fence from a model reply, since models add one even
// when told not to.
func stripFences(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	lines := strings.Split(s, "\n")
	if len(lines) < 2 {
		return s
	}
	lines = lines[1:]
	if strings.TrimSpace(lines[len(lines)-1]) == "```" {
		lines = lines[:len(lines)-1]
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}
