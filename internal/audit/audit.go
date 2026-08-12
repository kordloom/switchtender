// Package audit records API mutations: who changed what, when. Reads are free; every
// authenticated write appends an entry, giving operators an ordered trail of configuration and
// execution actions. Entries are linked into a tamper-evident SHA-256 hash chain so an altered or
// deleted entry can be detected.
package audit

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/kordloom/loomseal/jcs"

	"github.com/kordloom/switchtender/internal/idgen"
	"github.com/kordloom/switchtender/internal/util"
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
	// ContentDigest is the keyed commitment to the canonical, redacted change payload, empty when the
	// request carried no body. It is committed by the chain link, so the trail proves what a change
	// contained and not only that a call was made, and it is keyed by Nonce so it cannot be guessed
	// back to the payload by a holder of an export. See ContentDigestOf for the exact form.
	ContentDigest string `json:"content_digest,omitempty"`
	// Nonce is the random key the ContentDigest commits under, hex encoded, stored beside the entry
	// and never exported. The json:"-" tag is load-bearing: it keeps the nonce out of every bundle,
	// SIEM forward, and evidence document, which is what stops a party holding one of those from
	// recomputing the payload. It is not part of the chain hash; the digest already commits to the
	// nonce's own hash, so a swapped nonce is detectable without the nonce being in the link.
	Nonce string `json:"-"`
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
	return linkOf(claimObject(e.Seq, e.At.UTC().Format(time.RFC3339Nano),
		e.Actor, e.Method, e.Path, e.PrevHash, e.ActorType, e.OnBehalfOf, e.ContentDigest))
}

// claimObject builds the exact map a chain link is computed over: the fields the link commits to, in
// the shape both EntryHash and bundle verification serialize. Sharing it is what keeps producing a
// link and recomputing one from a bundled claim on a single definition, so they cannot drift.
func claimObject(seq int64, at, actor, method, path, prev, actorType, onBehalfOf, contentDigest string) map[string]any {
	claim := map[string]any{"seq": seq, "at": at, "actor": actor, "method": method, "path": path, "prev": prev}
	for key, value := range map[string]string{
		"actor_type": actorType, "on_behalf_of": onBehalfOf, "content_digest": contentDigest,
	} {
		if value != "" {
			claim[key] = value
		}
	}
	return claim
}

// linkOf serializes a claim object canonically and returns its hex SHA-256, the chain link.
func linkOf(claim map[string]any) string {
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

// secretKey reports whether a key's value is secret material, deferring to the one classifier the
// inventory redactor and the run-log masker also use. The digest is stored in the chain and served
// in exports, and a SHA-256 over a low-entropy secret is an offline brute-force target, so a value
// under such a key is replaced before the digest is taken.
func secretKey(key string) bool {
	return util.SecretKey(key)
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

// ContentDigestOf returns the keyed commitment to a change payload and the nonce it is keyed under,
// or empty strings when there is no body. The digest is "sha256s:", the hex SHA-256 of the nonce,
// and the hex HMAC-SHA256 of the canonical redacted payload under the nonce. The caller stores the
// nonce beside the entry and never exports it, and commits the digest into the chain.
//
// A JSON body is redacted and canonicalized before it is committed: the value of any secret-bearing
// key becomes a fixed marker, so the commitment proves the non-secret shape and content of the change
// without carrying the secret. Keying it with a per-entry nonce is what prevents a holder of a bundle
// or a SIEM event, which carry the digest but not the nonce, from guessing a payload whose secret
// slipped past redaction and confirming it by recomputation. A body that is not JSON, or one too
// large to parse economically, is committed as its exact bytes; the mutating endpoints that carry a
// secret all take JSON, and the oversized case is an upload with no secret in it.
//
// A request with no body carries no digest at all rather than the digest of the empty string. Using a
// commitment over "" would make "there was no body" indistinguishable from "the body was empty", and
// an absent field is the honest statement of the first.
func ContentDigestOf(body []byte) (digest, nonce string, err error) {
	if len(body) == 0 {
		return "", "", nil
	}
	input := canonicalForDigest(body)

	// A plain SHA-256 over the redacted body is recomputable by anyone holding it, which is what an
	// export and a SIEM event both carry. That makes a body whose secret slipped past redaction, a
	// secret under a variable name the stem list does not know, an offline guessing target: try a
	// candidate, recompute, compare. Keying the commitment with a fresh random nonce removes that:
	// the digest is HMAC(nonce, body), so guessing the body is useless without the nonce, and the
	// nonce is stored beside the entry and never leaves in a bundle. The nonce's own hash is
	// committed too, so a nonce that was later swapped no longer matches what the chain fixed, which
	// keeps a disclosure honest. See VerifyContentDigest for how a holder of the body and nonce
	// checks one entry without being handed any other.
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", "", err
	}
	nonceHash := sha256.Sum256(raw[:])
	mac := hmac.New(sha256.New, raw[:])
	mac.Write(input)
	return "sha256s:" + hex.EncodeToString(nonceHash[:]) + ":" + hex.EncodeToString(mac.Sum(nil)),
		hex.EncodeToString(raw[:]), nil
}

// canonicalForDigest reduces a request body to the bytes the digest commits to: the redacted,
// canonical JSON when it parses, or the raw body when it is too large to canonicalize economically.
// A body that parses is never digested raw, so a secret the redaction removed is not committed by a
// re-encoding failure falling back to the original bytes. A value JCS cannot canonicalize falls back
// to a plain deterministic JSON encoding of the same redacted tree, and a tree that will not encode
// at all reduces to the marker.
func canonicalForDigest(body []byte) []byte {
	input := body
	if len(body) <= MaxCanonicalDigestBytes {
		if value, err := jcs.Parse(body); err == nil {
			value = redactSecrets(value)
			if canonical, err := jcs.Serialize(value); err == nil {
				input = canonical
			} else if marshaled, merr := json.Marshal(value); merr == nil {
				input = marshaled
			} else {
				input = []byte(redactedMarker)
			}
		}
	}
	return input
}

// VerifyContentDigest reports whether body, under nonce, is the change committed by digest. It is how
// a party handed one entry's body and nonce proves that entry without being shown any other: the body
// is redacted and canonicalized the same way it was at record time, and the keyed commitment is
// recomputed. It accepts the legacy unkeyed form so an entry written before the nonce existed still
// verifies from the body alone.
func VerifyContentDigest(digest, nonce string, body []byte) bool {
	input := canonicalForDigest(body)
	switch {
	case strings.HasPrefix(digest, "sha256s:"):
		parts := strings.Split(strings.TrimPrefix(digest, "sha256s:"), ":")
		if len(parts) != 2 {
			return false
		}
		raw, err := hex.DecodeString(nonce)
		if err != nil || len(raw) == 0 {
			return false
		}
		nonceHash := sha256.Sum256(raw)
		if !hmac.Equal([]byte(hex.EncodeToString(nonceHash[:])), []byte(parts[0])) {
			return false
		}
		mac := hmac.New(sha256.New, raw)
		mac.Write(input)
		return hmac.Equal([]byte(hex.EncodeToString(mac.Sum(nil))), []byte(parts[1]))
	case strings.HasPrefix(digest, "sha256:"):
		sum := sha256.Sum256(input)
		return hmac.Equal([]byte("sha256:"+hex.EncodeToString(sum[:])), []byte(digest))
	default:
		return false
	}
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
