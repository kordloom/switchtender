// Package audit records API mutations: who changed what, when. Reads are free; every
// authenticated write appends an entry, giving operators an ordered trail of configuration and
// execution actions. Entries are linked into a tamper-evident SHA-256 hash chain so an altered or
// deleted entry can be detected.
package audit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/kordloom/loomseal/jcs"

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
	// ActorType classifies the actor: human, agent, service, or system. It is committed by the chain
	// link, so a change an AI agent made cannot later be presented as a person's. Empty on an entry
	// recorded before the field existed, which a reader should treat as unstated rather than human.
	ActorType string `json:"actor_type,omitempty"`
	// OnBehalfOf names the account whose authority the actor used, empty when the actor acted as
	// itself. An agent bound to an operator account records the delegation here, so the trail answers
	// who authorized the agent and not only which agent ran.
	OnBehalfOf string `json:"on_behalf_of,omitempty"`
	// Method is the HTTP method.
	Method string `json:"method"`
	// Path is the request path.
	Path string `json:"path"`
	// ContentDigest is "sha256:" and the hex digest of the canonical, redacted change payload, empty
	// when the request carried no body. It is committed by the chain link, so the trail proves what a
	// change contained and not only that a call was made. See ContentDigestOf for the exact input.
	ContentDigest string `json:"content_digest,omitempty"`
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
	// ChainScan streams every entry with a sequence above afterSeq in chain order, oldest first,
	// calling fn once per entry, so a long trail can be verified without materializing it and an
	// incremental reader can resume from where it stopped. Zero streams the whole chain. A
	// non-nil error from fn stops the scan and is returned. The entry passed to fn is fn's to
	// keep.
	ChainScan(ctx context.Context, afterSeq int64, fn func(*Entry) error) error
}

// Receipt returns the entry's redeemable seq:link pair, the value the Audit-Receipt header carries
// and the one "switchtender audit receipt" redeems.
func Receipt(e *Entry) string {
	return strconv.FormatInt(e.Seq, 10) + ":" + e.Hash
}

// NewID returns a random audit entry identifier prefixed with "aud_".
func NewID() string {
	return idgen.New("aud_", 6)
}

// EntryHash returns the hex SHA-256 that commits to an entry's content and its PrevHash. The time is
// hashed in the canonical form the stores persist, so a hash computed at append matches one
// recomputed after a database round-trip.
//
// The input is the canonical JSON object of the entry's claim fields, not a fixed list of values in a
// fixed order. That choice is what makes the record extensible: a verifier canonicalizes whatever
// fields an entry carries, so an entry written before a field existed and one written after both
// verify, and adding a field later is not a format change. The previous construction hashed six
// values positionally, which meant every new field was a breaking revision of the profile and of
// every verifier implementing it.
//
// A field that is empty is omitted rather than hashed as an empty string, so an entry that predates
// a field is byte-identical to one that simply does not use it.
func EntryHash(e *Entry) string {
	claim := map[string]any{
		"seq":    e.Seq,
		"at":     e.At.UTC().Format(time.RFC3339Nano),
		"actor":  e.Actor,
		"method": e.Method,
		"path":   e.Path,
		"prev":   e.PrevHash,
	}
	for key, value := range map[string]string{
		"actor_type":     e.ActorType,
		"on_behalf_of":   e.OnBehalfOf,
		"content_digest": e.ContentDigest,
	} {
		if value != "" {
			claim[key] = value
		}
	}
	canonical, err := jcs.Serialize(claim)
	if err != nil {
		// Serialize fails only on a value type this map cannot hold: every value here is a string or
		// an int64. Hashing the error text would silently produce a link nothing can reproduce, so an
		// impossible input is a programming fault, not a runtime condition to paper over.
		panic("audit: canonicalize entry: " + err.Error())
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:])
}

// MaxCanonicalDigestBytes is the largest body canonicalized before digesting. A larger body is
// digested as its exact bytes, since parsing a multi-megabyte upload to normalize its key order
// costs more than the comparison it buys.
const MaxCanonicalDigestBytes = 1 << 20

// secretKeyStems are the substrings that mark a JSON key as secret-bearing, the same stems the
// run-log inventory masker uses. They match anywhere in the key, so ansible_become_password,
// secret_value, and token_id are all caught, not only the bare names. The digest is stored in the
// chain and served in exports, and a SHA-256 over a low-entropy secret is an offline brute-force
// target, so a value under one of these keys is replaced before the digest is taken.
var secretKeyStems = []string{
	"password", "passwd", "passphrase", "secret", "token", "apikey", "api_key",
	"private_key", "privatekey",
}

