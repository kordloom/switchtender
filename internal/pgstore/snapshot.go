package pgstore

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/stdlib"
)

// The pool limits Open sets, and a read snapshot puts back when it ends.
const (
	// poolMaxOpen caps the pool so a burst of API reads and SSE streams cannot exhaust the server's
	// max_connections.
	poolMaxOpen = 24
	// poolMaxIdle is how many idle connections the pool keeps.
	poolMaxIdle = 8
	// poolMaxLifetime recycles connections so a load balancer or pooler can rebalance.
	poolMaxLifetime = 30 * time.Minute
	// poolMaxIdleTime closes a connection that has sat idle this long.
	poolMaxIdleTime = 5 * time.Minute
)

// readPin records whether a read snapshot holds the pool.
type readPin struct {
	// held is true from the moment a snapshot begins until it is released.
	held atomic.Bool
}

// pinConnector opens the pool's connections through the pgx driver, each wrapped so a read snapshot
// can keep its transaction open across the statements it pins.
type pinConnector struct {
	// base opens the underlying pgx connections.
	base driver.Connector
	// pin is the pool's snapshot state, shared by every connection it opens.
	pin *readPin
}

// Connect opens one connection.
func (c *pinConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.base.Connect(ctx)
	if err != nil {
		return nil, err
	}
	pgxConn, ok := conn.(*stdlib.Conn)
	if !ok {
		return conn, nil
	}
	return &pinConn{Conn: pgxConn, pin: c.pin}, nil
}

// Driver returns the pgx driver the connections come from.
func (c *pinConnector) Driver() driver.Driver { return c.base.Driver() }

// pinConn is one pooled pgx connection. Every method is the pgx connection's own except the reset
// the pool runs before handing the connection out again.
type pinConn struct {
	*stdlib.Conn
	// pin is the pool's snapshot state.
	pin *readPin
}

// ResetSession keeps the connection exactly as it is while a read snapshot holds the pool. The pgx
// reset discards a connection that has a transaction open, which is the right default and the
// opposite of what a snapshot needs: its transaction is open on purpose, across every statement it
// pins. Outside a snapshot the pgx reset runs unchanged.
func (c *pinConn) ResetSession(ctx context.Context) error {
	if c.pin.held.Load() {
		return nil
	}
	return c.Conn.ResetSession(ctx)
}

// openPinnable opens the connection pool for dsn the way sql.Open with the pgx driver does, through
// a connector whose connections a read snapshot can pin.
func openPinnable(dsn string) (*sql.DB, *readPin, error) {
	drv, ok := stdlib.GetDefaultDriver().(driver.DriverContext)
	if !ok {
		return nil, nil, errors.New("open postgres: the pgx driver opens no connector")
	}
	base, err := drv.OpenConnector(dsn)
	if err != nil {
		return nil, nil, fmt.Errorf("open postgres: %w", err)
	}
	pin := &readPin{}
	return sql.OpenDB(&pinConnector{base: base, pin: pin}), pin, nil
}

// BeginReadSnapshot pins every read that follows to one connection holding an open REPEATABLE READ,
// READ ONLY transaction, so a backup reads the whole database at one instant instead of table by
// table while a replica writes it. A backup gathered unpinned could hold a state the database never
// was in, an object created mid-gather present in the tables read after it and absent from those
// read before, and a restore wrote that state back as though it had been real.
//
// It narrows the pool to one connection, opens the transaction on it, and keeps the pool from
// resetting or recycling that connection until the release, so each read that follows runs inside
// the snapshot. That is sound here because the one caller is the backup command, whose only
// database work is the snapshot itself. It is not a facility for a process with concurrent work,
// whose statements would land inside a read-only transaction. The release's error is real: it
// checks that the backend that took the snapshot is the one still holding it in the same
// transaction, so a connection lost and replaced mid-read fails the backup rather than sealing a
// mix of instants.
func (d *DB) BeginReadSnapshot(ctx context.Context) (func() error, error) {
	if d.pin == nil {
		return nil, errors.New("begin read snapshot: this database handle cannot pin a snapshot")
	}
	d.db.SetMaxOpenConns(1)
	d.db.SetMaxIdleConns(1)
	d.db.SetConnMaxLifetime(0)
	d.db.SetConnMaxIdleTime(0)
	d.pin.held.Store(true)
	unpin := func() {
		d.pin.held.Store(false)
		d.db.SetMaxOpenConns(poolMaxOpen)
		d.db.SetMaxIdleConns(poolMaxIdle)
		d.db.SetConnMaxLifetime(poolMaxLifetime)
		d.db.SetConnMaxIdleTime(poolMaxIdleTime)
	}
	if _, err := d.db.ExecContext(ctx, "BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY"); err != nil {
		unpin()
		return nil, fmt.Errorf("begin read snapshot: %w", err)
	}
	// A repeatable read transaction takes its snapshot at its first query, not at BEGIN, so the
	// snapshot is fixed here, before the caller reads anything, by the query that names the backend
	// holding it.
	var backend int64
	if err := d.db.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&backend); err != nil {
		_, _ = d.db.ExecContext(context.Background(), "ROLLBACK")
		unpin()
		return nil, fmt.Errorf("begin read snapshot: %w", err)
	}
	return func() error {
		defer unpin()
		var holder int64
		var isolation string
		herr := d.db.QueryRowContext(context.Background(),
			"SELECT pg_backend_pid(), current_setting('transaction_isolation')").Scan(&holder,
			&isolation)
		_, cerr := d.db.ExecContext(context.Background(), "COMMIT")
		switch {
		case herr != nil:
			return fmt.Errorf("the read snapshot did not hold: %w", herr)
		case holder != backend || isolation != "repeatable read":
			return errors.New("the read snapshot did not hold: the connection that took it was " +
				"replaced while the backup read")
		case cerr != nil:
			return fmt.Errorf("the read snapshot did not hold: %w", cerr)
		}
		return nil
	}, nil
}
