package sqlitestore

import (
	"context"
	"fmt"
)

// BeginReadSnapshot pins every read that follows to one connection holding an open deferred
// transaction, so a backup reads the whole database at a single instant instead of table by table
// while another process writes it. WAL keeps the snapshot for as long as the transaction is open,
// and the writer is never blocked by it.
//
// It works by shrinking the read pool to one connection and opening a plain transaction on it, so
// each read that follows lands on that same connection inside the snapshot. That is sound here
// because the one caller is the backup CLI, whose only database work is the snapshot itself; it is
// not a facility for a process with concurrent readers, whose statements would land inside the
// transaction. The release's error is real: SQLite refuses a COMMIT with no transaction active, so
// a snapshot that was lost mid-read surfaces as a failed release rather than as a quiet backup of
// mixed instants.
func (d *DB) BeginReadSnapshot(ctx context.Context) (func() error, error) {
	r := d.db.r
	// When the path could not carry a second handle, reads already run on the single serialized
	// write connection and the pool must stay at one; resizing it to the read pool's width on
	// release would break the single-writer invariant the whole store is built on.
	resize := r != d.db.w
	if resize {
		r.SetMaxOpenConns(1)
	}
	if _, err := r.ExecContext(ctx, "BEGIN"); err != nil {
		if resize {
			r.SetMaxOpenConns(readPoolConns)
		}
		return nil, fmt.Errorf("begin read snapshot: %w", err)
	}
	return func() error {
		_, cerr := r.ExecContext(context.Background(), "COMMIT")
		if resize {
			r.SetMaxOpenConns(readPoolConns)
		}
		if cerr != nil {
			return fmt.Errorf("the read snapshot did not hold: %w", cerr)
		}
		return nil
	}, nil
}
