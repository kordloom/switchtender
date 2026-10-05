package run

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/kordloom/switchtender/internal/util"
)

// DedupeWindow is how long a repeat of the same one-click action collapses onto the run it already
// created. It is long enough to absorb an impatient second click and short enough that deliberately
// firing the same action again a moment later still starts a fresh run.
const DedupeWindow = 10 * time.Second

// internalKeyPrefix marks an idempotency key the server derived rather than one a client supplied.
//
// Both kinds live in the same column under one unique index. Without a marker a caller could send
// Idempotency-Key with the exact string a later rerun would derive, planting a run under that key so
// the rerun resolves to it and never executes. The prefix is reserved: ClientKey refuses to mint one,
// so a derived key cannot be forged from outside.
const internalKeyPrefix = "st:"

// ErrReservedKey is returned when a caller supplies an idempotency key in the server's namespace.
var ErrReservedKey = errors.New("reserved idempotency key")

// MaxClientKeyBytes bounds the idempotency key a caller may send. The key is stored in a column
// under a unique index, and PostgreSQL refuses an index entry past about 2.7 kilobytes, so a longer
// key that does not compress failed the submit with a 500. A UUID is 36 bytes and the keys clients
// derive from a request run to a few dozen more, so the bound leaves every real key room and stays
// far below the index limit.
const MaxClientKeyBytes = 255

// clientDigestPrefix opens the stored form of a caller's idempotency key that is kept as a digest:
// every key scoped to an organization, and a key that is not text. It sits in the reserved
// namespace, so no caller can send it, and no derived action is named client.
const clientDigestPrefix = internalKeyPrefix + "client:"

// digestMark opens a part of a derived key that is kept as a digest of the id it stands for.
const digestMark = "#"

// DedupeKey returns the idempotency key that action on the run named by id saves under during the
// window containing at. Two requests inside one window derive the same key, so the store's unique
// index on it rejects the second run rather than letting a double click fire twice.
//
// The key is text every backend stores. An id that is not, such as a tuple joined with a NUL byte
// or a name that is not valid UTF-8, stands in the key as its digest. PostgreSQL refuses both in a
// text value with SQLSTATE 22021, and a provisioning callback's id joins its template and its host
// with a NUL byte, so before this every callback failed its replay lookup on PostgreSQL.
func DedupeKey(action, id string, at time.Time) string {
	return dedupeKeyIn(action, id, at.UnixNano()/int64(DedupeWindow))
}

// dedupeKeyIn returns the key action on id saves under in the window numbered bucket.
func dedupeKeyIn(action, id string, bucket int64) string {
	return fmt.Sprintf("%s%s:%s:%d", internalKeyPrefix, action, keyPart(id), bucket)
}

// keyPart returns s as it stands inside a derived key: s itself when every backend stores it as
// text, and otherwise digestMark followed by its digest. A part that already begins with digestMark
// is digested too, so the two forms can never spell each other.
func keyPart(s string) string {
	if util.IsSafeText(s) && !strings.HasPrefix(s, digestMark) {
		return s
	}
	return digestMark + digestHex(s)
}

// digestHex returns the hex SHA-256 of s.
func digestHex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// ScheduleKey returns the idempotency key the run a schedule fires for one occurrence saves under.
// Every fire of the same occurrence derives the same key, so a fire that takes up an occurrence an
// interrupted fire handed back finds the run that one made, if it made one, instead of starting a
// second. The key sits in the server's reserved namespace, so no caller can plant a run under it
// and make the schedule's fire resolve to that run.
func ScheduleKey(scheduleID string, occurrence time.Time) string {
	return fmt.Sprintf("%sschedule:%s:%d", internalKeyPrefix, scheduleID, occurrence.UnixNano())
}

// orgKeySeparator joins an organization to a caller's idempotency key in the text a scoped key's
// digest is taken over. It is a byte a key cannot contain, so one organization cannot spell
// another's key.
const orgKeySeparator = "\x00"

