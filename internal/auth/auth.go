// Package auth holds API token authentication: opaque bearer tokens whose hashes persist in the
// store. The plaintext token is shown once at creation and never stored or logged.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"
)

// tokenPrefix marks SwitchTender API tokens so leaked strings are recognizable in scanners.
const tokenPrefix = "ymt_"

// ErrNotFound is returned when a token does not exist in the store.
var ErrNotFound = errors.New("token not found")

// Token is one API token's stored form. The secret itself never persists, only its hash.
type Token struct {
	// ID is the unique token identifier.
	ID string `json:"id"`
	// Name labels the token for humans, for example ci or laptop.
	Name string `json:"name"`
	// UserID names the account the token authenticates as. Empty means an unscoped token from
	// the command line, which carries admin rights.
	UserID string `json:"user_id,omitempty"`
	// Kind declares what holds the token. Empty means a person. KindAgent marks a token issued to an
	// AI agent: the chain records its actions under that identity, and the token is capped so it
	// cannot manage identity, access, or secrets no matter what account it is bound to. The kind is
	// set when the token is minted and observed, never guessed from how a request looks.
	Kind string `json:"kind,omitempty"`
	// Hash is the hex encoded SHA-256 of the plaintext token.
	Hash string `json:"-"`
	// CreatedAt is when the token was created.
	CreatedAt time.Time `json:"created_at"`
	// LastUsedAt is when the token last authenticated a request.
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	// ExpiresAt is when the token stops working. Nil means it never expires.
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// KindAgent marks a token held by an AI agent rather than a person. An empty kind is a person.
const KindAgent = "agent"

// Expired reports whether the token is past its expiry.
func (t *Token) Expired(now time.Time) bool {
	return t.ExpiresAt != nil && now.After(*t.ExpiresAt)
}

// IsAgent reports whether the token was issued to an agent.
func (t *Token) IsAgent() bool {
	return t.Kind == KindAgent
}

// Store persists API tokens. Implementations must be safe for concurrent use.
type Store interface {
	// Save inserts or replaces the token identified by t.ID.
	Save(ctx context.Context, t *Token) error
	// List returns all tokens ordered by creation time, oldest first.
	List(ctx context.Context) ([]*Token, error)
	// Delete removes the token with the given id, or returns ErrNotFound.
	Delete(ctx context.Context, id string) error
	// FindByHash returns the token with the given hash, or ErrNotFound.
	FindByHash(ctx context.Context, hash string) (*Token, error)
	// Count returns how many tokens exist, deciding whether authentication is enforced.
	Count(ctx context.Context) (int, error)
}

// New mints a token: the plaintext to hand to the caller exactly once, and the stored record.
func New(name string) (string, *Token, error) {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", nil, err
	}
	plain := tokenPrefix + hex.EncodeToString(b[:])
	var id [6]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", nil, err
	}
	return plain, &Token{
		ID:        "tok_" + hex.EncodeToString(id[:]),
		Name:      name,
		Hash:      HashToken(plain),
		CreatedAt: time.Now(),
	}, nil
}

// HashToken returns the hex encoded SHA-256 of a plaintext token. Comparing hashes of fixed
// length stands in for constant-time comparison of secrets.
func HashToken(plain string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(plain)))
	return hex.EncodeToString(sum[:])
}

// FromHeader extracts the bearer token from an Authorization header value, empty when absent.
func FromHeader(header string) string {
	const scheme = "Bearer "
	if len(header) > len(scheme) && strings.EqualFold(header[:len(scheme)], scheme) {
		return strings.TrimSpace(header[len(scheme):])
	}
	return ""
}
