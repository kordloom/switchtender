package sqlitestore

import (
	"database/sql"
	"fmt"

	"github.com/kordloom/switchtender/internal/schedule"
	"github.com/kordloom/switchtender/internal/sqlutil"
)

// staleScheduleTime is one stored next_run_at that is not in the form FormatTime produces now.
type staleScheduleTime struct {
	// id is the schedule.
	id string
	// stored is the stamp as it was read.
	stored string
	// want is the same instant in the canonical form.
	want string
}

// normalizeScheduleTimes rewrites every stored next_run_at into the form FormatTime produces now.
//
// ClaimDue is a compare and swap on next_run_at as text, so the stored bytes have to match what the
// running build writes. A release that changed the fractional second's width left existing rows in a
// form no later build could match, and every affected schedule stopped firing silently, because the
// scheduler reads a failed claim as another node having won.
//
// Normalizing on open makes the claim independent of which release wrote the row. It is cheap:
// schedules are few, and a row already in the canonical form is not rewritten.
//
// Each rewrite is itself a compare and swap on the stamp it read. During a rolling upgrade a server
// of the earlier release keeps claiming from the same rows, and a rewrite keyed by id alone landed
// on top of a claim made between the read and the write: it put the occurrence that had just fired
// back as due, and this release, which can match the canonical stamp, fired it a second time.
func normalizeScheduleTimes(db *sql.DB) error {
	stale, err := staleScheduleTimes(db)
	if err != nil {
		return err
	}
	return rewriteScheduleTimes(db, stale)
}

// staleScheduleTimes reads every stored next_run_at that is not in the canonical form.
func staleScheduleTimes(db *sql.DB) ([]staleScheduleTime, error) {
	rows, err := db.Query("SELECT id, next_run_at FROM schedules WHERE next_run_at IS NOT NULL")
	if err != nil {
		return nil, fmt.Errorf("normalize schedule times: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var stale []staleScheduleTime
	for rows.Next() {
		var id, stored string
		if err := rows.Scan(&id, &stored); err != nil {
			return nil, fmt.Errorf("normalize schedule times: %w", err)
		}
		at, err := sqlutil.ParseTime(stored)
		if err != nil {
			// An unparseable stamp is left alone. Rewriting a value we cannot read would be a guess,
			// and the schedule is already broken in a way a migration cannot honestly repair.
			continue
		}
		if want := sqlutil.FormatTime(at); want != stored {
			stale = append(stale, staleScheduleTime{id: id, stored: stored, want: want})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("normalize schedule times: %w", err)
	}
	return stale, nil
}

// rewriteScheduleTimes writes each stamp's canonical form where the row still holds the stamp that
// was read. A row a claim moved in between keeps the claim.
func rewriteScheduleTimes(db *sql.DB, stale []staleScheduleTime) error {
	for _, f := range stale {
		if _, err := db.Exec("UPDATE schedules SET next_run_at=? WHERE id=? AND next_run_at=?",
			f.want, f.id, f.stored); err != nil {
			return fmt.Errorf("normalize schedule times: %w", err)
		}
	}
	return nil
}

// pinScheduleZones writes schedule.UnnamedZone onto every stored schedule that names no zone, which
// only an earlier release writes.
//
// Such a schedule was read in the zone of whichever server evaluated it, so a highly available pair
// whose servers sat in different zones fired it in both, a daily schedule twice in one day. This
// release reads it in schedule.UnnamedZone, and writing that onto the row makes a server of the
// earlier release still running during a rolling upgrade read it the same way, and shows the zone
// on every view of it. Each write is a compare and swap on the empty zone, so an edit that named a
// zone in between is not overwritten. A schedule whose cron descriptor or DTSTART names its zone is
// left alone, since it never depended on the server.
func pinScheduleZones(db *sql.DB) error {
	rows, err := db.Query("SELECT id, cron, rrule FROM schedules WHERE timezone=''")
	if err != nil {
		return fmt.Errorf("pin schedule zones: %w", err)
	}
	var ids []string
	for rows.Next() {
		var sc schedule.Schedule
		if err := rows.Scan(&sc.ID, &sc.Cron, &sc.RRule); err != nil {
			_ = rows.Close()
			return fmt.Errorf("pin schedule zones: %w", err)
		}
		if !sc.NamesZone() {
			ids = append(ids, sc.ID)
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("pin schedule zones: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("pin schedule zones: %w", err)
	}
	for _, id := range ids {
		if _, err := db.Exec("UPDATE schedules SET timezone=? WHERE id=? AND timezone=''",
			schedule.UnnamedZone, id); err != nil {
			return fmt.Errorf("pin schedule zones: %w", err)
		}
	}
	return nil
}
