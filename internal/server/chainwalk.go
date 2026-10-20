package server

import (
	"context"
	"net/http"
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
func (c *chainWalks) hold(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		release, err := c.acquire(r.Context())
		if err != nil {
			return
		}
		defer release()
		next.ServeHTTP(w, r)
	})
}
