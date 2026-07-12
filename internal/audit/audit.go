// Package audit records API mutations: who changed what, when. Reads are free; every
// authenticated write appends an entry, giving operators an ordered trail of configuration and
// execution actions. Entries are linked into a tamper-evident SHA-256 hash chain so an altered or
// deleted entry can be detected.
package audit

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"time"
)

// Entry is one recorded API mutation, linked into a tamper-evident hash chain.
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
	// Seq is the entry's position in the chain, assigned at append starting at one.
	Seq int64 `json:"seq"`
	// PrevHash is the hash of the entry before this one, empty for the first entry.
	PrevHash string `json:"prev_hash"`
	// Hash commits to this entry's content and PrevHash, so altering any field or reordering the
	// chain breaks verification.
	Hash string `json:"hash"`
}

// Store persists audit entries. Implementations must be safe for concurrent use and must serialize
// appends so the hash chain stays linear.
type Store interface {
	// Append records one entry, assigning its chain fields from the current head.
	Append(ctx context.Context, e *Entry) error
	// List returns up to limit entries, newest first.
	List(ctx context.Context, limit int) ([]*Entry, error)
	// Chain returns every entry in chain order, oldest first, for verification.
	Chain(ctx context.Context) ([]*Entry, error)
}

// NewID returns a random audit entry identifier prefixed with "aud_".
func NewID() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("audit: read random: " + err.Error())
	}
	return "aud_" + hex.EncodeToString(b[:])
}

// EntryHash returns the hex SHA-256 that commits to an entry's content and its PrevHash. The time is
// hashed in the canonical form the stores persist, so a hash computed at append matches one
// recomputed after a database round-trip.
func EntryHash(e *Entry) string {
	payload, _ := json.Marshal([]string{
		strconv.FormatInt(e.Seq, 10),
		e.At.UTC().Format(time.RFC3339Nano),
		e.Actor, e.Method, e.Path, e.PrevHash,
	})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

// Link fills e's chain fields from prev, the current head of the chain, or a genesis link when prev
// is nil. A store calls it while holding the append lock so the chain stays linear.
func Link(prev, e *Entry) {
	if prev != nil {
		e.Seq = prev.Seq + 1
		e.PrevHash = prev.Hash
	} else {
		e.Seq = 1
		e.PrevHash = ""
	}
	e.Hash = EntryHash(e)
}

// Verify walks entries in chain order, oldest first, and reports whether the chain is intact. When
// it is broken it returns the one-based position of the first entry whose sequence, link, or hash
// does not check out. An entry with no hash breaks the chain, so a blanked entry cannot hide from
// verification.
func Verify(entries []*Entry) (ok bool, brokeAt int) {
	var prev *Entry
	for i, e := range entries {
		wantSeq := int64(1)
		wantPrev := ""
		if prev != nil {
			wantSeq = prev.Seq + 1
			wantPrev = prev.Hash
		}
		if e.Hash == "" || e.Seq != wantSeq || e.PrevHash != wantPrev || e.Hash != EntryHash(e) {
			return false, i + 1
		}
		prev = e
	}
	return true, 0
}
