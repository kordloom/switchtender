// Package witnessclient is the install's side of a push witness: it keeps the enrollment, pushes
// every appended span beat with the tree root at its size and a consistency proof from the size the
// witness last acknowledged, and stores the countersignatures the witness answers with so bundles
// drawn from the chain later carry them. An outage queues the pushes in order and costs nothing but
// time, a witness restored from an older copy is proved to again from the size it says it holds,
// and only a signed refusal with findings is ever reported as one.
package witnessclient

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/identity"
	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/spanbeat"
	"github.com/kordloom/switchtender/witness"
)

// maxReprove bounds how many times one push is proved again from a size the witness names before
// the client waits for the next beat. A witness that names a new size on every answer is another
// writer racing this one, and the next beat settles it.
const maxReprove = 2

// Client pushes each appended beat to the witness an install is enrolled with and keeps the
// countersignatures it answers with. Beat is its spanbeat.AnchorFunc. It is safe for concurrent
// use: beats are pushed one at a time, in order, and Status may be read while one is in flight.
type Client struct {
	// path is the enrollment file. It is read again when an answer is signed by a key the held
	// enrollment does not list, so a key list refreshed by witness enroll --refresh-keys takes effect
	// without a restart.
	path string
	// id is the producer identity every push is signed with.
	id identity.Identity
	// chain is the audit chain the pushed trees are computed over.
	chain Chain
	// attestations keeps every countersignature the witness answers with.
	attestations audit.AttestationStore
	// http carries every request to the witness.
	http *http.Client
	// log records outages, refusals, notes, and beats that were dropped or stale.
	log *zap.Logger
	// clock reads the time status entries are stamped with.
	clock func() time.Time
	// run serializes beats, so one push is in flight at a time and the queue drains in order. The
	// cache, the queue, and base are touched only under it.
	run sync.Mutex
	// cache holds the leaf hash of every chain entry read so far.
	cache leafCache
	// queue holds the pushes not yet answered, oldest first.
	queue *queue
	// base is the tree size the next push proves from: the size the witness last countersigned or
	// said it holds. It is zero until the witness answers, and a witness holding more says so.
	base int64
	// mu guards enr, status, and down.
	mu sync.Mutex
	// enr is the enrollment pushes are made under.
	enr *Enrollment
	// status is the account Status returns.
	status Status
	// down is whether the last push found the witness unreachable, so an outage is logged once
	// when it begins and once when it ends rather than on every beat.
	down bool
}

// Option configures a Client.
type Option func(*Client)

// WithHTTPClient sends every request through c instead of GuardedClient.
func WithHTTPClient(c *http.Client) Option {
	return func(cl *Client) { cl.http = c }
}

// WithClock replaces the clock status entries are stamped with.
func WithClock(now func() time.Time) Option {
	return func(cl *Client) { cl.clock = now }
}

// New returns a client pushing under the enrollment at path for the producer identity id, over the
// chain chain, storing countersignatures in attestations. cadence is the span beat cadence, which
// sizes the queue to one day of beats. An enrollment that is missing, out of form, or not id's is
// an error, so a key changed under an enrollment never pushes. It panics on a nil chain or store, a
// programming error.
func New(path string, id identity.Identity, chain Chain, attestations audit.AttestationStore,
	cadence time.Duration, log *zap.Logger, opts ...Option) (*Client, error) {
	if chain == nil {
		panic("witnessclient: New needs the audit chain")
	}
	if attestations == nil {
		panic("witnessclient: New needs an attestation store")
	}
	enr, err := Load(path)
	if err != nil {
		return nil, err
	}
	if err := enr.Bind(id); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if log == nil {
		log = zap.NewNop()
	}
	c := &Client{path: path, id: id, chain: chain, attestations: attestations, log: log,
		clock: time.Now, queue: newQueue(QueueCap(cadence)), enr: enr}
	for _, opt := range opts {
		opt(c)
	}
	if c.http == nil {
		c.http = GuardedClient()
	}
	c.status.QueueCap = c.queue.capacity
	return c, nil
}

