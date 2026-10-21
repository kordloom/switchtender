package server

import (
	"context"
	"net/http"
	"sync"
)

// chainWalks admits one full audit chain walk at a time across a server. A walk streams every entry
// over one store read connection for as long as the chain takes to read, about 1.5 s at 200,000
// entries, and SQLite has four. Concurrent verifies, bundles, receipts, and chain health once held
// all four and stalled every other read the server answers. A second walker now waits for the first
// instead of taking another connection.
type chainWalks struct {
	// slot holds a token while a walk runs.
	slot chan struct{}
}

// newChainWalks returns a gate with nothing walking.
func newChainWalks() *chainWalks {
	return &chainWalks{slot: make(chan struct{}, 1)}
}

// acquire waits for the walk slot and returns its release, or the context's error when the caller
// gives up first.
func (c *chainWalks) acquire(ctx context.Context) (func(), error) {
	select {
	case c.slot <- struct{}{}:
		return c.release, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// tryAcquire takes the walk slot when it is free and reports whether it did, so a caller with a
// cached answer can serve that instead of queuing.
func (c *chainWalks) tryAcquire() (func(), bool) {
	select {
	case c.slot <- struct{}{}:
		return c.release, true
	default:
		return nil, false
	}
}

// release frees the walk slot.
func (c *chainWalks) release() {
	<-c.slot
}

// hold wraps a handler that walks the whole chain so it runs only while holding the walk slot. The
// whole request holds it rather than each scan, because a receipt walks the chain twice and a
// bundle scans it twice, and a request that let another walker in between its scans would wait for
// the slot a second time with nothing gained. A caller that goes away while waiting is answered
// nothing, since nobody is left to read it.
//
// The slot is held through the scans and not through the response. A handler whose scans are done
// frees it with releaseWalk before writing its body, because serve sets no write timeout and a
// client reading a 20 MB bundle slowly would otherwise hold every other walker for the whole
// download. The release is made idempotent here, since a second receive on the empty slot would
// block forever, and it still runs on return for every path that never reached its write.
func (c *chainWalks) hold(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		acquired, err := c.acquire(r.Context())
		if err != nil {
			return
		}
		release := sync.OnceFunc(acquired)
		defer release()
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), walkReleaseKey{}, release)))
	})
}

// walkReleaseKey is the request context key under which hold stores the walk slot's release.
type walkReleaseKey struct{}

// releaseWalk frees the walk slot the request holds, so a handler whose last scan has returned can
// write its response without keeping other walkers waiting on its reader. It does nothing on a
// request hold did not admit, and nothing when called again.
func releaseWalk(ctx context.Context) {
	if release, ok := ctx.Value(walkReleaseKey{}).(func()); ok {
		release()
	}
}
