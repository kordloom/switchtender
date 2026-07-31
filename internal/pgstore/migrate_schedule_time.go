package pgstore

import (
	"database/sql"
	"fmt"

	"github.com/kordloom/switchtender/internal/sqlutil"
)

// normalizeScheduleTimes rewrites every stored next_run_at into the form FormatTime produces now.
//
// ClaimDue is a compare and swap on next_run_at as text, so the stored bytes have to match what the
// running build writes. A release that changed the fractional second's width left existing rows in a
// form no later build could match, and every affected schedule stopped firing silently, because the
// scheduler reads a failed claim as another node having won.
//
// Normalizing on open makes the claim independent of which release wrote the row. It is cheap:
// schedules are few, and a row already in the canonical form is not rewritten.
func normalizeScheduleTimes(db *sql.DB) error {
	rows, err := db.Query("SELECT id, next_run_at FROM schedules WHERE next_run_at IS NOT NULL")
	if err != nil {
		return fmt.Errorf("normalize schedule times: %w", err)
	}
	type fix struct{ id, want string }
	var fixes []fix
	for rows.Next() {
		var id, stored string
		if err := rows.Scan(&id, &stored); err != nil {
			_ = rows.Close()
			return fmt.Errorf("normalize schedule times: %w", err)
		}
		at, err := sqlutil.ParseTime(stored)
		if err != nil {
			// An unparseable stamp is left alone. Rewriting a value we cannot read would be a guess,
			// and the schedule is already broken in a way a migration cannot honestly repair.
			continue
		}
		if want := sqlutil.FormatTime(at); want != stored {
			fixes = append(fixes, fix{id: id, want: want})
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("normalize schedule times: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("normalize schedule times: %w", err)
	}
	for _, f := range fixes {
		if _, err := db.Exec("UPDATE schedules SET next_run_at=$1 WHERE id=$2", f.want, f.id); err != nil {
			return fmt.Errorf("normalize schedule times: %w", err)
		}
	}
	return nil
}
