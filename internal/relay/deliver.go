package relay

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/handoff"
	"github.com/kordloom/switchtender/internal/run"
)

// SecretOpener opens one claimed run's secrets on the control node, for the relay to seal to the
// worker pool that claimed it. The dispatcher satisfies it, so the set opened is exactly the set an
// executor with database access would open for the same run.
type SecretOpener interface {
	// NeedsSecrets reports whether r carries any secret its executor would open.
	NeedsSecrets(ctx context.Context, r *run.Run) bool
	// OpenSecrets opens them. It returns the payload and a release for anything the opening minted,
	// nil when nothing was, which the relay runs when the claim ends.
	OpenSecrets(ctx context.Context, r *run.Run) (*handoff.Payload, func(), error)
}

// PlanSealer seals the plan file a worker's plan saved, with the control node's key, so the apply
// proposed from it carries the plan sealed at rest. The dispatcher satisfies it.
type PlanSealer interface {
	// SealPlanFile seals plan for storage on the apply that carries it out.
	SealPlanFile(plan []byte) (string, error)
}

// WithPlanSealer lets the control node accept the plan file a worker's plan saved and seal it onto
// the apply it proposes. Without one, a plan-gated apply cannot complete on a relay worker.
func WithPlanSealer(p PlanSealer) HandlerOption {
	return func(s *relayServer) { s.planSealer = p }
}

// WithSecretOpener lets the control node deliver a claimed run's secrets to the worker that claimed
// it, sealed to the delivery key the worker's pool registered. A pool that registered no key, or is
// not bound to explicit queues, is still refused every secret, with the reason.
func WithSecretOpener(o SecretOpener) HandlerOption {
	return func(s *relayServer) { s.opener = o }
}

// deliveryOpenTimeout bounds how long the control node spends opening one run's secrets at claim.
// It sits inside the worker's own request timeout, so a slow secret source becomes a refusal the
// worker fails the run with, rather than a claim answer that never arrives and a lease left to
// expire.
const deliveryOpenTimeout = 20 * time.Second

// claimResponse is a claim answer: the leased run, with what the control node sealed for the worker
// beside it. The run's fields stay at the top level, so the answer to a run needing no secret is
// the same bytes it always was, and a worker that predates delivery reads the run it always read.
type claimResponse struct {
	// Run is the leased run. It is embedded so its fields stay at the top level of the answer.
	*run.Run
	// Delivery carries the run's sealed secrets, or why none were sent. Nil when the run needs none.
	Delivery *claimDelivery `json:"relay_secret_delivery,omitempty"`
}

// claimDelivery is the sealed-delivery part of a claim answer.
type claimDelivery struct {
	// Sealed is the run's secrets sealed to the claiming pool's key, nil when they were not sent.
	Sealed *handoff.Envelope `json:"sealed,omitempty"`
	// Refused says why the control node sent nothing for a run that needs secrets.
	Refused string `json:"refused,omitempty"`
}

