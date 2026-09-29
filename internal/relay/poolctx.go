package relay

import "context"

// poolKey is the context key carrying the resolved worker pool.
type poolKey struct{}

// withPool returns a context carrying the pool the presented token belongs to.
func withPool(ctx context.Context, p *Pool) context.Context {
	return context.WithValue(ctx, poolKey{}, p)
}

// poolFrom returns the pool the request's token belongs to, or nil when the request did not come
// through the authenticating middleware.
func poolFrom(ctx context.Context) *Pool {
	p, _ := ctx.Value(poolKey{}).(*Pool)
	return p
}
