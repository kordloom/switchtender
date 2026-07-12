package server

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"go.uber.org/zap"

	"github.com/dcadolph/yardmaster/internal/ai"
	"github.com/dcadolph/yardmaster/internal/run"
)

// proposeRunSystemPrompt instructs the model to translate a request into one structured run and
// nothing else. The generated run never executes on its own, so the model is told to be direct
// rather than cautious, but to leave destructive choices to the reviewer.
const proposeRunSystemPrompt = "You turn a plain-language request into one Yardmaster run. Reply " +
	"with only a JSON object and nothing else, no prose and no markdown fence. The object has these " +
	"fields: tool (one of bash, python, go, ansible), command (the script for bash, python, or go), " +
	"playbook (the playbook path for ansible), limit (an optional host pattern), dry_run (true to " +
	"run in no-change mode), and summary (a one-line description of what the run does). Set command " +
	"for bash, python, and go; set playbook for ansible; leave the other empty. Keep the command " +
	"minimal and do exactly what was asked, nothing more."

// proposeIntentCap bounds the request intent so a huge paste cannot bloat the provider request.
const proposeIntentCap = 1000

// proposeBodyCap bounds the request body read.
const proposeBodyCap = 1 << 16

// proposeRequest is the body of POST /ai/propose-run.
type proposeRequest struct {
	// Intent is the plain-language description of the run to propose.
	Intent string `json:"intent"`
}

// proposedRun is the structured run the model returns, validated before anything is created.
type proposedRun struct {
	// Tool selects the execution engine.
	Tool string `json:"tool"`
	// Command is the script for a non-ansible tool.
	Command string `json:"command"`
	// Playbook is the playbook path for the ansible tool.
	Playbook string `json:"playbook"`
	// Limit is an optional host pattern.
	Limit string `json:"limit"`
	// DryRun asks for the tool's no-change mode.
	DryRun bool `json:"dry_run"`
	// Summary is a one-line description of the run.
	Summary string `json:"summary"`
}

// proposeRunHandler turns a plain-language request into a proposed run held for approval. The model
// proposes the run, but the run is built and validated by the server, born pending approval, and
// stamped with the request that generated it. It never executes on its own: an approver reviews the
// exact generated command against the request and releases or rejects it, and both the request and
// the decision land in the audit trail. Proposing is operator work, exactly like launching a run.
func proposeRunHandler(submitter Submitter, provider ai.Provider, log *zap.Logger) http.HandlerFunc {
	if submitter == nil {
		panic("server: proposeRunHandler: Submitter required")
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if provider == nil {
			respondError(w, log, http.StatusNotFound, "ai is not enabled")
			return
		}
		var req proposeRequest
		if err := json.NewDecoder(io.LimitReader(r.Body, proposeBodyCap)).Decode(&req); err != nil {
			respondError(w, log, http.StatusBadRequest, "invalid json body")
			return
		}
		intent := strings.TrimSpace(req.Intent)
		if intent == "" {
			respondError(w, log, http.StatusBadRequest, "a description is required")
			return
		}
		intent = clip(intent, proposeIntentCap)

		answer, err := provider.Complete(r.Context(), proposeRunSystemPrompt, "Request: "+intent)
		if err != nil {
			respondAIError(w, log, "propose run", err)
			return
		}
		proposal, ok := parseProposedRun(answer)
		if !ok {
			respondError(w, log, http.StatusUnprocessableEntity,
				"could not turn that into a run, try rephrasing it")
			return
		}

		opts := []run.SubmitOption{
			run.WithTool(proposal.Tool),
			run.WithCommand(proposal.Command),
			run.WithDryRun(proposal.DryRun),
			run.WithLimit(proposal.Limit),
			run.WithRequireApproval(true),
			run.WithIntent(intent),
		}
		created, err := submitter.Submit(r.Context(), proposal.Playbook, "", opts...)
		if err != nil {
			log.Error("server: propose run submit: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not create the proposal")
			return
		}
		respondJSON(w, log, http.StatusAccepted, created, wantsPretty(r))
	}
}

// parseProposedRun decodes the model reply into a validated proposed run, reporting false when the
// reply is not a usable run: unparseable, an unknown tool, or a tool missing its required input.
func parseProposedRun(answer string) (proposedRun, bool) {
	raw := stripFences(answer)
	start := strings.IndexByte(raw, '{')
	end := strings.LastIndexByte(raw, '}')
	if start < 0 || end <= start {
		return proposedRun{}, false
	}
	var p proposedRun
	if err := json.Unmarshal([]byte(raw[start:end+1]), &p); err != nil {
		return proposedRun{}, false
	}
	p.Tool = strings.ToLower(strings.TrimSpace(p.Tool))
	p.Command = strings.TrimSpace(p.Command)
	p.Playbook = strings.TrimSpace(p.Playbook)
	p.Limit = strings.TrimSpace(p.Limit)
	if !run.ValidTool(p.Tool) {
		return proposedRun{}, false
	}
	if run.NormalizeTool(p.Tool) == run.ToolAnsible {
		if p.Playbook == "" {
			return proposedRun{}, false
		}
		p.Command = ""
	} else if p.Command == "" {
		return proposedRun{}, false
	}
	return p, true
}
