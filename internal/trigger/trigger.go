// Package trigger fires job templates from inbound git webhooks. A trigger holds a secret token
// embedded in a webhook URL; a push to that URL launches the trigger's template, which syncs its
// project fresh and so runs the commit that was just pushed.
package trigger

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"
)

// tokenPrefix marks trigger webhook tokens so leaked strings are recognizable.
const tokenPrefix = "whk_"

// ErrNotFound is returned when a trigger does not exist in the store.
var ErrNotFound = errors.New("trigger not found")

// Trigger launches a template when its webhook URL is hit.
type Trigger struct {
	// ID is the unique trigger identifier.
	ID string `json:"id"`
	// Name labels the trigger for humans.
	Name string `json:"name"`
	// TemplateID is the template this trigger launches.
	TemplateID string `json:"template_id"`
	// TokenHash is the hex encoded SHA-256 of the webhook token; the token itself never persists.
	TokenHash string `json:"-"`
	// LastFiredAt is when the trigger last launched a run.
	LastFiredAt *time.Time `json:"last_fired_at,omitempty"`
	// CreatedAt is when the trigger was created.
	CreatedAt time.Time `json:"created_at"`
}

// Store persists triggers. Implementations must be safe for concurrent use.
type Store interface {
	// Save inserts or replaces the trigger identified by t.ID.
	Save(ctx context.Context, t *Trigger) error
	// Get returns the trigger with the given id, or ErrNotFound.
	Get(ctx context.Context, id string) (*Trigger, error)
	// List returns all triggers ordered by creation time, oldest first.
	List(ctx context.Context) ([]*Trigger, error)
	// Delete removes the trigger with the given id, or returns ErrNotFound.
	Delete(ctx context.Context, id string) error
	// FindByTokenHash returns the trigger with the given token hash, or ErrNotFound.
	FindByTokenHash(ctx context.Context, hash string) (*Trigger, error)
}

// New mints a trigger for a template: the plaintext token to embed in the webhook URL exactly
// once, and the stored record.
func New(name, templateID string) (string, *Trigger, error) {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", nil, err
	}
	plain := tokenPrefix + hex.EncodeToString(b[:])
	var id [6]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", nil, err
	}
	return plain, &Trigger{
		ID:         "trg_" + hex.EncodeToString(id[:]),
		Name:       name,
		TemplateID: templateID,
		TokenHash:  HashToken(plain),
		CreatedAt:  time.Now(),
	}, nil
}

// HashToken returns the hex encoded SHA-256 of a webhook token.
func HashToken(plain string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(plain)))
	return hex.EncodeToString(sum[:])
}