// deliver opens, seals, and records the secrets of a run a worker just claimed, or says why it will
// not. It returns nil for a run that needs no secret.
//
// Every refusal is fail closed: nothing secret leaves, and the worker fails the run with the reason
// instead of executing it without its credentials. The order is open, seal, record, send. A
// delivery the chain could not record is not sent, because the record of which worker received
// which credentials is the point of sending them sealed rather than not at all.
func (s *relayServer) deliver(ctx context.Context, pool *Pool, owner string, leased *run.Run) *claimDelivery {
	if s.opener == nil || !s.opener.NeedsSecrets(ctx, leased) {
		return nil
	}
	name := ""
	if pool != nil {
		name = pool.Name
	}
	refuse := func(reason string) *claimDelivery {
		s.log.Warn("relay: a claimed run's secrets were not delivered", zap.String("run_id", leased.ID),
			zap.String("pool", name), zap.String("reason", reason))
		return &claimDelivery{Refused: reason}
	}
	switch {
	case pool == nil || pool.key == nil:
		return refuse(fmt.Sprintf("worker pool %q has registered no delivery key, so the control "+
			"node did not send this run's credentials or secret answers to it. Register one with "+
			"delivery_key in the worker pool file, or run this on a queue a worker with database "+
			"access serves", name))
	case !pool.deliversSecrets():
		return refuse(fmt.Sprintf("worker pool %q is not bound to explicit queues, so the control "+
			"node sends it no secrets", name))
	case leased.ClaimSecret == "":
		return refuse("this claim carries no lease to bind a delivery to, so nothing was sent")
	case s.audits == nil:
		return refuse("this control node keeps no audit trail, so it cannot record which worker " +
			"received which credentials, and sends none")
	}
	openCtx, cancel := context.WithTimeout(ctx, deliveryOpenTimeout)
	defer cancel()
	payload, release, err := s.opener.OpenSecrets(openCtx, leased)
	if err != nil {
		return refuse("the control node could not open this run's secrets: " + err.Error())
	}
	defer payload.Wipe()
	binding := handoff.Binding{RunID: leased.ID, Lease: leased.ClaimSecret, Owner: owner}
	env, err := handoff.Seal(pool.key, pool.Name, binding, payload)
	if err != nil {
		releaseLater(release)
		return refuse("the control node could not seal this run's secrets: " + err.Error())
	}
	if err := s.recordDelivery(ctx, pool, owner, leased.ID, env.KeyID, payload); err != nil {
		env.Wipe()
		releaseLater(release)
		s.log.Error("relay: record a secret delivery: "+err.Error(), zap.String("run_id", leased.ID))
		return refuse("the delivery could not be recorded in the audit trail, so nothing was sent")
	}
	s.held.hold(leased.ID, leased.ClaimSecret, release)
	return &claimDelivery{Sealed: env}
}

// recordDelivery writes which pool and worker received which credential ids and secret answer
// names for a run, and under which key, as one chain entry. Values never enter it. Unlike the other
// relay records it is fail closed: the caller sends nothing it could not record.
func (s *relayServer) recordDelivery(ctx context.Context, pool *Pool, owner, runID, keyID string,
	p *handoff.Payload) error {
	entry := &audit.Entry{
		ID: audit.NewID(), Actor: "pool:" + pool.Name + " worker:" + owner, ActorType: actorTypeWorker,
		Method: relayMethod, Path: DeliveryPath(runID, keyID, p.DeliveredIDs(), p.AnswerNames()),
	}
	return s.audits.Append(ctx, entry)
}

// DeliveryPath is the audit path recording one sealed delivery: the run, the pool key it was sealed
// to, the credential ids, and the secret answer names. Each name is path escaped, so a name cannot
// forge another segment of the record.
func DeliveryPath(runID, keyID string, credentialIDs, answerNames []string) string {
	path := "/relay/delivered/" + url.PathEscape(runID) + "/key/" + url.PathEscape(keyID)
	if len(credentialIDs) > 0 {
		path += "/credentials/" + escapeAll(credentialIDs)
	}
	if len(answerNames) > 0 {
		path += "/answers/" + escapeAll(answerNames)
	}
	return path
}

// escapeAll path escapes each name and joins them with commas.
func escapeAll(names []string) string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = url.PathEscape(n)
	}
	return strings.Join(out, ",")
}

// releaseLater runs a release off the request path. A release revokes minted secrets at their
// engine, which is a network call with its own deadline, and a worker's report should not wait on
// it.
func releaseLater(release func()) {
	if release != nil {
		go release()
	}
}

const (
	// heldSweepInterval is how often the control node checks whether a held delivery's claim has
	// ended without a terminal report reaching it: a worker that died, a run the janitor requeued or
	// interrupted, a finish another replica recorded.
	heldSweepInterval = 30 * time.Second
	// heldSweepTimeout bounds one timed sweep's reads, so a store that hangs delays the next sweep
	// rather than stopping them.
	heldSweepTimeout = 20 * time.Second
)

