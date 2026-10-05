// Package handoff seals one run's opened secrets to a relay worker pool's delivery key on the
// control node, and opens them in memory on the worker that claimed the run.
//
// A relay worker has no credential store and no encryption key, so before this it could not execute
// a run that needed one. Each pool that opts in registers an X25519 public key. When one of its
// workers claims a run, the control node opens exactly that run's secrets, seals them to the pool's
// key, and binds the seal to the run, the claim's lease, and the worker that asserted it, so the
// result opens for that claim and nothing else.
//
// The construction is HPKE (RFC 9180) in base mode, with DHKEM(X25519, HKDF-SHA256), HKDF-SHA256,
// and ChaCha20-Poly1305, from the Go standard library's crypto/hpke. It was chosen over NaCl's
// anonymous sealed box because HPKE takes associated data: the binding is authenticated by the AEAD
// itself, so a delivery presented for the wrong run, lease, or worker fails to decrypt rather than
// decrypting and being checked afterward. Every seal draws a fresh ephemeral key, the suite is a
// published standard with a security analysis behind it, and nothing outside the standard library
// is needed. A worker in another pool holds another private key, so it cannot open what was sealed
// to this one.
package handoff

import (
	"crypto/hpke"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strconv"
)

const (
	// Version is the envelope format Seal writes and the only one Open reads.
	Version = 1
	// Suite names the HPKE ciphersuite an envelope is sealed with. Open refuses any other, so an
	// envelope cannot talk a worker into a weaker one.
	Suite = "hpke-base-x25519-hkdf-sha256-chacha20poly1305"
	// aadLabel opens the associated data, separating it from anything else the same fields could
	// be hashed or encrypted under.
	aadLabel = "switchtender relay delivery binding v1"
	// maxSealed bounds the ciphertext Open will try, so a malformed envelope cannot ask a worker to
	// decrypt an unbounded buffer.
	maxSealed = 8 << 20
)

// info is HPKE's context string: it binds every seal to this purpose and this version.
var info = []byte("switchtender relay run secret delivery v1")

// suite returns the KDF and AEAD the envelope is sealed with. The KEM comes with the key.
func suite() (hpke.KDF, hpke.AEAD) {
	return hpke.HKDFSHA256(), hpke.ChaCha20Poly1305()
}

// Binding is what a delivery is sealed for: one claim of one run by one worker. Each field is part
// of the authenticated data, so a delivery opens only under the binding it was sealed with.
type Binding struct {
	// RunID is the run the secrets belong to. Sealing it in is what stops a delivery for one run
	// being handed to another run's execution.
	RunID string
	// Lease is the per-claim capability the control node minted when the worker claimed the run.
	// It is secret, so only its hash enters the associated data, and it changes on every claim, so
	// a delivery from an earlier claim of the same run does not open for a later one.
	Lease string
	// Owner is the lease name the claiming worker asserted, as the control node recorded it.
	Owner string
}

// Envelope is a sealed delivery as it crosses the wire. Everything but the ciphertext is public,
// and all of it is authenticated.
type Envelope struct {
	// Version is the envelope format.
	Version int `json:"version"`
	// Suite names the HPKE ciphersuite.
	Suite string `json:"suite"`
	// KeyID names the pool key the payload was sealed to.
	KeyID string `json:"key_id"`
	// DeliveryID is random per delivery, so a worker can refuse to open the same delivery twice.
	DeliveryID string `json:"delivery_id"`
	// Pool names the worker pool the delivery was sealed for.
	Pool string `json:"pool"`
	// RunID is the run the delivery was sealed for.
	RunID string `json:"run_id"`
	// Enc is HPKE's encapsulated key: the sender's ephemeral public key.
	Enc []byte `json:"enc"`
	// Ciphertext is the sealed payload.
	Ciphertext []byte `json:"ciphertext"`
}

// Wipe drops the envelope's ciphertext. It is called once a delivery has been opened or will never
// be, so no copy of it outlives the run.
func (e *Envelope) Wipe() {
	if e == nil {
		return
	}
	clear(e.Enc)
	clear(e.Ciphertext)
	e.Enc, e.Ciphertext = nil, nil
}