// Status returns the client's account of its enrollment and its pushes.
func (c *Client) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.status
	s.Witness, s.WitnessKeyID, s.InstallID = c.enr.Witness, c.enr.Keys.KeyID, c.enr.InstallID
	s.KeyID, s.EnrolledAt = c.enr.KeyID, c.enr.EnrolledAt
	s.LastRefusal, s.LastNote = s.LastRefusal.clone(), s.LastNote.clone()
	return s
}

// Beat pushes the appended beat b after every push still waiting from earlier beats, in order. It
// reads the entries appended since the last beat into the leaf cache, the whole chain on the first,
// fixes the tree root at the beat's size, queues the push, and drains the queue: each push is
// proved at the moment it is sent from the size the witness last acknowledged, so a push dropped
// from a full queue never breaks the next one's proof. What is still waiting after the drain is
// trimmed to the queue's capacity, oldest dropped first.
//
// A witness that cannot be reached leaves the queue as it was and the error is ErrUnreachable. A
// refused or rejected push is dropped from the queue and named in the error, and the pushes after
// it are still sent. Once the witness has answered that the install's license lapsed, Beat pushes
// nothing more and returns nil.
func (c *Client) Beat(ctx context.Context, b spanbeat.AppendedBeat) error {
	c.run.Lock()
	defer c.run.Unlock()
	if c.lapsed() {
		return nil
	}
	if b.Seq < 1 {
		err := fmt.Errorf("%w: a beat at chain position %d", ErrChain, b.Seq)
		c.failed(err)
		return err
	}
	if err := c.cache.extend(ctx, c.chain, c.label(), b.Seq, b.Hash); err != nil {
		c.failed(err)
		return err
	}
	p := pending{Beat: b.Beat, At: b.At.UTC().Format(time.RFC3339Nano), Seq: b.Seq, Head: b.Hash,
		Root: hex.EncodeToString(treeRoot(c.cache.hashes[:b.Seq]))}
	c.queue.add(p)
	err := c.drain(ctx)
	for _, old := range c.queue.trim() {
		c.log.Warn("witness: the push queue is full, so the oldest waiting beat was dropped; the "+
			"witness will note it as a beat it never received",
			zap.Int64("beat", old.Beat), zap.Int64("seq", old.Seq),
			zap.Int64("dropped", c.queue.dropped))
	}
	c.publishQueue()
	return err
}

// drain pushes the waiting beats oldest first until the queue is empty or a push must wait for the
// next beat. It returns every error the pushes met, joined.
func (c *Client) drain(ctx context.Context) error {
	var errs []error
	for {
		p, ok := c.queue.front()
		if !ok {
			return errors.Join(errs...)
		}
		done, err := c.push(ctx, p)
		if err != nil {
			errs = append(errs, err)
			c.failed(err)
		}
		if errors.Is(err, ErrLapsed) {
			c.lapse(err)
			return errors.Join(errs...)
		}
		if !done {
			if errors.Is(err, ErrUnreachable) {
				c.outage(err)
			}
			return errors.Join(errs...)
		}
		c.queue.pop()
		c.publishQueue()
	}
}

// push sends one waiting beat and settles the witness's answer. It reports whether the beat is
// finished with, countersigned or dropped, and the error that says why it was not countersigned. A
// beat that is not finished with stays at the front of the queue for the next beat.
func (c *Client) push(ctx context.Context, p pending) (bool, error) {
	for attempt := 0; ; attempt++ {
		// The witness already said it holds a larger tree, and no proof runs from a larger tree to
		// a smaller one, so the beat is stale without asking again. At the held size itself the
		// push is sent: the witness may hold exactly this push, and answers it again if so.
		if c.base > p.Seq {
			c.stale(p, c.base)
			return true, nil
		}
		sent, err := c.build(p, c.base)
		if err != nil {
			return false, err
		}
		code, answer, raw, err := c.send(ctx, sent)
		if err != nil {
			return false, err
		}
		switch {
		case code == http.StatusOK:
			return c.countersigned(ctx, p, answer)
		case code == http.StatusConflict && answer.HeldSize != nil:
			held := *answer.HeldSize
			c.base = held
			if held >= p.Seq {
				c.stale(p, held)
				return true, nil
			}
			if attempt >= maxReprove {
				return false, fmt.Errorf("%w: the witness named a new tree size on %d answers in a "+
					"row for beat %d, so the push waits for the next beat", ErrRejected,
					attempt+1, p.Beat)
			}
			c.restored(p, held)
		case code == http.StatusConflict && len(answer.Findings) > 0:
			return true, c.refused(p, answer)
		case code == http.StatusBadRequest:
			err := answerError(fmt.Sprintf("beat %d", p.Beat), code, raw)
			c.log.Error("witness: the witness could not check a push, so it was dropped and will "+
				"not be sent again: "+err.Error(), zap.Int64("beat", p.Beat), zap.Int64("seq", p.Seq))
			return true, err
		default:
			return false, answerError(fmt.Sprintf("beat %d", p.Beat), code, raw)
		}
	}
}

