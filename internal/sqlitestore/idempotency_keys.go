package sqlitestore

import (
	"database/sql"
	"fmt"

	"github.com/kordloom/switchtender/internal/run"
)

// legacyKeyCandidates selects the runs whose idempotency key may be one an earlier release stored in
// a form PostgreSQL cannot hold: a key with a NUL byte, found byte by byte on the blob since SQLite's
// text functions stop at one, and a key with any character outside printable ASCII, which is where a
// byte that is not UTF-8 shows. Every key the server derives today is printable ASCII, so the scan
// reads next to nothing once an install has been upgraded.
const legacyKeyCandidates = `SELECT id, idempotency_key FROM runs WHERE idempotency_key <> '' AND
	(instr(CAST(idempotency_key AS BLOB), X'00') > 0 OR idempotency_key GLOB '*[^ -~]*')`

// upgradeIdempotencyKeys rewrites every idempotency key an earlier release stored in a form
// PostgreSQL cannot hold into the key this release derives for the same request.
//
// Earlier releases joined an organization to a caller's key, and a provisioning callback's template
// to its host, with a NUL byte, and stored a caller's key that was not UTF-8 as it arrived. SQLite
// kept all of them. This release derives text in their place, so without the rewrite a retried
// submission from an organization would miss the run it already made and fire a second one. The
// rewrite runs in one transaction, so an interrupted open leaves every key as it was.
func upgradeIdempotencyKeys(db *sql.DB) error {
	rows, err := db.Query(legacyKeyCandidates)
	if err != nil {
		return fmt.Errorf("read idempotency keys to upgrade: %w", err)
	}
	type rekey struct {
		// id is the run holding the key.
		id string
		// key is the key this release derives in its place.
		key string
	}
	var todo []rekey
	for rows.Next() {
		var id, stored string
		if err := rows.Scan(&id, &stored); err != nil {
			_ = rows.Close()
			return fmt.Errorf("read idempotency keys to upgrade: %w", err)
		}
		if key, changed := run.CurrentKey(stored); changed {
			todo = append(todo, rekey{id: id, key: key})
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("read idempotency keys to upgrade: %w", err)
	}
	// The store holds one connection, so the read is closed before the rewrite takes it.
	if err := rows.Close(); err != nil {
		return fmt.Errorf("read idempotency keys to upgrade: %w", err)
	}
	if len(todo) == 0 {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("upgrade idempotency keys: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, k := range todo {
		if _, err := tx.Exec("UPDATE runs SET idempotency_key=? WHERE id=?", k.key, k.id); err != nil {
			return fmt.Errorf("upgrade the idempotency key of run %s: %w", k.id, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("upgrade idempotency keys: %w", err)
	}
	return nil
}
