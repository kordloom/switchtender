package relay

import (
	"context"
	"crypto/subtle"
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
	"github.com/kordloom/switchtender/internal/dispatch"
	"github.com/kordloom/switchtender/internal/event"
	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/run"
)

// leaseHeader carries the per-claim capability. The claim response sets it once, and every report,
// record write, and heartbeat a worker makes for that run presents it back. It is a header rather
// than a body field because the field is json:"-": the secret must never land in a body a worker can
// log, cache, or forward, and a header is read once into memory instead.
const leaseHeader = "X-Switchtender-Lease"

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
	mux.HandleFunc("POST /relay/v1/runs/{id}/propose-apply", s.proposeApply)
	mux.HandleFunc("POST /relay/v1/runs/{id}/host-summary", s.saveHostSummary)
	mux.HandleFunc("POST /relay/v1/runs/{id}/host-facts", s.saveHostFacts)
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

// actorTypeWorker classifies a relay entry as a machine acting for the estate, distinct from the
// API's human and token actors. A relay worker executes runs on a machine the control node cannot
// reach, so "service" is the right class from the vocabulary audit.Entry.ActorType documents, and it
// is different from every value the API emits, which is what lets a reader tell a worker's report
// from an operator's API call in the chain.
const actorTypeWorker = "service"

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
func (s *relayServer) record(ctx context.Context, pool *Pool, owner, path string) {
	if s.audits == nil {
		return
	}
	// The proven identity is the pool the presented token resolved to, since every worker in a pool
	// shares one token. The owner is the lease name the worker asserted, which is not proof of
	// anything on its own. Record the proven pool first, then the asserted name, so a reader can tell
	// them apart. pool.Name is a non-secret operator label; only the token's hash is ever stored.
	actor := "worker:" + owner
	if pool != nil {
		actor = "pool:" + pool.Name + " " + actor
	}
	entry := &audit.Entry{
		ID: audit.NewID(), At: time.Now(), Actor: actor, ActorType: actorTypeWorker,
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
		s.record(r.Context(), poolFrom(r.Context()), body.Owner, "/relay/claim/"+leased.ID)
		// The capability travels in a header, not the body. The field is json:"-", so it is not in
		// the body a worker can log, cache, or forward, and this claim response is the one place it
		// crosses the wire. The transport reads it once and keeps it only in memory, presenting it on
		// the reports it makes for this run.
		w.Header().Set(leaseHeader, leased.ClaimSecret)
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
	// Renewing a lease keeps a run alive, so it answers the same question every other call does: is
	// this the run's holder, and is this a run this pool serves. The stored run is fetched for both,
	// and the not-found answer is reused for a mismatch so a caller learns nothing about a run it
	// does not hold.
	stored, gerr := s.store.Get(r.Context(), body.ID)
	if gerr != nil {
		writeErr(w, http.StatusNotFound, "run not found")
		return
	}
	if pool := poolFrom(r.Context()); pool != nil {
		if _, ok := pool.allows([]string{stored.Queue}); !ok {
			writeErr(w, http.StatusNotFound, "run not found")
			return
		}
	}
	// The capability minted at claim renews the lease, the same proof the reports carry. A run
	// claimed before it existed has no secret and falls back to the owner match the store applies.
	if !leaseHeld(stored, r) {
		writeErr(w, http.StatusNotFound, "run not found")
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
// servesRun reports whether the calling pool may touch this run at all, answering the caller and
// returning nil when it may not.
//
// Confining the claim was not confinement. A pool that could not lease a production run could still
// read it by id, append to its log, and save a terminal status over it, because seven of the eight
// endpoints never looked at the pool. So the queue bounded which work a token could start and
// nothing else: a worker in the least trusted segment read the playbook, the inventory, the
// credential ids, and the extra vars of a production run, and could cancel it. A queue is a
// boundary or it is not; it cannot be one for a single endpoint.
func (s *relayServer) servesRun(w http.ResponseWriter, r *http.Request) *run.Run {
	stored, err := s.store.Get(r.Context(), r.PathValue("id"))
	switch {
	case errors.Is(err, run.ErrNotFound):
		writeErr(w, http.StatusNotFound, "run not found")
		return nil
	case err != nil:
		s.internal(w, "read run", err)
		return nil
	}
	if pool := poolFrom(r.Context()); pool != nil {
		if q, ok := pool.allows([]string{stored.Queue}); !ok {
			// Answered as not found rather than forbidden. A pool that may not serve this queue has
			// no business learning that a run with this id exists.
			_ = q
			writeErr(w, http.StatusNotFound, "run not found")
			return nil
		}
	}
	return stored
}

func (s *relayServer) get(w http.ResponseWriter, r *http.Request) {
	got := s.servesRun(w, r)
	if got == nil {
		return
	}
	s.writeJSON(w, got)
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
	// A worker only ever reports on a run it claimed, so an unknown id is not a run to create, and
	// a run outside this pool's queues is not this pool's to report on.
	stored := s.servesRun(w, r)
	if stored == nil {
		return
	}
	// The capability minted at claim is the proof this report comes from the run's holder. Every
	// worker presents the same shared token, so the lease name a report carries is asserted, not
	// proven, and a worker that reads another run's id could otherwise forge a terminal report for
	// it. A run claimed before the capability existed carries no secret and is checked the older way.
	if !leaseHeld(stored, r) {
		writeErr(w, http.StatusForbidden, "the run's lease was not presented or did not match")
		return
	}
	if err := checkWorkerReport(stored, &body, stored.ClaimSecret != ""); err != nil {
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
		// The control node commits the run's outcome to the chain here, the same entry a run executed
		// in process gets, so a relay run is receiptable too. The worker streamed its log, events, and
		// summaries before this terminal save, so the evidence is in the store to digest. A child of a
		// split or pipeline is skipped, its outcome rolled into the parent the coordinator commits, and
		// the commit is not fail-closed since the run has already happened.
		if s.audits != nil && stored.ParentID == nil {
			if err := outcome.Commit(r.Context(), s.audits, s.store, stored, "system:relay"); err != nil {
				s.log.Error("relay: commit run outcome: "+err.Error(), zap.String("run_id", stored.ID))
			}
		}
		s.record(r.Context(), poolFrom(r.Context()), stored.ClaimedBy,
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
// leaseHeld reports whether the request carries the capability minted for this run's current claim.
// A run claimed before the capability existed carries no stored secret, so it is accepted here and
// the caller falls back to the older lease-name check; this is what keeps runs already in flight at
// upgrade time from stranding. A run that has a secret must present the matching one, compared in
// constant time so a wrong guess reveals nothing through how long the comparison took.
func leaseHeld(stored *run.Run, r *http.Request) bool {
	if stored.ClaimSecret == "" {
		return true
	}
	presented := r.Header.Get(leaseHeader)
	if presented == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(presented), []byte(stored.ClaimSecret)) == 1
}

// checkWorkerReport rejects a report a worker has no business making. leaseVerified is true when the
// run carried a per-claim secret and the request presented the matching one, which already proves
// the report comes from the holder; the older lease-name comparison then adds nothing and is skipped.
// It stays in force for a run claimed before the capability existed, whose secret is empty.
func checkWorkerReport(stored, reported *run.Run, leaseVerified bool) error {
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
	// A split or pipeline parent is coordinated by the control node, never executed by a worker: the
	// claim loop skips any run carrying a kind, so no worker can ever have been handed one. Its
	// holder is the control node's own coordinator, so without this a worker that learned the
	// parent's id, which its own claim response gives it, could terminalize the coordinator's run and
	// strand every shard beneath it. A report on one is a report on work this worker did not do.
	if stored.Kind != "" {
		return fmt.Errorf("run is coordinated by the control node and is not a worker's to report on")
	}
	// A run awaiting a decision was never claimable in the first place.
	if stored.Status == run.StatusPendingApproval || stored.Status == run.StatusRejected {
		return fmt.Errorf("run is awaiting a decision and is not a worker's to report on")
	}
	// A report has to come from the holder. When the run carries a per-claim secret, presenting it
	// is that proof and the caller has already checked it, so the lease name need not be sent. For a
	// run claimed before the capability existed there is no secret, and the lease name is the only
	// identity available since every worker presents the same token. It is compared even when the
	// field is absent: treating an empty value as "no claim to check" let a worker omitting
	// claimed_by skip the holder test entirely and report on, and terminate, a run held by somebody
	// else.
	if !leaseVerified && reported.ClaimedBy != stored.ClaimedBy {
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
	stored := s.servesRun(w, r)
	if stored == nil {
		return false
	}
	// The same capability that gates a status report gates the writes that build the record. A
	// worker's captured log and events are what an approver reads while deciding, so they are exactly
	// the thing worth forging, and one shared token is not proof of who is writing. A run claimed
	// before the capability existed carries no secret and is left to the holder checks below.
	if !leaseHeld(stored, r) {
		writeErr(w, http.StatusForbidden, "the run's lease was not presented or did not match")
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
	// A split or pipeline parent is coordinated by the control node and executed by nobody, so no
	// worker has output to add to it. Its captured log and events are the rollup of its children, and
	// letting a worker append to it would write output into the record of work it did not do. This is
	// the same boundary the report check draws, applied to the writers that carry the evidence.
	if stored.Kind != "" {
		writeErr(w, http.StatusConflict,
			"run is coordinated by the control node and is not a worker's to add to")
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

// maxRelayElements bounds how many items one relay call may carry.
//
// A count cap is not a work cap on its own: "[{},{},{}...]" fits a million empty structs into a
// megabyte, and each one becomes a marshal and a single-row insert inside one transaction. Measured,
// a one megabyte body decoded into 349,525 events and 366 MB of heap, and on SQLite held the single
// writer for that many round trips. A real run reports in batches of tens. The cap is enforced during
// the decode, not after it, so a body over the cap never lands whole in memory.
const maxRelayElements = 5000

// errTooManyElements is returned by decodeCapped when the body carries more than the cap allows. It
// is mapped to 413 so a worker learns to report in smaller batches.
var errTooManyElements = errors.New("too many items in one call; report in smaller batches")

// decodeCapped decodes a JSON array of T, refusing once it holds more than max elements so a worker
// cannot force the whole array into memory before the cap applies. It streams the array a token at a
// time rather than decoding the slice whole, which is what makes the cap bound the work rather than
// only the result. A body that is not a JSON array is a decode error, the same as before.
func decodeCapped[T any](r io.Reader, max int) ([]T, error) {
	dec := json.NewDecoder(r)
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '[' {
		return nil, fmt.Errorf("expected a JSON array")
	}
	out := make([]T, 0)
	for dec.More() {
		if len(out) >= max {
			return nil, errTooManyElements
		}
		var v T
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	return out, nil
}

// appendEvents appends the structured events in the body to the run, or 404 when the run is gone.
func (s *relayServer) appendEvents(w http.ResponseWriter, r *http.Request) {
	if !s.heldForReport(w, r) {
		return
	}
	events, derr := decodeCapped[event.Event](r.Body, maxRelayElements)
	switch {
	case errors.Is(derr, errTooManyElements):
		writeErr(w, http.StatusRequestEntityTooLarge, derr.Error())
		return
	case derr != nil:
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
	summaries, derr := decodeCapped[run.HostSummary](r.Body, maxRelayElements)
	switch {
	case errors.Is(derr, errTooManyElements):
		writeErr(w, http.StatusRequestEntityTooLarge, derr.Error())
		return
	case derr != nil:
		writeErr(w, http.StatusBadRequest, "invalid host summary body")
		return
	}
	if err := s.store.SaveHostSummary(r.Context(), r.PathValue("id"), summaries); err != nil {
		s.internal(w, "save host summary", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// saveHostFacts replaces the facts the run gathered per host with those in the body.
func (s *relayServer) saveHostFacts(w http.ResponseWriter, r *http.Request) {
	if !s.heldForReport(w, r) {
		return
	}
	facts, derr := decodeCapped[run.HostFacts](r.Body, maxRelayElements)
	switch {
	case errors.Is(derr, errTooManyElements):
		writeErr(w, http.StatusRequestEntityTooLarge, derr.Error())
		return
	case derr != nil:
		writeErr(w, http.StatusBadRequest, "invalid host facts body")
		return
	}
	// Facts are bounded by the hosts this run has recorded results for. The table is keyed on the host
	// alone, so without this a worker leased one run could replace the recorded facts for any machine in
	// the fleet, or invent machines outright, and the control node would store it as gathered evidence.
	//
	// A worker also authors those results, so this does not make one trustworthy; it makes a fabrication
	// attributable. To write facts about a machine, a worker must first say on its own run's record that
	// the run touched it, which shows up in that run's dossier and on the fleet page.
	if bad := s.unrecordedHost(r.Context(), r.PathValue("id"), factHosts(facts)); bad != "" {
		writeErr(w, http.StatusForbidden, "this run has recorded no result for host "+bad+
			", so facts for it are refused: report the run's per-host results first")
		return
	}
	if err := s.store.SaveHostFacts(r.Context(), r.PathValue("id"), facts); err != nil {
		s.internal(w, "save host facts", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// factHosts lists the hosts a facts body names, in order.
func factHosts(facts []run.HostFacts) []string {
	out := make([]string, 0, len(facts))
	for _, f := range facts {
		out = append(out, f.Host)
	}
	return out
}

// unrecordedHost returns the first host in want that the run has no recorded result for, or empty when
// every one of them is accounted for. A host named as empty is skipped, since the store drops those
// anyway.
//
// The comparison is against the run's stored per-host summaries, which the executor writes before it
// writes facts, so an ordinary report passes and one arriving out of order is told to send its results
// first rather than being silently trusted.
func (s *relayServer) unrecordedHost(ctx context.Context, runID string, want []string) string {
	need := make(map[string]bool, len(want))
	for _, h := range want {
		if h != "" {
			need[h] = true
		}
	}
	if len(need) == 0 {
		return ""
	}
	summaries, err := s.store.RunHostSummaries(ctx, runID)
	if err != nil {
		// A store that cannot answer is not a store that said yes.
		s.log.Error("relay: read host summaries: " + err.Error())
		return want[0]
	}
	for _, hs := range summaries {
		delete(need, hs.Host)
	}
	for _, h := range want {
		if need[h] {
			return h
		}
	}
	return ""
}

// proposeApply creates the apply a worker's plan gated, from the plan run the control node holds.
//
// A worker has no path to create a run, on purpose, so the plan-content gate could not complete on one:
// the proposal failed with a 404 and the plan failed with it, leaving a gated terraform apply neither
// held nor run. The worker reports what its plan found and nothing else. Every field of the apply, its
// command, target, credentials, image, organization, requester, and the commit it is pinned to, comes
// from the stored plan, so a worker cannot use this to propose an apply of something it was not asked
// to plan.
func (s *relayServer) proposeApply(w http.ResponseWriter, r *http.Request) {
	var body struct {
		// Destroys is how many resources the plan said it would destroy.
		Destroys int `json:"destroys"`
		// Read reports whether that count came from a summary the parser found.
		Read bool `json:"read"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid propose body")
		return
	}
	plan := s.servesRun(w, r)
	if plan == nil {
		return
	}
	if !leaseHeld(plan, r) {
		writeErr(w, http.StatusForbidden, "the run's lease was not presented or did not match")
		return
	}
	// The policies are read here rather than taken from the worker, so the rule that decides the hold is
	// the control node's. A store that cannot answer holds the apply instead of queueing it, the same
	// fail-closed rule the in-process gate follows: a gate that could not be evaluated has not passed.
	var policies []*policy.Policy
	read := body.Read
	if s.policies == nil {
		read = false
	} else {
		list, err := s.policies.List(r.Context())
		if err != nil {
			s.log.Error("relay: list policies: " + err.Error())
			read = false
		} else {
			policies = list
		}
	}
	proposal, err := dispatch.ProposeApplyFor(r.Context(), s.store, policies, plan, body.Destroys, read)
	if err != nil {
		s.internal(w, "propose apply", err)
		return
	}
	s.record(r.Context(), poolFrom(r.Context()), plan.ClaimedBy,
		"/relay/proposed/"+plan.ID+"/"+proposal.ID)
	s.writeJSONStatus(w, http.StatusCreated, proposal)
}

// saveTaskSummary replaces the run's per-task summaries with those in the body.
func (s *relayServer) saveTaskSummary(w http.ResponseWriter, r *http.Request) {
	if !s.heldForReport(w, r) {
		return
	}
	summaries, derr := decodeCapped[run.TaskSummary](r.Body, maxRelayElements)
	switch {
	case errors.Is(derr, errTooManyElements):
		writeErr(w, http.StatusRequestEntityTooLarge, derr.Error())
		return
	case derr != nil:
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
	s.writeJSONStatus(w, http.StatusOK, v)
}

// writeJSONStatus writes v as a JSON response with the given status, for the one call that creates
// something and answers 201.
func (s *relayServer) writeJSONStatus(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		s.internal(w, "marshal response", err)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
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