// build returns the signed push for p, proving from the tree at size from.
func (c *Client) build(p pending, from int64) (witness.Push, error) {
	if p.Seq > c.cache.size() {
		return witness.Push{}, fmt.Errorf("%w: beat %d is at entry %d and the chain read so far "+
			"holds %d", ErrChain, p.Beat, p.Seq, c.cache.size())
	}
	proof, err := consistencyProof(from, c.cache.hashes[:p.Seq])
	if err != nil {
		return witness.Push{}, err
	}
	push := witness.Push{InstallID: c.label(), BeatNumber: p.Beat, At: p.At, Seq: p.Seq,
		Head: p.Head, Size: p.Seq, Root: p.Root, FromSize: from, Proof: hexHashes(proof)}
	if err := witness.SignPush(&push, c.id); err != nil {
		return witness.Push{}, fmt.Errorf("%w: %w", ErrRejected, err)
	}
	return push, nil
}

// send posts one push and returns the status, the answer read as a push answer when it is one, and
// the raw body. A witness that does not answer, or answers 5xx, is ErrUnreachable.
func (c *Client) send(ctx context.Context, p witness.Push) (int, *witness.PushAnswer, []byte, error) {
	body, err := json.Marshal(p)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("%w: encode the push: %w", ErrRejected, err)
	}
	enr := c.enrollment()
	code, raw, err := exchange(ctx, c.http, http.MethodPost, enr.Witness+enr.PushPath, body)
	if err != nil {
		return code, nil, raw, err
	}
	answer := &witness.PushAnswer{}
	if code == http.StatusOK || code == http.StatusConflict {
		if err := json.Unmarshal(raw, answer); err != nil {
			return code, nil, raw, fmt.Errorf("%w: the witness answered %d with something that is "+
				"not a push answer: %w", ErrRejected, code, err)
		}
	}
	return code, answer, raw, nil
}

// countersigned settles a 200: the answer must name the pushed coordinate, carry both
// countersignatures, each verifying over what it names and signed by a key the enrollment holds as
// the witness's word at the time it signed, and both are stored before the beat is finished with.
// An answer that fails any of that leaves the beat queued: the witness holds it, so the retry is
// answered with the same bytes, which verify once the key list that holds their key is in place.
func (c *Client) countersigned(ctx context.Context, p pending, a *witness.PushAnswer) (bool, error) {
	enr := c.enrollment()
	if a.InstallID != enr.InstallID || a.KeyID != enr.KeyID || a.Beat != p.Beat ||
		a.Seq != p.Seq || a.Head != p.Head || a.Size != p.Seq || a.Root != p.Root {
		return false, fmt.Errorf("%w: the witness countersigned beat %d at seq %d with root %s, "+
			"and the push was beat %d at seq %d with root %s", ErrRejected, a.Beat, a.Seq, a.Root,
			p.Beat, p.Seq, p.Root)
	}
	if a.ClaimAttestation == nil || a.HeadAttestation == nil {
		return false, fmt.Errorf("%w: the witness answered 200 for beat %d without both "+
			"countersignatures", ErrRejected, p.Beat)
	}
	if err := witness.VerifyClaimAttestation(p.Head, *a.ClaimAttestation); err != nil {
		return false, fmt.Errorf("%w: beat %d: %w", ErrRejected, p.Beat, err)
	}
	if err := witness.VerifyHeadAttestation(p.Root, p.Seq, *a.HeadAttestation); err != nil {
		return false, fmt.Errorf("%w: beat %d: %w", ErrRejected, p.Beat, err)
	}
	for _, cs := range []witness.Countersignature{*a.ClaimAttestation, *a.HeadAttestation} {
		if err := c.trusts(cs); err != nil {
			return false, err
		}
	}
	enr = c.enrollment()
	claim := attestation(*a.ClaimAttestation, enr.Keys.KeyID)
	claim.Kind, claim.Seq, claim.Link = audit.AttestationClaim, p.Seq, p.Head
	head := attestation(*a.HeadAttestation, enr.Keys.KeyID)
	head.Kind, head.Seq, head.Size, head.Root = audit.AttestationHead, p.Seq, p.Seq, p.Root
	for _, w := range []*audit.WitnessAttestation{claim, head} {
		if err := c.attestations.SaveAttestation(ctx, w); errors.Is(err, audit.ErrAttestationConflict) {
			c.log.Error("witness: a different countersignature is already stored for this "+
				"coordinate, and the stored one is kept: "+err.Error(), zap.Int64("seq", w.Seq),
				zap.String("kind", w.Kind))
		} else if err != nil {
			return false, fmt.Errorf("store the witness countersignature for beat %d: %w", p.Beat, err)
		}
	}
	c.base = p.Seq
	c.reachable()
	c.mu.Lock()
	c.status.LastBeat, c.status.LastSeq = p.Beat, p.Seq
	c.status.LastCountersignedAt = a.ClaimAttestation.At
	c.mu.Unlock()
	if len(a.Findings) > 0 {
		c.noted(p, a.Findings)
	}
	return true, nil
}

