package run

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"time"
)

// SecretLease records one dynamic secret minted for a claimed run, one this process executes or a
// relay run whose secrets a control node opened for its worker: enough for any replica to revoke it
// once the claim it was opened under ends, and never the secret itself.
//
// The process that minted a secret used to keep only the revoke func, in memory. One that crashed,
// stopped, or restarted while the run was out forgot what it minted, so the secret lived until its
// engine's TTL ran out, however long after the run that secret was minted for had ended.
type SecretLease struct {
	// ID identifies the record.
	ID string
	// RunID is the run the secret was minted for.
	RunID string
	// ClaimHash is ClaimHash of the claim lease the secret was opened under, so a later claim of the
	// same run is told apart without keeping the lease itself.
	ClaimHash string
	// CredentialID is the credential whose source minted the secret. A revoke reads its configuration
	// again for what the engine needs besides the handle, such as a token.
	CredentialID string
	// Kind is the source kind that minted the secret.
	Kind string
	// Handle is the engine's revoke handle, sealed. It names a live secret to the engine that issued
	// it, so it is never logged or returned, and it is opened only to revoke.
	Handle string `json:"-"`
	// ExpiresAt is when the engine ends the secret on its own. A record past it is dropped unrevoked.
	ExpiresAt time.Time
	// CreatedAt is when the secret was minted.
	CreatedAt time.Time
	// Attempts counts the revokes of the secret that have failed.
	Attempts int
	// RetryAt is when a failed revoke may be tried again, zero before the first failure.
	RetryAt time.Time
}

// SecretLeases keeps the records of minted secrets for as long as each may need revoking. The
// database stores implement it, and so does the in-memory store, for processes that share one.
type SecretLeases interface {
	// SaveSecretLease records l, replacing any record with its id.
	SaveSecretLease(ctx context.Context, l *SecretLease) error
	// ListSecretLeases returns up to limit records, the oldest first.
	ListSecretLeases(ctx context.Context, limit int) ([]*SecretLease, error)
	// TakeSecretLease deletes the record with id and reports whether this call deleted it, so of two
	// processes about to revoke one secret, exactly one does.
	TakeSecretLease(ctx context.Context, id string) (bool, error)
}

// ClaimHash returns the hex SHA-256 of a claim lease, how a record names the claim it belongs to.
func ClaimHash(claimSecret string) string {
	sum := sha256.Sum256([]byte(claimSecret))
	return hex.EncodeToString(sum[:])
}

// SaveSecretLease records l, replacing any record with its id.
func (m *memStore) SaveSecretLease(_ context.Context, l *SecretLease) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.secretLeases == nil {
		m.secretLeases = make(map[string]SecretLease)
	}
	m.secretLeases[l.ID] = *l
	return nil
}

// ListSecretLeases returns up to limit records, the oldest first.
func (m *memStore) ListSecretLeases(_ context.Context, limit int) ([]*SecretLease, error) {
	m.mu.RLock()
	out := make([]*SecretLease, 0, len(m.secretLeases))
	for _, l := range m.secretLeases {
		cp := l
		out = append(out, &cp)
	}
	m.mu.RUnlock()
	slices.SortFunc(out, func(a, b *SecretLease) int {
		return cmp.Or(a.CreatedAt.Compare(b.CreatedAt), cmp.Compare(a.ID, b.ID))
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// TakeSecretLease deletes the record with id and reports whether this call deleted it.
func (m *memStore) TakeSecretLease(_ context.Context, id string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.secretLeases[id]; !ok {
		return false, nil
	}
	delete(m.secretLeases, id)
	return true, nil
}
