package server

import (
	"context"
	"net/http"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/dispatch"
	"github.com/kordloom/switchtender/internal/run"
)

// healthReporter is a submitter that can say whether it records what its runs do. The dispatcher is
// one; a submitter that is not, such as a test's, reports nothing and readiness rests on the store.
type healthReporter interface {
	// Health reports this process's write health.
	Health() dispatch.Health
}

// readyAnswer is the readiness probe's body.
type readyAnswer struct {
	// Ready is true when the store answers.
	Ready bool `json:"ready"`
	// Degraded is true when the store answers but this process recently failed to write to it, or
	// holds run ends it has not yet taken.
	Degraded bool `json:"degraded,omitempty"`
	// Reason says why the server is not ready.
	Reason string `json:"reason,omitempty"`
	// Reasons say why the server is degraded, one line each.
	Reasons []string `json:"reasons,omitempty"`
	// LastWriteFailure is when a write on the run path last failed.
	LastWriteFailure *time.Time `json:"last_write_failure,omitempty"`
	// OwedEnds counts runs whose end the store has not yet taken, which read as running meanwhile.
	OwedEnds int `json:"owed_ends,omitempty"`
}

// healthHandler reports service liveness.
func healthHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		respondJSON(w, zap.NewNop(), http.StatusOK, map[string]string{"status": "ok"}, false)
	}
}

// readyHandler reports whether the server can serve real work, not just that its process is up. It
// touches the store with a bounded query, so a database that is unreachable or still starting
// answers 503 and a load balancer holds traffic off until it responds. /healthz stays a pure
// liveness check that never touches the store, so the two probes mean different things: alive, and
// ready. A Kubernetes deployment wires /healthz to livenessProbe and /readyz to readinessProbe.
//
// A store that answers reads but refuses writes, a full disk, looked ready. Readiness now also
// reports degraded, still with 200, for a while after a write on the run path failed and while any
// run's end is held for the store, since those runs read as running until it lands. Degraded keeps
// the server in rotation on purpose: a single install taken out by its own probe would also take
// down the page that tells its operator why.
func readyHandler(store run.Store, submitter Submitter) http.HandlerFunc {
	reporter, _ := submitter.(healthReporter)
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		if _, err := store.RunStatusCounts(ctx); err != nil {
			respondJSON(w, zap.NewNop(), http.StatusServiceUnavailable,
				readyAnswer{Reason: "the run store is not reachable"}, false)
			return
		}
		answer := readyAnswer{Ready: true}
		if reporter != nil {
			if h := reporter.Health(); h.Degraded {
				answer.Degraded, answer.Reasons, answer.OwedEnds = true, h.Reasons, h.OwedEnds
				if !h.LastWriteFailure.IsZero() {
					at := h.LastWriteFailure.UTC()
					answer.LastWriteFailure = &at
				}
			}
		}
		respondJSON(w, zap.NewNop(), http.StatusOK, answer, false)
	}
}

// actorName returns the authenticated caller's audit name, empty when the API runs open.
func actorName(r *http.Request) string {
	if a, ok := actorFrom(r.Context()); ok {
		return a.Name
	}
	return ""
}

// actorAccount returns the account behind the caller's credential, empty when the API runs open or the
// credential names no account. It is stamped beside the actor's name because the name is the
// credential's and differs between a person's token and their browser session, so anything asking "is
// this the same person" has to compare the account.
func actorAccount(r *http.Request) string {
	if a, ok := actorFrom(r.Context()); ok {
		return a.UserID
	}
	return ""
}

// actorType returns how the caller authenticated, in the audit chain's vocabulary, empty when the
// API runs open. Stamped on submitted runs so a policy can tell an agent's request from a person's.
func actorType(r *http.Request) string {
	if a, ok := actorFrom(r.Context()); ok {
		return a.Type
	}
	return ""
}
