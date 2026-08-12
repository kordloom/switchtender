package server

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/ai"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/util"
)

// askSystemPrompt frames the model as a fleet analyst bound to the snapshot it is given.
const askSystemPrompt = "You answer questions about a SwitchTender automation fleet. Answer only " +
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
func askFleetHandler(store run.Store, provider ai.Provider, authz *authorizer,
	log *zap.Logger) http.HandlerFunc {
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
		if !decodeStrict(w, log, io.LimitReader(r.Body, askBodyCap), &req) {
			return
		}
		question := strings.TrimSpace(req.Question)
		if question == "" {
			respondError(w, log, http.StatusBadRequest, "a question is required")
			return
		}
		question = util.Clip(question, askQuestionCap)
		if !limiter.allow() {
			respondError(w, log, http.StatusTooManyRequests, "too many questions, wait a minute")
			return
		}
		// The snapshot is built from what this caller may read, not from the whole install. It was
		// assembled with an empty filter and no authorization, so a viewer in one organization got
		// an answer drawn from every organization's runs: playbook names, the first line of shell
		// and Terraform commands, host names, and which hosts have drifted. The model then repeats
		// it in prose, which is a comfortable way for a boundary to leak.
		snapshot, serr := buildFleetSnapshot(r.Context(), store, authz)
		if serr != nil {
			log.Error("server: build fleet snapshot: " + serr.Error())
			respondError(w, log, http.StatusInternalServerError, "could not read the fleet")
			return
		}
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
func buildFleetSnapshot(ctx context.Context, store run.Store,
	authz *authorizer) (string, error) {
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

	// Read a wider page than is shown, because filtering removes rows and a caller granted a few
	// runs should still see their most recent ones rather than whatever survived the first page.
	page, err := store.ListPage(ctx, run.ListFilter{}, askSnapshotRuns*8, 0)
	if err != nil {
		return "", err
	}
	runs, err := readableRuns(ctx, authz, page)
	if err != nil {
		return "", err
	}
	if len(runs) > askSnapshotRuns {
		runs = runs[:askSnapshotRuns]
	}
	if len(runs) > 0 {
		b.WriteString("\nRecent runs, newest first:\n")
		for _, r := range runs {
			target := r.Playbook
			if run.NormalizeTool(r.Tool) != run.ToolAnsible {
				target = util.Clip(strings.SplitN(r.Command, "\n", 2)[0], 60)
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

	// Fleet health and drift are aggregates over every run on the install, so a caller whose run
	// list was just filtered must not receive them. Filtering the run list and then appending an
	// unfiltered host table returned exactly the hosts the filter removed, and the drift lines named
	// the run ids suppressed a few paragraphs earlier. There is no per-host grant to filter these
	// by, since a host is not an object grants are written against, so a restricted caller is given
	// the runs they may read and no estate-wide summary at all.
	restricted, err := grantsEnforced(ctx, authz)
	if err != nil {
		return "", err
	}
	if health, err := store.FleetHealth(ctx, defaultFleetWindow); !restricted && err == nil && len(health) > 0 {
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

	if drift, err := store.DriftStatus(ctx); !restricted && err == nil {
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
	return b.String(), nil
}
