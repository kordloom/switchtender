package pgstore

import (
	"context"
	"fmt"
	"time"

	"github.com/kordloom/switchtender/internal/sqlutil"
)

// Now returns the database clock, the one Claim stamps and ReclaimStale ages with. A sweep on one
// replica aging rows another replica stamped must measure with this clock: each process trusting
// its own wall clock is how a healthy run on a slow server got settled as timed out by a fast one.
func (s *store) Now(ctx context.Context) (time.Time, error) {
	var raw string
	if err := s.db.QueryRowContext(ctx, "SELECT "+pgNowText).Scan(&raw); err != nil {
		return time.Time{}, fmt.Errorf("store now: %w", err)
	}
	t, err := sqlutil.ParseTime(raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("store now: %w", err)
	}
	return t, nil
}
