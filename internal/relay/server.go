package relay

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/auth"
	"github.com/kordloom/switchtender/internal/event"
	"github.com/kordloom/switchtender/internal/run"
)

// claimRequest is the body of a relay claim: the owner leasing work and the queues it serves.
type claimRequest struct {
	// Owner is the lease name the worker stamps on the run it claims.
	Owner string `json:"owner"`
	// Queues names the queues the worker serves; empty carries the default pool.
	Queues []string `json:"queues"`
}

// heartbeatRequest is the body of a relay heartbeat: the run and the owner renewing its lease.
type heartbeatRequest struct {
	// ID is the run whose lease is renewed.
	ID string `json:"id"`
	// Owner is the lease holder renewing it.
	Owner string `json:"owner"`
}

// errorBody is the JSON error envelope the relay endpoints return, and the transport decodes.
type errorBody struct {
	// Error is a short failure message.
	Error string `json:"error"`
}

// relayServer serves the worker execution path over HTTP, backed by a run.Store. It is the control
// node side of the phase-1 mesh: a worker's httpTransport dials it over one outbound connection to
// lease and execute runs without a path to the shared database.
type relayServer struct {
	// store is the shared run store the worker's calls read and write.
	store run.Store
	// token is the worker bearer token every call must present.
	token string
	// log records server-side faults, never token material.
	log *zap.Logger
}

// NewHandler returns an http.Handler that serves the Transport methods over the run store, guarded
// by the worker bearer token. Mount it on the control node so relay workers have a path to the
// shared store. It panics on a nil store or an empty token, both wiring errors; a nil logger becomes
// a no-op.
func NewHandler(store run.Store, token string, log *zap.Logger) http.Handler {
	if store == nil {
		panic("relay: Store required")
	}
	if token == "" {
		panic("relay: worker token required")
	}
	if log == nil {
		log = zap.NewNop()
	}
	s := &relayServer{store: store, token: token, log: log}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /relay/v1/claim", s.claim)
	mux.HandleFunc("POST /relay/v1/heartbeat", s.heartbeat)
	mux.HandleFunc("GET /relay/v1/runs/{id}", s.get)
	mux.HandleFunc("POST /relay/v1/runs/{id}/save", s.save)
	mux.HandleFunc("POST /relay/v1/runs/{id}/log", s.appendLog)
	mux.HandleFunc("POST /relay/v1/runs/{id}/events", s.appendEvents)
	mux.HandleFunc("POST /relay/v1/runs/{id}/host-summary", s.saveHostSummary)
	mux.HandleFunc("POST /relay/v1/runs/{id}/task-summary", s.saveTaskSummary)
	return s.authed(mux)
}