// heldReleases keeps, per run, the release of whatever opening its secrets minted, such as a Vault
// dynamic secret's lease, until the claim it was delivered for ends.
//
// An executor with database access revokes those itself when its run ends. A relay worker cannot:
// the lease was minted here, against an engine the worker may not reach, with credentials the
// worker never holds. So the control node revokes it, when the worker reports the run finished,
// when another claim of the same run is delivered, or when a sweep finds the claim gone.
//
// The sweep runs on its own timer for as long as anything is held. It used to run only when a
// later claim reached the same control node, so a pool whose only worker died kept the minted
// secret alive for as long as the control node ran, and a replica whose run finished through
// another replica, or that was drained out of the load balancer, never revoked what it minted. A
// control node that stops or restarts still loses what it held, since the means to revoke lives in
// its memory, and those secrets expire on their own lifetimes, the same as a crashed executor's.
type heldReleases struct {
	// mu guards byRun, lastSweep, and timer.
	mu sync.Mutex
	// byRun maps a run id to the release held for its current claim.
	byRun map[string]heldRelease
	// lastSweep is when the claims behind held releases were last checked.
	lastSweep time.Time
	// store is what a timed sweep reads to tell whether a claim has ended, nil for a set that is
	// only swept when asked.
	store run.Store
	// timer fires the next timed sweep, nil while none is scheduled.
	timer *time.Timer
}

// heldRelease is one run's held release.
type heldRelease struct {
	// lease is the hash of the claim lease the delivery was made under, so a later claim is told
	// apart without keeping the lease itself.
	lease [32]byte
	// release hands back what the opening minted.
	release func()
}

// hold keeps release until the run's claim ends. A release already held for an earlier claim of the
// same run is run now, since that claim is over.
func (h *heldReleases) hold(runID, lease string, release func()) {
	if release == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.byRun == nil {
		h.byRun = map[string]heldRelease{}
	}
	if prior, ok := h.byRun[runID]; ok {
		releaseLater(prior.release)
	}
	h.byRun[runID] = heldRelease{lease: sha256.Sum256([]byte(lease)), release: release}
	h.scheduleLocked()
}

// scheduleLocked arms the next timed sweep when something is held, none is armed, and the set has
// a store to sweep against. The caller holds mu.
func (h *heldReleases) scheduleLocked() {
	if h.store == nil || h.timer != nil || len(h.byRun) == 0 {
		return
	}
	h.timer = time.AfterFunc(heldSweepInterval, h.timedSweep)
}

// timedSweep runs one sweep from the timer and arms the next while anything is still held.
func (h *heldReleases) timedSweep() {
	ctx, cancel := context.WithTimeout(context.Background(), heldSweepTimeout)
	h.sweep(ctx, h.store, time.Now())
	cancel()
	h.mu.Lock()
	defer h.mu.Unlock()
	h.timer = nil
	h.scheduleLocked()
}

// releaseRun runs and forgets the release held for a run, if any.
func (h *heldReleases) releaseRun(runID string) {
	h.mu.Lock()
	held, ok := h.byRun[runID]
	delete(h.byRun, runID)
	h.mu.Unlock()
	if ok {
		releaseLater(held.release)
	}
}

// sweep releases what is held for any run whose claim has ended without a terminal report: the run
// is gone, finished, or claimed again under another lease. It checks at most every
// heldSweepInterval.
func (h *heldReleases) sweep(ctx context.Context, store run.Store, now time.Time) {
	h.mu.Lock()
	if len(h.byRun) == 0 || now.Sub(h.lastSweep) < heldSweepInterval {
		h.mu.Unlock()
		return
	}
	h.lastSweep = now
	ids := make([]string, 0, len(h.byRun))
	for id := range h.byRun {
		ids = append(ids, id)
	}
	h.mu.Unlock()
	for _, id := range ids {
		stored, err := store.Get(ctx, id)
		switch {
		case errors.Is(err, run.ErrNotFound):
		case err != nil:
			continue
		case stored.Status.Terminal():
		default:
			h.mu.Lock()
			held, ok := h.byRun[id]
			h.mu.Unlock()
			if !ok || held.lease == sha256.Sum256([]byte(stored.ClaimSecret)) {
				continue
			}
		}
		h.releaseRun(id)
	}
}