// attestation returns the stored form of a countersignature, its members kept exactly as signed,
// recorded against the witness key the enrollment names as current.
func attestation(cs witness.Countersignature, pinned string) *audit.WitnessAttestation {
	return &audit.WitnessAttestation{ID: audit.NewAttestationID(), KeyID: cs.KeyID,
		PublicKey: cs.PublicKey, Role: cs.Role, At: cs.At, Sig: cs.Sig, Witness: pinned}
}

// trusts checks that cs is signed by a key the enrollment lists as the witness's word at the time
// cs names. When the held list does not hold it, the enrollment file is read again and its list is
// taken only when witness.VerifyKeyList accepts it in place of the held one, so a list refreshed on
// disk takes effect and a list edited on disk never does.
func (c *Client) trusts(cs witness.Countersignature) error {
	enr := c.enrollment()
	if keyHeld(enr.Keys, cs) {
		return nil
	}
	if fresh, err := Load(c.path); err == nil && fresh.Bind(c.id) == nil &&
		fresh.Witness == enr.Witness && witness.VerifyKeyList(enr.Keys, fresh.Keys) == nil &&
		keyHeld(fresh.Keys, cs) {
		c.mu.Lock()
		c.enr = fresh
		c.mu.Unlock()
		c.log.Info("witness: took the refreshed witness key list", zap.String("current",
			fresh.Keys.KeyID))
		return nil
	}
	return fmt.Errorf("%w: the witness countersigned with key %s, which this install's enrollment "+
		"does not hold as the witness's key at %s. If the witness rotated its key, run switchtender "+
		"witness enroll --refresh-keys", ErrWitnessKey, cs.KeyID, cs.At)
}

// keyHeld reports whether keys lists cs's key, with the same public key, as the witness's word at
// the time cs names.
func keyHeld(keys witness.WitnessKeys, cs witness.Countersignature) bool {
	at, err := time.Parse(time.RFC3339Nano, cs.At)
	if err != nil {
		return false
	}
	k, ok := keys.KeyAt(cs.KeyID, at)
	return ok && k.PublicKey == cs.PublicKey
}

// refused records a refusal: the witness held the push against its memory and it contradicted what
// the witness remembers. Each finding is logged by its class, refusals at error and notes at warn,
// with its detail verbatim, and the answer is kept as the last refusal.
func (c *Client) refused(p pending, a *witness.PushAnswer) error {
	for _, f := range a.Findings {
		msg := "witness: install " + c.label() + " " + f.Class + " " + f.Kind + ": " + f.Detail
		fields := []zap.Field{zap.Int64("beat", p.Beat), zap.Int64("seq", p.Seq)}
		if f.Class == witness.ClassNote {
			c.log.Warn(msg, fields...)
		} else {
			c.log.Error(msg, fields...)
		}
	}
	c.mu.Lock()
	c.status.LastRefusal = c.testimony(p, a.Findings)
	c.mu.Unlock()
	return fmt.Errorf("%w: beat %d at seq %d: %s", ErrRefused, p.Beat, p.Seq, a.Error)
}

