// Package audit records API mutations: who changed what, when. Reads are free; every
// authenticated write appends an entry, giving operators an ordered trail of configuration and
// execution actions.
package audit

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"time"
)

// Entry is one recorded API mutation.
type Entry struct {
	// ID is the unique entry identifier.
	ID string `json:"id"`
	// At is when the mutation happened.
	At time.Time `json:"at"`
	// Actor names who acted: the token label, which for UI sessions carries the username.
	Actor string `json:"actor"`
	// Method is the HTTP method.
	Method string `json:"method"`
	// Path is the request path.
	Path string `json:"path"`
}

// Store persists audit entries. Implementations must be safe for concurrent use.
type Store interface {
	// Append records one entry.
	Append(ctx context.Context, e *Entry) error
	// List returns up to limit entries, newest first.
	List(ctx context.Context, limit int) ([]*Entry, error)
}

// NewID returns a random audit entry identifier prefixed with "aud_".
func NewID() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("audit: read random: " + err.Error())
	}
	return "aud_" + hex.EncodeToString(b[:])
}
