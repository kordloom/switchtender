package factcache

import "errors"

var (
	// ErrNotFound is returned when a host has no cached facts in an inventory.
	ErrNotFound = errors.New("no cached facts for this host")
	// ErrTooLarge is returned when a host's fact document is larger than MaxFactsBytes.
	ErrTooLarge = errors.New("fact document too large")
	// ErrInvalid is returned when an entry names no inventory, names a host that cannot be a cache
	// file, or carries facts that are not a JSON object.
	ErrInvalid = errors.New("invalid fact cache entry")
)