// authed guards next with the worker bearer token, compared in constant time so a wrong guess leaks
// no timing signal. A missing or wrong token gets 401.
func (s *relayServer) authed(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		presented := auth.FromHeader(r.Header.Get("Authorization"))
		if subtle.ConstantTimeCompare([]byte(presented), []byte(s.token)) != 1 {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// claim leases the oldest pending run the owner's queues serve, returning it as JSON, or 204 when
// nothing is pending.
func (s *relayServer) claim(w http.ResponseWriter, r *http.Request) {
	var body claimRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid claim body")
		return
	}
	leased, err := s.store.Claim(r.Context(), body.Owner, body.Queues)
	switch {
	case errors.Is(err, run.ErrNonePending):
		w.WriteHeader(http.StatusNoContent)
	case err != nil:
		s.internal(w, "claim", err)
	default:
		s.writeJSON(w, leased)
	}
}

// heartbeat renews the owner's lease on a run, or 404 when the lease is gone.
func (s *relayServer) heartbeat(w http.ResponseWriter, r *http.Request) {
	var body heartbeatRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid heartbeat body")
		return
	}
	err := s.store.Heartbeat(r.Context(), body.ID, body.Owner)
	switch {
	case errors.Is(err, run.ErrNotFound):
		writeErr(w, http.StatusNotFound, "run not found")
	case err != nil:
		s.internal(w, "heartbeat", err)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// get returns the run with the id in the path, or 404.
func (s *relayServer) get(w http.ResponseWriter, r *http.Request) {
	got, err := s.store.Get(r.Context(), r.PathValue("id"))
	switch {
	case errors.Is(err, run.ErrNotFound):
		writeErr(w, http.StatusNotFound, "run not found")
	case err != nil:
		s.internal(w, "get", err)
	default:
		s.writeJSON(w, got)
	}
}

// save records a worker's report about a run it is executing.
//
// A worker reports an outcome, never a spec. This used to decode a whole run.Run and hand it
// straight to Save, which is an upsert, so a holder of the worker token could post a run that did
// not exist with any playbook, command, and credential ids it liked, and the control node's claim
// loop would lease and execute it. That path answered to no approval policy, no object grant, and
// no audit entry, because it never went through the API gate at all.
//
// Now the stored run is authoritative for everything that decides what executes, and only the
// fields a worker learns by running it are taken from the body.
func (s *relayServer) save(w http.ResponseWriter, r *http.Request) {
	var body run.Run
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid run body")
		return
	}
	id := r.PathValue("id")
	// A report has to be about the run it was addressed to.
	if body.ID != "" && body.ID != id {
		writeErr(w, http.StatusBadRequest, "run id does not match the path")
		return
	}
	stored, err := s.store.Get(r.Context(), id)
	switch {
	case errors.Is(err, run.ErrNotFound):
		// A worker only ever reports on a run it claimed, so an unknown id is not a run to create.
		writeErr(w, http.StatusNotFound, "run not found")
		return
	case err != nil:
		s.internal(w, "save", err)
		return
	}
	applyWorkerReport(stored, &body)
	if err := s.store.Save(r.Context(), stored); err != nil {
		s.internal(w, "save", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// applyWorkerReport copies onto stored the fields a worker learns by executing a run, and leaves
// every field that decides what executes alone.
//
// The split is the security boundary. Status, timing, exit code, failure text, the lease, and the
// values a playbook published are things only the executing process knows. The playbook, command,
// tool, credentials, project, inventory, image, extra vars, host limit, and queue are things the
// control node decided when it accepted the run, after checking policy and grants, and a worker
// that could change them could change what it was authorized to do.
func applyWorkerReport(stored, reported *run.Run) {
	stored.Status = reported.Status
	stored.ExitCode = reported.ExitCode
	stored.Error = reported.Error
	stored.Warning = reported.Warning
	stored.StartedAt = reported.StartedAt
	stored.EndedAt = reported.EndedAt
	stored.ClaimedBy = reported.ClaimedBy
	stored.ClaimedAt = reported.ClaimedAt
	stored.CommitSHA = reported.CommitSHA
	if len(reported.Outputs) > 0 {
		stored.Outputs = reported.Outputs
	}
}

// appendLog appends the raw request body to the run's captured output, or 404 when the run is gone.
func (s *relayServer) appendLog(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read log body")
		return
	}
	err = s.store.AppendLog(r.Context(), r.PathValue("id"), body)
	switch {
	case errors.Is(err, run.ErrNotFound):
		writeErr(w, http.StatusNotFound, "run not found")
	case err != nil:
		s.internal(w, "append log", err)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// appendEvents appends the structured events in the body to the run, or 404 when the run is gone.
func (s *relayServer) appendEvents(w http.ResponseWriter, r *http.Request) {
	var events []event.Event
	if err := json.NewDecoder(r.Body).Decode(&events); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid events body")
		return
	}
	err := s.store.AppendEvents(r.Context(), r.PathValue("id"), events)
	switch {
	case errors.Is(err, run.ErrNotFound):
		writeErr(w, http.StatusNotFound, "run not found")
	case err != nil:
		s.internal(w, "append events", err)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// saveHostSummary replaces the run's per-host summaries with those in the body.
func (s *relayServer) saveHostSummary(w http.ResponseWriter, r *http.Request) {
	var summaries []run.HostSummary
	if err := json.NewDecoder(r.Body).Decode(&summaries); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid host summary body")
		return
	}
	if err := s.store.SaveHostSummary(r.Context(), r.PathValue("id"), summaries); err != nil {
		s.internal(w, "save host summary", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// saveTaskSummary replaces the run's per-task summaries with those in the body.
func (s *relayServer) saveTaskSummary(w http.ResponseWriter, r *http.Request) {
	var summaries []run.TaskSummary
	if err := json.NewDecoder(r.Body).Decode(&summaries); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid task summary body")
		return
	}
	if err := s.store.SaveTaskSummary(r.Context(), r.PathValue("id"), summaries); err != nil {
		s.internal(w, "save task summary", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// writeJSON writes v as a 200 JSON response.
func (s *relayServer) writeJSON(w http.ResponseWriter, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		s.internal(w, "marshal response", err)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(body); err != nil {
		s.log.Error("relay: write response: " + err.Error())
	}
}

// internal logs a server-side fault and writes a 500, keeping the underlying error off the wire so
// the worker sees only the failed operation, not store internals.
func (s *relayServer) internal(w http.ResponseWriter, op string, err error) {
	s.log.Error("relay: " + op + ": " + err.Error())
	writeErr(w, http.StatusInternalServerError, op+" failed")
}

// writeErr writes a JSON error body with the given status. It does not log, since a client-caused
// status carries the outcome on its own; server faults are logged where they arise.
func writeErr(w http.ResponseWriter, status int, msg string) {
	body, _ := json.Marshal(errorBody{Error: msg})
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
