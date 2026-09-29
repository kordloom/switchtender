package run

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
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

// DedupeKey returns the idempotency key that action on the run named by id saves under during the
// window containing at. Two requests inside one window derive the same key, so the store's unique
// index on it rejects the second run rather than letting a double click fire twice.
func DedupeKey(action, id string, at time.Time) string {
	return fmt.Sprintf("%s%s:%s:%d", internalKeyPrefix, action, id, at.UnixNano()/int64(DedupeWindow))
}

// orgKeySeparator joins an organization to a caller's idempotency key. It is a byte a key cannot
// contain, so one organization cannot spell another's stored key.
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
// An install with no organizations stores the key exactly as sent, so a single-tenant deployment's keys
// keep the shape they always had.
func ClientKey(supplied, orgID string) (string, error) {
	if strings.HasPrefix(supplied, internalKeyPrefix) {
		return "", fmt.Errorf("%w: an idempotency key may not begin with %q", ErrReservedKey,
			internalKeyPrefix)
	}
	if strings.Contains(supplied, orgKeySeparator) {
		return "", fmt.Errorf("%w: an idempotency key may not contain a null byte", ErrReservedKey)
	}
	if orgID == "" {
		return supplied, nil
	}
	return orgID + orgKeySeparator + supplied, nil
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
