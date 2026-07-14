package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/dcadolph/railwarden/internal/ai"
	"github.com/dcadolph/railwarden/internal/run"
)

// askSystemPrompt frames the model as a fleet analyst bound to the snapshot it is given.
const askSystemPrompt = "You answer questions about a Railwarden automation fleet. Answer only " +
	"from the snapshot provided: run status counts, recent runs, host health, and drift. When the " +
	"snapshot does not hold the answer, say so plainly. Reply in two to five sentences, specific, " +
	"and do not invent hosts, runs, or numbers."

// askQuestionCap bounds the question so a huge paste cannot bloat the provider request.
const askQuestionCap = 500

// askBodyCap bounds the request body read.
const askBodyCap = 1 << 16

// askSnapshotRuns is how many recent runs the snapshot lists.
const askSnapshotRuns = 25

// askSnapshotHosts is how many hosts the health and drift sections list, worst first.
const askSnapshotHosts = 15

// askRateLimit is how many questions one instance answers per minute, so a scripted loop at
// viewer privilege cannot multiply provider spend.
const askRateLimit = 10

// askLimiter is a fixed-window counter shared by every ask request on this instance.
type askLimiter struct {
	// mu guards the window.
	mu sync.Mutex
	// windowStart is when the current window opened.
	windowStart time.Time
	// count is how many questions the current window has answered.
	count int
}

// allow consumes one slot in the current window, reporting false when the window is spent.
func (l *askLimiter) allow() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if now.Sub(l.windowStart) > time.Minute {
		l.windowStart = now
		l.count = 0
	}
	if l.count >= askRateLimit {
		return false
	}
	l.count++
	return true
}

// askRequest is the body of POST /ai/ask.
type askRequest struct {
	// Question is the plain-language question about the fleet.
	Question string `json:"question"`
}

// askFleetHandler answers a plain-language question about the fleet. The data is assembled
// deterministically from the store, metadata only, and the model answers from that snapshot
// alone. It is advisory and read-only: asking changes nothing and starts nothing.
func askFleetHandler(store run.Store, provider ai.Provider, log *zap.Logger) http.HandlerFunc {
	if store == nil {
		panic("server: askFleetHandler: Store required")
	}
	limiter := &askLimiter{}
	return func(w http.ResponseWriter, r *http.Request) {
		if provider == nil {
			respondError(w, log, http.StatusNotFound, "ai is not enabled")
			return
		}
		var req askRequest
		if err := json.NewDecoder(io.LimitReader(r.Body, askBodyCap)).Decode(&req); err != nil {
			respondError(w, log, http.StatusBadRequest, "invalid json body")
			return
		}
		question := strings.TrimSpace(req.Question)
		if question == "" {
			respondError(w, log, http.StatusBadRequest, "a question is required")
			return
		}
		question = clip(question, askQuestionCap)
		if !limiter.allow() {
			respondError(w, log, http.StatusTooManyRequests, "too many questions, wait a minute")
			return
		}
		snapshot := buildFleetSnapshot(r.Context(), store)
		answer, err := provider.Complete(r.Context(), askSystemPrompt,
			snapshot+"\n\nQuestion: "+question)
		if err != nil {
			respondAIError(w, log, "ask fleet", err)
			return
		}
		respondJSON(w, log, http.StatusOK,
			map[string]string{"answer": strings.TrimSpace(answer)}, wantsPretty(r))
	}
}

// buildFleetSnapshot assembles the metadata the model may answer from: run status counts, recent
// runs, the worst hosts by failures, and drifted hosts. Every section is bounded and carries no
// log content, variables, or credential material.
func buildFleetSnapshot(ctx context.Context, store run.Store) string {
	var b strings.Builder
	b.WriteString("Fleet snapshot.\n")

	if counts, err := store.RunStatusCounts(ctx); err == nil && len(counts) > 0 {
		b.WriteString("\nRun counts by status:\n")
		for _, status := range []run.Status{
			run.StatusPending, run.StatusRunning, run.StatusPendingApproval,
			run.StatusSucceeded, run.StatusFailed, run.StatusCanceled,
			run.StatusInterrupted, run.StatusRejected,
		} {
			if n := counts[status]; n > 0 {
				fmt.Fprintf(&b, "- %s: %d\n", status, n)
			}
		}
	}

	if runs, err := store.ListPage(ctx, "", askSnapshotRuns, 0); err == nil && len(runs) > 0 {
		b.WriteString("\nRecent runs, newest first:\n")
		for _, r := range runs {
			target := r.Playbook
			if run.NormalizeTool(r.Tool) != run.ToolAnsible {
				target = clip(strings.SplitN(r.Command, "\n", 2)[0], 60)
			}
			line := fmt.Sprintf("- %s %s %s %s", r.ID, run.NormalizeTool(r.Tool), r.Status, target)
			if r.DryRun {
				line += " (check mode)"
			}
			if r.ProposedFrom != "" {
				line += " (reconcile proposal from " + r.ProposedFrom + ")"
			}
			b.WriteString(line + "\n")
		}
	}

	if health, err := store.FleetHealth(ctx, defaultFleetWindow); err == nil && len(health) > 0 {
		b.WriteString("\nHost health, worst first, over each host's recent runs:\n")
		for i, h := range health {
			if i >= askSnapshotHosts {
				fmt.Fprintf(&b, "- and %d more hosts\n", len(health)-i)
				break
			}
			fmt.Fprintf(&b, "- %s: %d failures in %d runs, last outcome %s",
				h.Host, h.Failures, h.Total, h.LastOutcome)
			if h.Flaky {
				b.WriteString(", flaky")
			}
			b.WriteString("\n")
		}
	}

	if drift, err := store.DriftStatus(ctx); err == nil {
		var drifted int
		for _, d := range drift {
			if d.DriftedTasks > 0 {
				drifted++
			}
		}
		if drifted > 0 {
			b.WriteString("\nDrifted hosts, most drifted first:\n")
			shown := 0
			for _, d := range drift {
				if d.DriftedTasks == 0 {
					continue
				}
				if shown >= askSnapshotHosts {
					fmt.Fprintf(&b, "- and %d more drifted hosts\n", drifted-shown)
					break
				}
				fmt.Fprintf(&b, "- %s: %d drifted tasks, checked by %s\n",
					d.Host, d.DriftedTasks, d.RunID)
				shown++
			}
		}
	}
	return b.String()
}