// noted logs the notes a countersigned answer carried, at warn, and keeps the answer as the last
// note.
func (c *Client) noted(p pending, found []witness.RecordedFinding) {
	for _, f := range found {
		c.log.Warn("witness: install "+c.label()+" "+f.Class+" "+f.Kind+": "+f.Detail,
			zap.Int64("beat", p.Beat), zap.Int64("seq", p.Seq))
	}
	c.mu.Lock()
	c.status.LastNote = c.testimony(p, found)
	c.mu.Unlock()
}

// testimony returns the record of an answer about p that carried findings. The caller holds mu.
func (c *Client) testimony(p pending, found []witness.RecordedFinding) *Testimony {
	return &Testimony{At: c.clock().UTC().Format(time.RFC3339), Beat: p.Beat, Seq: p.Seq,
		Findings: append([]witness.RecordedFinding(nil), found...)}
}

// stale records a beat the witness already holds a tree at or past: a replica's superseded beat or
// a chain cut back, which one push cannot tell apart. It is dropped and never sent again, since no
// proof runs from a larger tree to a smaller one.
func (c *Client) stale(p pending, held int64) {
	c.mu.Lock()
	c.status.Stale++
	c.mu.Unlock()
	c.log.Warn("witness: the witness holds the tree at a size at or past this beat's, so the beat "+
		"was dropped and will not be sent again", zap.Int64("beat", p.Beat),
		zap.Int64("seq", p.Seq), zap.Int64("held_size", held))
}

// restored logs a push the witness asked to be proved again from a smaller size than this install
// last proved from, which is a witness whose memory was restored from an earlier copy, or a first
// push since this process started.
func (c *Client) restored(p pending, held int64) {
	c.log.Info("witness: proving again from the tree size the witness holds",
		zap.Int64("beat", p.Beat), zap.Int64("held_size", held))
}

// failed records err as the last error.
func (c *Client) failed(err error) {
	c.mu.Lock()
	c.status.LastError = err.Error()
	c.status.LastErrorAt = c.clock().UTC().Format(time.RFC3339)
	c.mu.Unlock()
}

// outage logs the start of an outage once.
func (c *Client) outage(err error) {
	c.mu.Lock()
	began := !c.down
	c.down = true
	queued := c.queue.size()
	c.mu.Unlock()
	if began {
		c.log.Warn("witness: the witness cannot be reached, so beats are queued in order and "+
			"retried on every beat: "+err.Error(), zap.Int("queued", queued))
	}
}

// reachable logs the end of an outage once.
func (c *Client) reachable() {
	c.mu.Lock()
	ended := c.down
	c.down = false
	c.mu.Unlock()
	if ended {
		c.log.Info("witness: the witness answers again, so the queued beats are being sent")
	}
}

// lapse stops the client for good: the witness countersigns nothing more from this install, so the
// queue is emptied and no later beat is pushed.
func (c *Client) lapse(err error) {
	c.queue.clear()
	c.mu.Lock()
	c.status.Lapsed = true
	c.mu.Unlock()
	c.log.Error("witness: the witness says this install's license and its grace have ended, so no " +
		"more beats will be pushed. Renew the license, enroll again, and restart: " + err.Error())
}

// lapsed reports whether the witness has said the install's license lapsed.
func (c *Client) lapsed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.status.Lapsed
}

// publishQueue copies the queue's length and drop count into the status.
func (c *Client) publishQueue() {
	c.mu.Lock()
	c.status.Queue, c.status.Dropped = c.queue.size(), c.queue.dropped
	c.mu.Unlock()
}

// enrollment returns the enrollment pushes are made under.
func (c *Client) enrollment() *Enrollment {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.enr
}

// label returns the install id the tree's leaves and every push name.
func (c *Client) label() string { return c.enrollment().InstallID }