// secretKey reports whether a key's value is secret material. It matches the stems above anywhere in
// the key, the exact field "fields" (the secret bag of a custom credential type), and the bare pass
// stem only as a whole key or a terminal _pass, so ansible_ssh_pass matches while bypass and
// passthrough, whose values are ordinary, do not.
func secretKey(key string) bool {
	k := strings.ToLower(key)
	if k == "fields" || k == "pass" || strings.HasSuffix(k, "_pass") {
		return true
	}
	for _, stem := range secretKeyStems {
		if strings.Contains(k, stem) {
			return true
		}
	}
	return false
}

// freeTextSecretKeys hold arbitrary text that can embed a connection secret, so their whole value is
// redacted rather than trusted to key matching. An inventory's content routinely carries an
// ansible_password or an API token in an assignment no JSON key names.
var freeTextSecretKeys = map[string]bool{"content": true}

// urlUserinfo matches the user:password@ credential in a URL-shaped string. A repo or endpoint URL
// can carry an embedded credential, and the digest must not commit it while still showing the host.
var urlUserinfo = regexp.MustCompile(`(?i)([a-z][a-z0-9+.\-]*://)[^/@\s]+@`)

// redactedMarker stands in for a secret value in the digest input. It is a fixed constant so a
// redacted digest is reproducible, and it is not a plausible real value.
const redactedMarker = "«redacted»"

// ContentDigestOf returns the digest committed for a change payload: "sha256:" and the hex digest of
// the canonical, secret-redacted form of body, or the empty string when there is no body.
//
// A JSON body is redacted and canonicalized before digesting: the value of any secret-bearing key is
// replaced with a fixed marker, so the digest proves the non-secret shape and content of the change
// without becoming a brute-force target for the secret, and canonicalization makes two semantically
// identical requests digest alike so an auditor is comparing content rather than key order or
// whitespace. A body that is not JSON, or one too large to parse economically, is digested as its
// exact bytes; the mutating endpoints that carry a secret all take JSON, and the oversized case is an
// upload with no secret in it.
//
// A request with no body carries no digest at all rather than the digest of the empty string. Using
// sha256("") would make "there was no body" indistinguishable from "the body was empty", and an
// absent field is the honest statement of the first.
func ContentDigestOf(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	input := body
	if len(body) <= MaxCanonicalDigestBytes {
		if value, err := jcs.Parse(body); err == nil {
			value = redactSecrets(value)
			// Once the body parsed, the digest is taken over the redacted tree and never the raw
			// bytes. Falling back to the raw body when re-encoding failed would commit the secret the
			// redaction just removed, which is the one thing this digest must never do. JCS is
			// preferred so two equal requests digest alike; a value JCS cannot canonicalize, a
			// fractional or a very large number, falls back to a plain deterministic JSON encoding of
			// the same redacted tree, and a tree that will not encode at all digests the marker.
			if canonical, err := jcs.Serialize(value); err == nil {
				input = canonical
			} else if marshaled, merr := json.Marshal(value); merr == nil {
				input = marshaled
			} else {
				input = []byte(redactedMarker)
			}
		}
	}
	sum := sha256.Sum256(input)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// redactSecrets walks a parsed JSON value and returns it with every secret removed: the value of a
// secret-bearing key becomes the marker, a free-text secret field's string value is replaced whole,
// and any URL credential in a string leaf is stripped to its host. Replacing rather than deleting
// keeps the digest sensitive to whether a secret field was present while never committing what it
// held. It returns the value so a scrubbed string leaf propagates to its parent.
func redactSecrets(value any) any {
	switch v := value.(type) {
	case map[string]any:
		for key, child := range v {
			switch {
			case secretKey(key):
				v[key] = redactedMarker
			case freeTextSecretKeys[strings.ToLower(key)]:
				if _, isString := child.(string); isString {
					v[key] = redactedMarker
				} else {
					v[key] = redactSecrets(child)
				}
			default:
				v[key] = redactSecrets(child)
			}
		}
		return v
	case []any:
		for i, child := range v {
			v[i] = redactSecrets(child)
		}
		return v
	case string:
		return urlUserinfo.ReplaceAllString(v, "$1"+redactedMarker+"@")
	default:
		return v
	}
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
	// The fields added after the first release are escaped on the same terms. An actor type or a
	// delegated account carrying a raw invalid byte would otherwise hash to a value a verifier
	// reading decoded JSON could not reproduce, which is the hole escapeInvalidUTF8 exists to close.
	// The digest is generated hex and cannot carry one, so it is left alone.
	e.ActorType = escapeInvalidUTF8(e.ActorType)
	e.OnBehalfOf = escapeInvalidUTF8(e.OnBehalfOf)
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
