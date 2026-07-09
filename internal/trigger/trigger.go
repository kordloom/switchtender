// Package trigger fires job templates from inbound git webhooks. A trigger holds a secret token
// embedded in a webhook URL; a push to that URL launches the trigger's template, which syncs its
// project fresh and so runs the commit that was just pushed.
package trigger

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"
)

// tokenPrefix marks trigger webhook tokens so leaked strings are recognizable.
const tokenPrefix = "whk_"

// secretPrefix marks trigger signing secrets so leaked strings are recognizable.
const secretPrefix = "whs_"

// signaturePrefix is the algorithm label GitHub and GitLab prepend to the hex HMAC digest in the
// X-Hub-Signature-256 header.
const signaturePrefix = "sha256="

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
	// SigningSecret is the AES-GCM sealed HMAC secret used to verify X-Hub-Signature-256. It is
	// separate from the URL token so a leaked webhook URL does not also leak the signing key, and
	// never serializes to JSON. Empty when no encryption key was configured at creation.
	SigningSecret string `json:"-"`
	// RequireSignature rejects an inbound webhook whose HMAC signature is missing or wrong.
	RequireSignature bool `json:"require_signature"`
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

// NewSigningSecret returns a fresh webhook signing secret. The plaintext is shown once, sealed at
// rest with credential.Sealer, and set as the secret on the git host so its HMAC signatures verify.
// Rotation mints a new one.
func NewSigningSecret() (string, error) {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return secretPrefix + hex.EncodeToString(b[:]), nil
}

// SignBody returns the X-Hub-Signature-256 value for body under secret: the string sha256=<hex> the
// git host sends and the hook recomputes to authenticate the payload.
func SignBody(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return signaturePrefix + hex.EncodeToString(mac.Sum(nil))
}

// VerifySignature reports whether header is a valid X-Hub-Signature-256 for body under secret. The
// compare is constant time; an empty secret or header, or a digest of the wrong length, never
// matches.
func VerifySignature(secret string, body []byte, header string) bool {
	if secret == "" || header == "" {
		return false
	}
	return hmac.Equal([]byte(SignBody(secret, body)), []byte(header))
}
