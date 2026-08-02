package relay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/kordloom/switchtender/internal/audit"
	"io"
	"net/http"
	"strconv"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/auth"
	"github.com/kordloom/switchtender/internal/event"
	"github.com/kordloom/switchtender/internal/policy"
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
	// policies is the approval policy store a worker reads across the relay, nil when the install
	// has none configured. The plan-content gate runs where the run executes, so a worker that
	// cannot read the policies cannot tell whether the run it claimed needs one.
	policies policy.Store
	// store is the shared run store the worker's calls read and write.
	store run.Store
	// pools maps a presented token to the worker pool it belongs to and the queues that pool may
	// lease from.
	pools *Pools
	// audits records the decisions a worker reports, nil when the install keeps no audit trail.
	audits audit.Store
	// log records server-side faults, never token material.
	log *zap.Logger
}

// NewHandler returns an http.Handler that serves the Transport methods over the run store, guarded
// by the worker pools. Mount it on the control node so relay workers have a path to the shared
// store. It panics on a nil store or no pools, both wiring errors; a nil logger becomes a no-op.
func NewHandler(store run.Store, pools *Pools, log *zap.Logger,
	policies policy.Store, audits audit.Store) http.Handler {
	if store == nil {
		panic("relay: Store required")
	}
	if pools == nil || len(pools.pools) == 0 {
		panic("relay: at least one worker pool required")
	}
	if log == nil {
		log = zap.NewNop()
	}
	s := &relayServer{store: store, pools: pools, log: log, policies: policies, audits: audits}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /relay/v1/policies", s.listPolicies)
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
		pool := s.pools.resolve(presented)
		if pool == nil {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		// The pool travels with the request so the claim can hold the queues asked for against the
		// queues this token may lease from.
		next.ServeHTTP(w, r.WithContext(withPool(r.Context(), pool)))
	})
}

// relayMethod names the transport a relay entry came through, so a reader can tell a worker report
// from an API call without parsing the path.
const relayMethod = "RELAY"

// record writes one relay decision into the audit chain.
//
// The relay serves the mesh from outside the API's own gate, so nothing a worker reported reached
// the trail at all. For a product whose claim is that every change is provable, a run starting and
// finishing on a machine the control node cannot reach is exactly the change worth recording.
//
// What is recorded is the decision, not the stream. A worker leasing a run and a run reaching a
// terminal state are events. Captured output, structured events, summaries, and heartbeats are the
// content and liveness of a run that is already recorded, they arrive several times a second, and
// writing each one into a hash chain would drown the record it is meant to make readable.
//
// The append is deliberately not fail closed here. Refusing a worker's report because the audit
// store is unhealthy does not un-finish the run; it loses the outcome of work that already happened
// on real hosts. The API refuses a mutation it cannot record because refusing prevents it. This one
// cannot be prevented, so it is logged loudly instead.
func (s *relayServer) record(ctx context.Context, actor, path string) {
	if s.audits == nil {
		return
	}
	entry := &audit.Entry{
		ID: audit.NewID(), At: time.Now(), Actor: actor,
		Method: relayMethod, Path: path,
	}
	if err := s.audits.Append(ctx, entry); err != nil {
		s.log.Error("relay: record worker report: "+err.Error(), zap.String("path", path))
	}
}

// claim leases the oldest pending run the owner's queues serve, returning it as JSON, or 204 when
// nothing is pending.
func (s *relayServer) claim(w http.ResponseWriter, r *http.Request) {
	var body claimRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid claim body")
		return
	}
	// A worker names the queues it serves, and the token decides which of those it may have. Serving
	// whatever was asked for made a queue a routing hint rather than a boundary: any worker holding
	// the shared token could lease from any queue, so the least trusted machine in the estate could
	// take a production run and execute it with production credentials.
	if pool := poolFrom(r.Context()); pool != nil {
		if q, ok := pool.allows(body.Queues); !ok {
			writeErr(w, http.StatusForbidden,
				"this worker token does not serve queue "+strconv.Quote(q))
			return
		}
	}
	leased, err := s.store.Claim(r.Context(), body.Owner, body.Queues)
	switch {
	case errors.Is(err, run.ErrNonePending):
		w.WriteHeader(http.StatusNoContent)
	case err != nil:
		s.internal(w, "claim", err)
	default:
		// A worker took a run onto a machine the control node cannot reach. That is the moment the
		// work left this side of the boundary, so it is the moment worth recording.
		s.record(r.Context(), "worker:"+body.Owner, "/relay/claim/"+leased.ID)
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
	if err := checkWorkerReport(stored, &body); err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	wasTerminal := stored.Status.Terminal()
	applyWorkerReport(stored, &body)
	if err := s.store.Save(r.Context(), stored); err != nil {
		s.internal(w, "save", err)
		return
	}
	// Only the transition into a terminal state is recorded. A worker saves repeatedly as a run
	// progresses, and the outcome is the part somebody asks about later.
	if !wasTerminal && stored.Status.Terminal() {
		s.record(r.Context(), "worker:"+stored.ClaimedBy,
			"/relay/finished/"+stored.ID+"/"+string(stored.Status))
	}
	w.WriteHeader(http.StatusNoContent)
}

