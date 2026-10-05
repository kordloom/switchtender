package federation

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/go-jose/go-jose/v4"

	"github.com/kordloom/switchtender/internal/credential"
)

// rsaBits is the modulus size of a generated signing key. RS256 over a 2048 bit key is what every
// cloud that federates an external OpenID Connect issuer accepts, which an elliptic curve or
// Ed25519 key is not: Microsoft Entra verifies RSA only.
const rsaBits = 2048

// Key is one signing key of the federation issuer. The private half is sealed under the install's
// encryption key before it reaches a store, and the public half is kept beside it in the clear, so
// the public key set can be served without opening any private material.
//
// It is a key of its own. The audit chain's producer key signs evidence and the license key signs
// entitlements, and neither may be reachable from a token a cloud verifies: a key that signs run
// identity is rotated on its own schedule, and its compromise must not let anyone forge evidence.
//
// A key records four times. It is published from CreatedAt, signs from ActivatedAt until RetiredAt,
// and leaves the public key set at RemovedAt, when its private half is erased. A time in the future
// is a scheduled one: a normal rotation writes the whole schedule at once, so every process on the
// database reads the same plan from the store and nothing has to fire for a rotation to complete.
type Key struct {
	// ID is the key id, the RFC 7638 thumbprint of the public key, carried as kid in every token.
	ID string `json:"id"`
	// Algorithm is the JWS algorithm the key signs with, RS256.
	Algorithm string `json:"algorithm"`
	// PublicKey is the PEM encoded public key, served in the public key set.
	PublicKey string `json:"public_key"`
	// Sealed is the PKCS #8 private key, PEM encoded and sealed with the credential sealer, empty
	// once the key is removed. It never serializes.
	Sealed string `json:"-"`
	// CreatedAt is when the key was generated, which is also when it was first published.
	CreatedAt time.Time `json:"created_at"`
	// ActivatedAt is when the key starts signing, nil for a key that never signs. A first key and an
	// emergency key activate as they are created, and a normal rotation's key RotationDelay later.
	ActivatedAt *time.Time `json:"activated_at,omitempty"`
	// RetiredAt is when the key stops signing, nil while no rotation has replaced it.
	RetiredAt *time.Time `json:"retired_at,omitempty"`
	// RemovedAt is when the key leaves the public key set, nil while no rotation has scheduled that.
	// A normal rotation sets it RetiredKeyGrace after RetiredAt, and an emergency rotation sets it to
	// the moment of the rotation.
	RemovedAt *time.Time `json:"removed_at,omitempty"`
}

// KeyStore persists federation signing keys. Implementations must be safe for concurrent use and
// store the sealed private key exactly as given.
//
// Every process sharing a store signs with and publishes the same keys, so a decision about them is
// made once for all of those processes. Change runs a decision under a lock every one of them
// takes, on the keys as they stand once it holds that lock, and Now is the clock every one of them
// judges the keys by. A store also refuses any write that would undo a removal, whoever makes it, so a key
// once removed and erased never signs or returns to the published set, not even from a process
// acting on a copy it read before the removal. A store keeps every key it was given, removed ones
// included, as the record of when each was trusted, and offers no way to delete one.
type KeyStore interface {
	// Save inserts or replaces the key identified by k.ID. It writes nothing and returns
	// ErrKeyRemoved when the write would give back a private half the store holds erased, or clear
	// or postpone a removal the store holds. Outside a change it is a change of its own.
	Save(ctx context.Context, k *Key) error
	// List returns every key ordered by creation time, oldest first.
	List(ctx context.Context) ([]*Key, error)
	// Change runs fn as one atomic change to the keys, serialized with every other change on the
	// same keys, from this process or any other sharing the store. List, Save, and Now called with
	// the context fn receives read, write, and tell time inside the change, and see the keys as
	// they stand once the change holds its lock. When fn returns an error, nothing it wrote is kept.
	// A Change given a context that is already inside a change of the same store joins it.
	Change(ctx context.Context, fn func(ctx context.Context) error) error
	// Now returns the store's clock, the one every process sharing the store judges keys by, which
	// for a database is the database server's clock. Inside a change it is the reading taken once
	// the change held its lock. A store with no clock of its own returns the zero time, and the
	// caller judges by its own clock.
	Now(ctx context.Context) (time.Time, error)
}

