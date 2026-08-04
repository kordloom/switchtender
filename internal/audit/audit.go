// Package audit records API mutations: who changed what, when. Reads are free; every
// authenticated write appends an entry, giving operators an ordered trail of configuration and
// execution actions. Entries are linked into a tamper-evident SHA-256 hash chain so an altered or
// deleted entry can be detected.
package audit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"time"

	"github.com/kordloom/switchtender/internal/idgen"
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
	// Append records one entry, assigning its chain fields from the current head. It refuses an
	// entry for which IsSpanMarker is true with ErrReservedSpan: the marker means "the server
	// minted this beat", so only AppendSpanBeat may write it.
	Append(ctx context.Context, e *Entry) error
	// AppendSpanBeat atomically mints and appends the next span beat: one past the newest
	// well-formed span entry's beat, or beat one when the chain holds none, with count set to how
	// many entries were appended after that span entry (for beat one, every prior entry). The read
	// and the append are one serialized step, since a duplicate or missing beat number fails every
	// bundle built over the chain. It returns the appended entry.
	//
	// A time that does not strictly advance past the newest beat is refused with ErrClockBehind and
	// nothing is written. A beat's time is a signed claim about when the population was counted, so
	// recording a time the clock did not read would put a false statement in an attestation. A
	// backward clock therefore skips the beat, and the longer interval is reported by a verifier as
	// a gap with its bounds and duration. The beat number is not consumed, so the next accepted
	// beat takes it and the numbering stays contiguous.
	AppendSpanBeat(ctx context.Context, at time.Time, cadenceS int) (*Entry, error)
	// SpanBeats returns the newest limit span beat entries, those for which IsSpanMarker is true,
	// answered oldest first so a watcher reads the present end of the stream in chain order. The
	// filter runs store-side so an unauthenticated feed request never loads the whole chain.
	SpanBeats(ctx context.Context, limit int) ([]*Entry, error)
	// List returns up to limit entries, newest first.
	List(ctx context.Context, limit int) ([]*Entry, error)
	// Chain returns every entry in chain order, oldest first, for verification.
	Chain(ctx context.Context) ([]*Entry, error)
	// ChainScan streams every entry in chain order, oldest first, calling fn once per entry, so a
	// long trail can be verified without materializing it. A non-nil error from fn stops the scan
	// and is returned. The entry passed to fn is fn's to keep.
	ChainScan(ctx context.Context, fn func(*Entry) error) error
}

// NewID returns a random audit entry identifier prefixed with "aud_".
func NewID() string {
	return idgen.New("aud_", 6)
}

// EntryHash returns the hex SHA-256 that commits to an entry's content and its PrevHash. The time is
// hashed in the canonical form the stores persist, so a hash computed at append matches one
// recomputed after a database round-trip.
func EntryHash(e *Entry) string {
	payload := canonicalStrings([]string{
		strconv.FormatInt(e.Seq, 10),
		e.At.UTC().Format(time.RFC3339Nano),
		e.Actor, e.Method, e.Path, e.PrevHash,
	})
	sum := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(sum[:])
}

// Link normalizes e's hashed text, then fills its chain fields from prev, the current head of the
// chain, or a genesis link when prev is nil. A store calls it once per append while holding the
// append lock, so the chain stays linear and the entry it persists is the entry that was hashed.
func Link(prev, e *Entry) {
	// The text fields are escaped before anything commits to them, so the stored entry is valid
	// UTF-8 and no two distinct requests can share a chain link. See escapeInvalidUTF8.
	e.Actor = escapeInvalidUTF8(e.Actor)
	e.Method = escapeInvalidUTF8(e.Method)
	e.Path = escapeInvalidUTF8(e.Path)
	// The recorded time is truncated to microseconds before anything hashes it.
	//
	// The chain profile permits nanoseconds, but a link is only useful if an independent verifier
	// can recompute it, and most languages carry time at microsecond precision: Python's datetime,
	// which the reference verifier normalizes through, silently drops the last three digits and then
	// computes a different link. Linux reports nanosecond-granular wall clocks, so in practice
	// almost every entry recorded there would hash to a value no third party could reproduce, which
	// is the whole point of the chain. macOS reports microseconds, which is why this was invisible
	// in testing.
	//
	// Microseconds are ample for an audit timestamp, and truncating here rather than at emit keeps
	// the stored entry and any bundle built from it committing to the same instant.
	//
	// LoomSeal v0.2.1 made this defensive rather than load-bearing: the profile now hashes the
	// stored time bytes instead of parsing and re-serializing them, so a nanosecond entry verifies
	// too. It stays because a relying party may be holding an older copy of the reference verifier,
	// and because a microsecond timestamp survives every language a third party might check it in.
	e.At = e.At.Truncate(time.Microsecond)
	if prev != nil {
		e.Seq = prev.Seq + 1
		e.PrevHash = prev.Hash
	} else {
		e.Seq = 1
		e.PrevHash = ""
	}
	e.Hash = EntryHash(e)
}

// Verify walks entries in chain order, oldest first, and reports whether the whole chain is intact.
// When it is broken it returns the one-based position of the first entry whose sequence, link, or
// hash does not check out. An entry with no hash breaks the chain, so a blanked entry cannot hide
// from verification. It requires the slice to start at genesis; use VerifyRange for part of a chain.
func Verify(entries []*Entry) (ok bool, brokeAt int) {
	v := NewChainScanner(true)
	for _, e := range entries {
		v.Feed(e)
	}
	ok, brokeAt, _ = v.Result()
	return ok, brokeAt
}

// VerifyRange walks a contiguous slice of the chain, oldest first, and reports whether it is
// internally sound: every hash recomputes over the entry's own content, every sequence follows its
// predecessor, and every link names the entry before it. When it is broken it returns the one-based
// position of the first entry that does not check out.
//
// Unlike Verify it does not require the slice to begin at genesis, so a bundle covering part of a
// chain can be checked. The first entry is the only one whose predecessor is not present, and it
// still constrains: sequence one is the one entry in a chain with nothing before it, so it must
// carry no previous link, and any other first entry must carry one. A bundle that gets this wrong
// is rejected by an independent verifier, which is the worst place to find out.
func VerifyRange(entries []*Entry) (ok bool, brokeAt int) {
	v := NewChainScanner(false)
	for _, e := range entries {
		v.Feed(e)
	}
	ok, brokeAt, _ = v.Result()
	return ok, brokeAt
}
