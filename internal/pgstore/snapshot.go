package pgstore

import (
	"context"
	"fmt"
)

// BeginReadSnapshot pins every read that follows inside one REPEATABLE READ, READ ONLY transaction,
// so a backup reads the whole database at a single instant instead of table by table while the
// server writes it.
//
// It works by shrinking the pool to one connection and opening the transaction on it, so each read
// that follows lands on that connection inside the snapshot. That is sound here because the one
// caller is the backup CLI, whose only database work is the snapshot itself; it is not a facility
// for a process with concurrent readers. The release verifies the snapshot actually held: the pool
// recycles connections on a lifetime, and a recycled connection would carry the reads that followed
// outside the snapshot with nothing else saying so, which must fail the backup rather than seal it.
func (d *DB) BeginReadSnapshot(ctx context.Context) (func() error, error) {
	d.db.SetMaxOpenConns(1)
	fail := func(err error) (func() error, error) {
		d.db.SetMaxOpenConns(maxPoolConns)
		return nil, err
	}
	if _, err := d.db.ExecContext(ctx, "BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY"); err != nil {
		return fail(fmt.Errorf("begin read snapshot: %w", err))
	}
	var pid int
	if err := d.db.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
		_, _ = d.db.ExecContext(ctx, "COMMIT")
		return fail(fmt.Errorf("begin read snapshot: %w", err))
	}
	return func() error {
		bg := context.Background()
		var pidNow int
		var readOnly string
		pidErr := d.db.QueryRowContext(bg, "SELECT pg_backend_pid()").Scan(&pidNow)
		roErr := d.db.QueryRowContext(bg,
			"SELECT current_setting('transaction_read_only')").Scan(&readOnly)
		_, cerr := d.db.ExecContext(bg, "COMMIT")
		d.db.SetMaxOpenConns(maxPoolConns)
		if pidErr != nil || roErr != nil || cerr != nil {
			return fmt.Errorf("the read snapshot did not hold: pid check %v, mode check %v, "+
				"commit %v", pidErr, roErr, cerr)
		}
		if pidNow != pid || readOnly != "on" {
			return fmt.Errorf("the read snapshot did not hold: the pool replaced its connection " +
				"mid-backup, so some tables were read outside the snapshot")
		}
		return nil
	}, nil
}
