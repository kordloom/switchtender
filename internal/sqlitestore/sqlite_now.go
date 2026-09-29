package sqlitestore

import (
	"context"
	"time"
)

// Now returns the clock this store stamps and ages leases with. SQLite rows are stamped from this
// process, so the process clock is the store clock; the method exists so sweeps measure with one
// named clock everywhere instead of assuming the two are the same.
func (s *store) Now(context.Context) (time.Time, error) { return time.Now(), nil }