// CheckSave reports whether next may replace stored, the key a store holds under the same id, nil
// when it holds none. Every store calls it inside the change a write belongs to, and refuses the
// write with ErrKeyRemoved when it would undo a removal: give back a private half the store holds
// erased, or clear or postpone a removal the store holds. A process that read a key before an
// emergency rotation removed it would make exactly that write, and refusing it in the store keeps
// the key removed whatever that process believes.
func CheckSave(stored, next *Key) error {
	if stored == nil {
		return nil
	}
	if stored.Sealed == "" && next.Sealed != "" {
		return fmt.Errorf("%w: key %s had its private half erased", ErrKeyRemoved, next.ID)
	}
	if stored.RemovedAt != nil && (next.RemovedAt == nil || next.RemovedAt.After(*stored.RemovedAt)) {
		return fmt.Errorf("%w: key %s leaves the published set at %s", ErrKeyRemoved, next.ID,
			stored.RemovedAt.UTC().Format(time.RFC3339Nano))
	}
	return nil
}

// MemKeyStore is an in-memory KeyStore for tests and for an install with no database.
type MemKeyStore struct {
	// change serializes changes, standing for the lock a database holds for every process.
	change sync.Mutex
	// mu guards keys.
	mu sync.Mutex
	// keys holds a copy of each key by id.
	keys map[string]*Key
	// clock is the store's own clock, nil for a store that leaves time to each caller.
	clock func() time.Time
}

// NewMemKeyStore returns an empty in-memory key store with no clock of its own, for one process,
// which judges the keys by its own clock.
func NewMemKeyStore() *MemKeyStore {
	return &MemKeyStore{keys: map[string]*Key{}}
}

// NewMemKeyStoreWithClock returns an empty in-memory key store that judges its keys by now,
// standing for a database several processes share, whose clock decides for every one of them.
func NewMemKeyStoreWithClock(now func() time.Time) *MemKeyStore {
	m := NewMemKeyStore()
	m.clock = now
	return m
}

// memChange is one change in progress on a MemKeyStore.
type memChange struct {
	// store is the store the change belongs to.
	store *MemKeyStore
	// keys holds the change's own copy of every key by id, which replaces the store's keys when the
	// change succeeds. It is nil once the change has ended.
	keys map[string]*Key
	// now is the store's clock when the change took its lock, zero for a store with no clock.
	now time.Time
}

// memChangeKey is the context key a MemKeyStore change travels under.
type memChangeKey struct{}

// changeOf returns the change of m that ctx is inside, or nil.
func (m *MemKeyStore) changeOf(ctx context.Context) *memChange {
	c, _ := ctx.Value(memChangeKey{}).(*memChange)
	if c == nil || c.store != m {
		return nil
	}
	return c
}

// Change runs fn as one atomic change on a copy of the keys, serialized with every other change on
// m, and keeps the copy only when fn succeeds.
func (m *MemKeyStore) Change(ctx context.Context, fn func(ctx context.Context) error) error {
	if m.changeOf(ctx) != nil {
		return fn(ctx)
	}
	m.change.Lock()
	defer m.change.Unlock()
	c := &memChange{store: m, keys: map[string]*Key{}}
	if m.clock != nil {
		c.now = m.clock()
	}
	m.mu.Lock()
	for id, k := range m.keys {
		c.keys[id] = cloneKey(k)
	}
	m.mu.Unlock()
	err := fn(context.WithValue(ctx, memChangeKey{}, c))
	kept := c.keys
	c.keys = nil
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.keys = kept
	m.mu.Unlock()
	return nil
}

// Save writes a copy of k inside a change, refusing a write CheckSave refuses.
func (m *MemKeyStore) Save(ctx context.Context, k *Key) error {
	return m.Change(ctx, func(ctx context.Context) error {
		c := m.changeOf(ctx)
		if c.keys == nil {
			return errChangeEnded
		}
		if err := CheckSave(c.keys[k.ID], k); err != nil {
			return err
		}
		c.keys[k.ID] = cloneKey(k)
		return nil
	})
}

