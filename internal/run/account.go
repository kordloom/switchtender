package run

import "context"

// accountKey types the context value carrying the account behind the request in flight.
type accountKey struct{}

// requestAccount is the account behind the credential of the request in flight.
type requestAccount struct {
	// id is the account's id, the value a run records as ActorUserID.
	id string
	// name is the account's username, the value a run records as Account.
	name string
}

// WithAccountContext returns ctx carrying the account behind the credential of the request in
// flight, by id and by username, so a run created while handling it records whose account asked. An
// empty id leaves ctx as it was: a credential bound to no account has none to record.
func WithAccountContext(ctx context.Context, id, name string) context.Context {
	if id == "" || name == "" {
		return ctx
	}
	return context.WithValue(ctx, accountKey{}, requestAccount{id: id, name: name})
}

// AccountFrom returns the id and username of the account behind the request in flight, both empty
// when the request carries none.
func AccountFrom(ctx context.Context) (id, name string) {
	a, _ := ctx.Value(accountKey{}).(requestAccount)
	return a.id, a.name
}
