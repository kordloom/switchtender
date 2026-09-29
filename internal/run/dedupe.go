package run

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// DedupeWindow is how long a repeat of the same one-click action collapses onto the run it already
// created. It is long enough to absorb an impatient second click and short enough that deliberately
// firing the same action again a moment later still starts a fresh run.
const DedupeWindow = 10 * time.Second

// DedupeKey returns the idempotency key that action on the run named by id saves under during the
// window containing at. Two requests inside one window derive the same key, so the store's unique
// index on it rejects the second run rather than letting a double click fire twice.
func DedupeKey(action, id string, at time.Time) string {
	return fmt.Sprintf("%s:%s:%d", action, id, at.UnixNano()/int64(DedupeWindow))
}

// ResolveDedupe returns the run that a repeat of action on id already created inside the dedupe
// window, nil when there is none, together with the key a fresh run must carry. It looks in the
// window containing now and in the one before it, so two clicks a moment apart always resolve to
// the same run even when they land either side of a window boundary.
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
		return existing, key, nil
	}
	return nil, key, nil
}