// Seal encrypts p to pub for one claim, recording pool as the pool it was sealed for.
func Seal(pub *PublicKey, pool string, b Binding, p *Payload) (*Envelope, error) {
	switch {
	case pub == nil:
		return nil, fmt.Errorf("%w: no delivery key", ErrSeal)
	case p == nil:
		return nil, fmt.Errorf("%w: no payload", ErrSeal)
	case b.RunID == "" || b.Lease == "" || b.Owner == "":
		return nil, fmt.Errorf("%w: a delivery is bound to a run, a lease, and a worker", ErrSeal)
	case p.RunID != b.RunID:
		return nil, fmt.Errorf("%w: the payload is for run %s, not %s", ErrSeal, p.RunID, b.RunID)
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return nil, fmt.Errorf("%w: delivery id: %w", ErrSeal, err)
	}
	env := &Envelope{
		Version: Version, Suite: Suite, KeyID: pub.ID(), DeliveryID: hex.EncodeToString(id[:]),
		Pool: pool, RunID: b.RunID,
	}
	plain, err := marshalPayload(p)
	defer clear(plain)
	if err != nil {
		return nil, err
	}
	kdf, aead := suite()
	enc, sender, err := hpke.NewSender(pub.key, kdf, aead, info)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrSeal, err)
	}
	ct, err := sender.Seal(associatedData(env, b), plain)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrSeal, err)
	}
	env.Enc, env.Ciphertext = enc, ct
	return env, nil
}

// Open decrypts e with the matching key from ring, for the claim b describes, and returns the
// payload. It fails closed: an envelope of another format or suite, sealed to a key the ring does
// not hold, or sealed for any other run, lease, or worker does not open, and nothing of what it
// carries is returned.
func Open(ring *KeyRing, b Binding, e *Envelope) (*Payload, error) {
	switch {
	case e == nil:
		return nil, fmt.Errorf("%w: no envelope", ErrEnvelope)
	case e.Version != Version:
		return nil, fmt.Errorf("%w: format version %d, this worker opens %d", ErrEnvelope,
			e.Version, Version)
	case e.Suite != Suite:
		return nil, fmt.Errorf("%w: suite %q, this worker opens %q", ErrEnvelope, e.Suite, Suite)
	case len(e.Ciphertext) > maxSealed:
		return nil, fmt.Errorf("%w: %d bytes is larger than any delivery", ErrEnvelope,
			len(e.Ciphertext))
	case b.RunID == "" || b.Lease == "" || b.Owner == "":
		return nil, fmt.Errorf("%w: the worker holds no claim to open it under", ErrBinding)
	case e.RunID != b.RunID:
		return nil, fmt.Errorf("%w: it was sealed for run %s, and this is run %s", ErrBinding,
			e.RunID, b.RunID)
	}
	key := ring.key(e.KeyID)
	if key == nil {
		return nil, fmt.Errorf("%w: it names key %s and this worker holds %v", ErrUnknownKey,
			e.KeyID, ring.IDs())
	}
	kdf, aead := suite()
	recipient, err := hpke.NewRecipient(e.Enc, key.key, kdf, aead, info)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrBinding, err)
	}
	plain, err := recipient.Open(associatedData(e, b), e.Ciphertext)
	defer clear(plain)
	if err != nil {
		return nil, fmt.Errorf("%w: it was sealed for another claim, another worker, or was "+
			"altered", ErrBinding)
	}
	p, err := unmarshalPayload(plain)
	if err != nil {
		return nil, err
	}
	if p.RunID != b.RunID {
		p.Wipe()
		return nil, fmt.Errorf("%w: the payload names run %s", ErrBinding, p.RunID)
	}
	return p, nil
}

// associatedData is the authenticated binding of one delivery: the envelope's public fields and the
// claim it is for. Each field is written with its length, so no two different bindings encode to
// the same bytes. The lease enters only as its hash, since it is a secret and the associated data
// is not.
func associatedData(e *Envelope, b Binding) []byte {
	lease := sha256.Sum256([]byte(b.Lease))
	fields := []string{
		aadLabel, strconv.Itoa(e.Version), e.Suite, e.KeyID, e.DeliveryID, e.Pool,
		b.RunID, b.Owner, hex.EncodeToString(lease[:]),
	}
	var out []byte
	for _, f := range fields {
		out = binary.BigEndian.AppendUint32(out, uint32(len(f)))
		out = append(out, f...)
	}
	return out
}
