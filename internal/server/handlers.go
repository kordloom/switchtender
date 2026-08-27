package server

import (
	"context"
	"net/http"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/run"
)

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
func readyHandler(store run.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		if _, err := store.RunStatusCounts(ctx); err != nil {
			respondJSON(w, zap.NewNop(), http.StatusServiceUnavailable,
				map[string]any{"ready": false, "reason": "the run store is not reachable"}, false)
			return
		}
		respondJSON(w, zap.NewNop(), http.StatusOK, map[string]any{"ready": true}, false)
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