// ClientKey returns the key a caller-supplied Idempotency-Key header is stored under, or an error when
// the caller tried to claim the server's reserved namespace or sent a key it may not spell.
//
// The key is scoped to the submitting organization. Callers choose ordinary words for these, "nightly",
// "deploy-2026-08-17", "1", so two organizations on one install collide as a matter of course, and the
// stored key used to be global: the second organization's submission found the first one's run and
// returned it, handing over that run's id, command, actor, and status, while the change they asked for
// never ran. Neither side saw an error, which is what made it a leak rather than a bug report.
//
// A scoped key is stored as the digest of the organization and the key joined by a NUL byte, under
// the reserved namespace. The joined text itself used to be stored, and PostgreSQL refuses a NUL in
// a text value, so on the backend high availability runs on every retried submission from an
// organization failed instead of finding its run. The digest is text on every backend, has one
// length whatever was sent, and cannot be sent by a caller. A key that is not valid UTF-8, which a
// header can carry, is stored as its digest for the same reason.
//
// An install with no organizations stores any other key exactly as sent, so a single-tenant
// deployment's keys keep the shape they always had.
func ClientKey(supplied, orgID string) (string, error) {
	if len(supplied) > MaxClientKeyBytes {
		return "", fmt.Errorf("%w: an idempotency key may be at most %d bytes, and this one is %d",
			ErrKeyTooLong, MaxClientKeyBytes, len(supplied))
	}
	if strings.HasPrefix(supplied, internalKeyPrefix) {
		return "", fmt.Errorf("%w: an idempotency key may not begin with %q", ErrReservedKey,
			internalKeyPrefix)
	}
	if strings.Contains(supplied, orgKeySeparator) {
		return "", fmt.Errorf("%w: an idempotency key may not contain a null byte", ErrReservedKey)
	}
	if orgID != "" {
		supplied = orgID + orgKeySeparator + supplied
	}
	return clientStoredKey(supplied), nil
}

// clientStoredKey returns how a caller's key is stored, given the text earlier releases stored for
// it: that text when every backend stores it, and its digest under clientDigestPrefix otherwise.
func clientStoredKey(joined string) string {
	if util.IsSafeText(joined) {
		return joined
	}
	return clientDigestPrefix + digestHex(joined)
}

// CurrentKey returns the idempotency key this release stores in place of one an earlier release
// stored, and whether the two differ.
//
// Earlier releases stored a key scoped to an organization and a provisioning callback's replay key
// with a NUL byte inside them, and a caller's key that was not valid UTF-8 exactly as it arrived.
// SQLite keeps all three. A store rewrites them with this when it opens, so a retried submission or
// a repeated callback still finds the run an earlier release recorded, and every key it holds is one
// PostgreSQL can hold too.
func CurrentKey(stored string) (string, bool) {
	if util.IsSafeText(stored) {
		return stored, false
	}
	if rest, ok := strings.CutPrefix(stored, internalKeyPrefix); ok {
		action, tail, found := strings.Cut(rest, ":")
		at := strings.LastIndexByte(tail, ':')
		if found && at >= 0 && util.IsSafeText(action) {
			if bucket, err := strconv.ParseInt(tail[at+1:], 10, 64); err == nil {
				return dedupeKeyIn(action, tail[:at], bucket), true
			}
		}
	}
	return clientDigestPrefix + digestHex(stored), true
}

// ResolveDedupe returns the run that a repeat of action on id already created inside the dedupe
// window, nil when there is none, together with the key a fresh run must carry. It looks in the
// bucket containing now and in the one before it, so two clicks a moment apart resolve to the same
// run even when they land either side of a bucket boundary, then it bounds the match by the run's
// own creation time so the window is DedupeWindow rather than the one-to-two buckets the lookup
// spans.
//
// An empty key means submit without one. That happens only when the current bucket's key is already
// taken by a run outside the window, which a forward-running clock cannot produce: bucket numbers
// rise with wall time, so a stale key is unreachable. It takes a clock that went backwards, and
// there the choice is between starting a run with no dedupe protection and silently swallowing a
// run the operator asked for. Losing the protection is the smaller failure.
//
// The keys are wall-clock derived and cannot be anything else. They are persisted on the run and
// have to be recomputable by another control node and by the same node after a restart, and a
// monotonic reading is meaningful in neither place. So a clock stepped backwards by more than
// DedupeWindow still lets a request land on a bucket it already visited, and if the same action ran
// on the same run in that bucket, the repeat collapses onto it. Keep the clock disciplined.
func ResolveDedupe(ctx context.Context, store Store, action, id string, now time.Time) (*Run, string, error) {
	key := DedupeKey(action, id, now)
	for _, k := range []string{key, DedupeKey(action, id, now.Add(-DedupeWindow))} {
		existing, err := store.ByIdempotencyKey(ctx, k)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, "", err
		}
		if age := now.Sub(existing.CreatedAt); age < DedupeWindow && age > -DedupeWindow {
			return existing, key, nil
		}
		if k == key {
			return nil, "", nil
		}
	}
	return nil, key, nil
}