// List returns copies of every key, oldest first, with the id breaking a tie: the change's own
// copies inside a change, and the kept keys outside one.
func (m *MemKeyStore) List(ctx context.Context) ([]*Key, error) {
	if c := m.changeOf(ctx); c != nil {
		if c.keys == nil {
			return nil, errChangeEnded
		}
		return sortedCopies(c.keys), nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return sortedCopies(m.keys), nil
}

// Now returns the store's clock, the reading the change took inside a change, and the zero time
// for a store with no clock of its own.
func (m *MemKeyStore) Now(ctx context.Context) (time.Time, error) {
	if c := m.changeOf(ctx); c != nil {
		return c.now, nil
	}
	if m.clock == nil {
		return time.Time{}, nil
	}
	return m.clock(), nil
}

// sortedCopies returns copies of every key in keys, oldest first.
func sortedCopies(keys map[string]*Key) []*Key {
	out := make([]*Key, 0, len(keys))
	for _, k := range keys {
		out = append(out, cloneKey(k))
	}
	SortKeys(out)
	return out
}

// SortKeys orders keys oldest first, with the id breaking a tie, the order every KeyStore lists in.
func SortKeys(keys []*Key) {
	slices.SortStableFunc(keys, func(a, b *Key) int {
		if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
			return c
		}
		switch {
		case a.ID < b.ID:
			return -1
		case a.ID > b.ID:
			return 1
		default:
			return 0
		}
	})
}

// cloneKey returns a deep copy of k, so a store never shares a lifecycle time with its caller.
func cloneKey(k *Key) *Key {
	c := *k
	c.ActivatedAt = cloneTime(k.ActivatedAt)
	c.RetiredAt = cloneTime(k.RetiredAt)
	c.RemovedAt = cloneTime(k.RemovedAt)
	return &c
}

// cloneKeys returns a deep copy of every key in keys.
func cloneKeys(keys []*Key) []*Key {
	out := make([]*Key, 0, len(keys))
	for _, k := range keys {
		out = append(out, cloneKey(k))
	}
	return out
}

// cloneTime returns a copy of t, nil for nil.
func cloneTime(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	c := *t
	return &c
}

// newKey generates an RSA signing key, sealing its private half with sealer. It carries no times:
// the change that saves it stamps them from the clock that change judges by, since generating the
// key takes long enough that it happens before the change takes its lock.
func newKey(sealer *credential.Sealer) (*Key, error) {
	priv, err := rsa.GenerateKey(rand.Reader, rsaBits)
	if err != nil {
		return nil, fmt.Errorf("generate federation key: %w", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, fmt.Errorf("encode federation key: %w", err)
	}
	block := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	clear(der)
	sealed, err := sealer.Seal(string(block))
	clear(block)
	if err != nil {
		return nil, fmt.Errorf("seal federation key: %w", err)
	}
	pubDER, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("encode federation public key: %w", err)
	}
	kid, err := keyID(&priv.PublicKey)
	if err != nil {
		return nil, err
	}
	return &Key{
		ID: kid, Algorithm: string(jose.RS256),
		PublicKey: string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})),
		Sealed:    sealed,
	}, nil
}

// stamp sets k's creation to now and its activation to activation, both in UTC.
func stamp(k *Key, now, activation time.Time) {
	activated := activation.UTC()
	k.CreatedAt, k.ActivatedAt = now.UTC(), &activated
}

// keyID returns the RFC 7638 thumbprint of pub, base64url encoded without padding, the same id a
// relying party computes from the published key.
func keyID(pub *rsa.PublicKey) (string, error) {
	sum, err := (&jose.JSONWebKey{Key: pub}).Thumbprint(crypto.SHA256)
	if err != nil {
		return "", fmt.Errorf("thumbprint federation key: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(sum), nil
}

// openPrivate unseals k's private key.
func openPrivate(sealer *credential.Sealer, k *Key) (*rsa.PrivateKey, error) {
	if k.Sealed == "" {
		return nil, fmt.Errorf("open federation key %s: its private half was erased when it was "+
			"removed", k.ID)
	}
	plain, err := sealer.Open(k.Sealed)
	if err != nil {
		return nil, fmt.Errorf("open federation key %s: %w", k.ID, err)
	}
	block, _ := pem.Decode([]byte(plain))
	if block == nil {
		return nil, fmt.Errorf("open federation key %s: not PEM", k.ID)
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	clear(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("open federation key %s: %w", k.ID, err)
	}
	priv, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("open federation key %s: not an RSA key", k.ID)
	}
	return priv, nil
}

// parsePublic decodes k's public key.
func parsePublic(k *Key) (*rsa.PublicKey, error) {
	block, _ := pem.Decode([]byte(k.PublicKey))
	if block == nil {
		return nil, fmt.Errorf("read federation key %s: public key is not PEM", k.ID)
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("read federation key %s: %w", k.ID, err)
	}
	pub, ok := parsed.(*rsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("read federation key %s: not an RSA key", k.ID)
	}
	return pub, nil
}