// workerStatuses are the states a worker may report. A worker claims a run, executes it, and says
// how it ended; it never puts a run back in the queue and never reopens one that finished.
var workerStatuses = map[run.Status]bool{
	run.StatusRunning:     true,
	run.StatusSucceeded:   true,
	run.StatusFailed:      true,
	run.StatusCanceled:    true,
	run.StatusInterrupted: true,
}

// checkWorkerReport rejects a report a worker has no business making.
//
// Constraining the spec was not enough, and constraining the status was not enough either. There is
// one shared worker token, so the question is not only what may be reported but which runs may be
// reported on at all. Without that, a token holder could mark a queued run succeeded so its
// playbook never ran, cancel a run another worker was executing, or report on a run nobody had
// claimed and take it out of band, bypassing the queue that decides order.
func checkWorkerReport(stored, reported *run.Run) error {
	if !workerStatuses[reported.Status] {
		return fmt.Errorf("a worker may not set status %q", reported.Status)
	}
	if stored.Status.Terminal() {
		return fmt.Errorf("run already finished as %q and cannot be reopened", stored.Status)
	}
	// Claiming is what hands a run to a worker, and it happens through the claim endpoint. A run
	// with no holder has not been handed to anyone, so there is nothing for a worker to report.
	if stored.ClaimedBy == "" {
		return fmt.Errorf("run is not claimed, so there is nothing for a worker to report on")
	}
	// A run awaiting a decision was never claimable in the first place.
	if stored.Status == run.StatusPendingApproval || stored.Status == run.StatusRejected {
		return fmt.Errorf("run is awaiting a decision and is not a worker's to report on")
	}
	// A report has to come from the holder. The lease is the only identity available here, since
	// every worker presents the same token.
	if reported.ClaimedBy != "" && reported.ClaimedBy != stored.ClaimedBy {
		return fmt.Errorf("run is held by another executor")
	}
	return nil
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
	// The lease is not taken from the wire. Copying it let a worker clear a live parent's holder,
	// and a parent that is running with no holder is exactly what the abandoned-parent sweep
	// settles, so one request turned into a remote kill switch on any long-running split or
	// pipeline. The control node already knows who holds a run, because it granted the claim.
	stored.CommitSHA = reported.CommitSHA
	if len(reported.Outputs) > 0 {
		stored.Outputs = reported.Outputs
	}
}

// heldForReport reports whether the run named in the request is one a worker may write a record
// for, answering the caller and returning false when it is not.
//
// The holder boundary covers the record, not only the status. A worker token refused a status
// report on a run awaiting a decision could still append "PLAY RECAP ok=12 failed=0" to that run's
// captured output, and could append to a run held by a different executor. What an approver reads
// while deciding is exactly the thing worth forging, so the same question that gates a status
// report gates the writes that build the record: which runs may be reported on at all.
func (s *relayServer) heldForReport(w http.ResponseWriter, r *http.Request) bool {
	stored, err := s.store.Get(r.Context(), r.PathValue("id"))
	switch {
	case errors.Is(err, run.ErrNotFound):
		writeErr(w, http.StatusNotFound, "run not found")
		return false
	case err != nil:
		s.internal(w, "read run", err)
		return false
	}
	// A finished run is not an error, it is a no-op. The store already drops these writes silently,
	// so answering with a conflict changed nothing about what is recorded and started a retry storm
	// instead: the transport retries the post, re-posts the whole batch on a timer, and keeps at it
	// for the abandon window while logging an error nobody can act on.
	if stored.Status.Terminal() {
		w.WriteHeader(http.StatusNoContent)
		return false
	}
	if stored.Status == run.StatusPendingApproval || stored.Status == run.StatusRejected {
		writeErr(w, http.StatusConflict, "run is awaiting a decision and is not a worker's to add to")
		return false
	}
	if stored.ClaimedBy == "" {
		writeErr(w, http.StatusConflict, "run is not claimed, so there is nothing to report on")
		return false
	}
	return true
}

// listPolicies serves the approval policies in force, so a worker across the relay can evaluate the
// plan-content gate the same way the control node would.
//
// An install with no policy store configured answers with an empty list rather than an error. That
// is the honest answer: there are no policies, so nothing is gated, and it is different from being
// unable to tell.
func (s *relayServer) listPolicies(w http.ResponseWriter, r *http.Request) {
	if s.policies == nil {
		s.writeJSON(w, []*policy.Policy{})
		return
	}
	all, err := s.policies.List(r.Context())
	if err != nil {
		s.internal(w, "list policies", err)
		return
	}
	if all == nil {
		all = []*policy.Policy{}
	}
	s.writeJSON(w, all)
}

// appendLog appends the raw request body to the run's captured output, or 404 when the run is gone.
func (s *relayServer) appendLog(w http.ResponseWriter, r *http.Request) {
	if !s.heldForReport(w, r) {
		return
	}
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
	if !s.heldForReport(w, r) {
		return
	}
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
	if !s.heldForReport(w, r) {
		return
	}
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
	if !s.heldForReport(w, r) {
		return
	}
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
