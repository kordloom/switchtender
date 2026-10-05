package sqlitestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/kordloom/switchtender/internal/federation"
	"github.com/kordloom/switchtender/internal/sqlutil"
)

// fedKeyColumns is the shared select list for federation key reads.
const fedKeyColumns = `id, algorithm, public_key, sealed, created_at, retired_at, activated_at,
	removed_at`

// fedKeyStore is a federation.KeyStore backed by the shared SQLite database. The private key
// arrives already sealed, so the column holds ciphertext only.
type fedKeyStore struct {
	// db is the open database handle shared with the run store.
	db *splitDB
}

// fedKeyChange is one change to the keys in progress.
type fedKeyChange struct {
	// store is the store the change belongs to.
	store *fedKeyStore
	// tx is the change's transaction, which holds the database's write lock.
	tx *sql.Tx
	// now is the host clock read once the change held the write lock.
	now time.Time
}

// fedKeyChangeKey is the context key a change travels under.
type fedKeyChangeKey struct{}

// changeOf returns the change of s that ctx is inside, or nil.
func (s *fedKeyStore) changeOf(ctx context.Context) *fedKeyChange {
	c, _ := ctx.Value(fedKeyChangeKey{}).(*fedKeyChange)
	if c == nil || c.store != s {
		return nil
	}
	return c
}

// Change runs fn in one transaction on the write connection, which begins immediately and so holds
// the database's write lock from its start: the change is atomic, and serialized with every other
// writer of the file, in this process or any other. It commits only when fn succeeds.
func (s *fedKeyStore) Change(ctx context.Context, fn func(ctx context.Context) error) error {
	if s.changeOf(ctx) != nil {
		return fn(ctx)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("change federation keys: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := fn(context.WithValue(ctx, fedKeyChangeKey{},
		&fedKeyChange{store: s, tx: tx, now: time.Now()})); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("change federation keys: %w", err)
	}
	return nil
}

// Save inserts or replaces the key inside a change, refusing a write federation.CheckSave refuses.
func (s *fedKeyStore) Save(ctx context.Context, k *federation.Key) error {
	return s.Change(ctx, func(ctx context.Context) error {
		tx := s.changeOf(ctx).tx
		stored, err := scanFedKey(tx.QueryRowContext(ctx,
			"SELECT "+fedKeyColumns+" FROM federation_keys WHERE id=?", k.ID))
		switch {
		case errors.Is(err, sql.ErrNoRows):
			stored = nil
		case err != nil:
			return fmt.Errorf("save federation key: %w", err)
		}
		if err := federation.CheckSave(stored, k); err != nil {
			return err
		}
		const q = `
INSERT INTO federation_keys (id, algorithm, public_key, sealed, created_at, retired_at,
	activated_at, removed_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
	algorithm=excluded.algorithm, public_key=excluded.public_key, sealed=excluded.sealed,
	created_at=excluded.created_at, retired_at=excluded.retired_at,
	activated_at=excluded.activated_at, removed_at=excluded.removed_at`
		if _, err := tx.ExecContext(ctx, q, k.ID, k.Algorithm, k.PublicKey, k.Sealed,
			sqlutil.FormatTime(k.CreatedAt), fedKeyTime(k.RetiredAt), fedKeyTime(k.ActivatedAt),
			fedKeyTime(k.RemovedAt)); err != nil {
			return fmt.Errorf("save federation key: %w", err)
		}
		return nil
	})
}

// List returns every key, oldest first, as the change sees them inside a change.
func (s *fedKeyStore) List(ctx context.Context) ([]*federation.Key, error) {
	var (
		rows *sql.Rows
		err  error
	)
	const q = "SELECT " + fedKeyColumns + " FROM federation_keys"
	if c := s.changeOf(ctx); c != nil {
		rows, err = c.tx.QueryContext(ctx, q)
	} else {
		rows, err = s.db.QueryContext(ctx, q)
	}
	if err != nil {
		return nil, fmt.Errorf("list federation keys: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []*federation.Key
	for rows.Next() {
		k, err := scanFedKey(rows)
		if err != nil {
			return nil, fmt.Errorf("list federation keys: %w", err)
		}
		out = append(out, k)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list federation keys: %w", err)
	}
	// Ordered here rather than in SQL: the stored text sorts an instant with a fraction after the
	// whole second it falls in, and a handful of keys costs nothing to sort exactly.
	federation.SortKeys(out)
	return out, nil
}

// Now returns the host clock, the one every process on the database judges the keys by: a SQLite
// database is one file on one host, so every process sharing it reads the same clock. Inside a
// change it is the reading the change took once it held the write lock.
func (s *fedKeyStore) Now(ctx context.Context) (time.Time, error) {
	if c := s.changeOf(ctx); c != nil {
		return c.now, nil
	}
	return time.Now(), nil
}

// scanFedKey reads one federation key row.
func scanFedKey(sc scanner) (*federation.Key, error) {
	var (
		k                                      federation.Key
		created, retired, activated, removedAt string
	)
	if err := sc.Scan(&k.ID, &k.Algorithm, &k.PublicKey, &k.Sealed, &created, &retired, &activated,
		&removedAt); err != nil {
		return nil, err
	}
	at, err := sqlutil.ParseTime(created)
	if err != nil {
		return nil, fmt.Errorf("decode federation key created_at: %w", err)
	}
	k.CreatedAt = at
	if k.ActivatedAt, err = parseFedKeyTime("activated_at", activated); err != nil {
		return nil, err
	}
	if k.RetiredAt, err = parseFedKeyTime("retired_at", retired); err != nil {
		return nil, err
	}
	if k.RemovedAt, err = parseFedKeyTime("removed_at", removedAt); err != nil {
		return nil, err
	}
	return &k, nil
}

// parseFedKeyTime decodes the optional key time in the column name, nil for an empty column.
func parseFedKeyTime(name, text string) (*time.Time, error) {
	if text == "" {
		return nil, nil
	}
	t, err := sqlutil.ParseTime(text)
	if err != nil {
		return nil, fmt.Errorf("decode federation key %s: %w", name, err)
	}
	return &t, nil
}

// fedKeyTime renders an optional key time for its column, empty for a time that is not set.
func fedKeyTime(t *time.Time) string {
	if t == nil {
		return ""
	}
	return sqlutil.FormatTime(*t)
}

// healFedKeyLifecycle adds the activation and removal times to a federation_keys table from before
// them, with the meaning they had then, ahead of the generic heal that would add them empty. The
// build that wrote such a table signed with every key from its creation and kept a retired key
// published for federation.RetiredKeyGrace after it retired, so each key reads back activated at
// its creation and, once retired, removed a grace period later. Added empty, the activation read as
// a key that never signs: the first start generated a key that signed at once, with none of the
// day's notice a relying party is promised, and the key every cloud had fetched was listed as
// pending forever, never retired, removed, or erased.
func healFedKeyLifecycle(db *sql.DB) error {
	var exists int
	if err := db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND " +
		"name='federation_keys'").Scan(&exists); err != nil {
		return fmt.Errorf("heal federation_keys: %w", err)
	}
	if exists == 0 {
		return nil
	}
	have := map[string]bool{}
	rows, err := db.Query("SELECT name FROM pragma_table_info('federation_keys')")
	if err != nil {
		return fmt.Errorf("heal federation_keys: %w", err)
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			_ = rows.Close()
			return fmt.Errorf("heal federation_keys: %w", err)
		}
		have[name] = true
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("heal federation_keys: %w", err)
	}
	_ = rows.Close()
	if have["activated_at"] && have["removed_at"] {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("heal federation_keys: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	clauses := map[string]string{}
	for _, col := range sqlutil.ParseSchemaColumns(schema)["federation_keys"] {
		clauses[col.Name] = col.Clause
	}
	if !have["activated_at"] {
		if _, err := tx.Exec("ALTER TABLE federation_keys ADD COLUMN activated_at " +
			clauses["activated_at"]); err != nil {
			return fmt.Errorf("heal federation_keys: add activated_at: %w", err)
		}
		if _, err := tx.Exec("UPDATE federation_keys SET activated_at = created_at"); err != nil {
			return fmt.Errorf("heal federation_keys: activated_at: %w", err)
		}
	}
	if !have["removed_at"] {
		if _, err := tx.Exec("ALTER TABLE federation_keys ADD COLUMN removed_at " +
			clauses["removed_at"]); err != nil {
			return fmt.Errorf("heal federation_keys: add removed_at: %w", err)
		}
		removals, err := fedKeyRemovals(tx.Query("SELECT id, retired_at FROM federation_keys " +
			"WHERE retired_at <> ''"))
		if err != nil {
			return fmt.Errorf("heal federation_keys: removed_at: %w", err)
		}
		for id, at := range removals {
			if _, err := tx.Exec("UPDATE federation_keys SET removed_at = ? WHERE id = ?", at,
				id); err != nil {
				return fmt.Errorf("heal federation_keys: removed_at: %w", err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("heal federation_keys: %w", err)
	}
	return nil
}

// fedKeyRemovals reads id and retired_at rows and returns, by id, the removal time the build before
// removal times gave each retired key: federation.RetiredKeyGrace after its retirement.
func fedKeyRemovals(rows *sql.Rows, err error) (map[string]string, error) {
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]string{}
	for rows.Next() {
		var id, retired string
		if err := rows.Scan(&id, &retired); err != nil {
			return nil, err
		}
		at, err := sqlutil.ParseTime(retired)
		if err != nil {
			return nil, fmt.Errorf("decode federation key %s retired_at: %w", id, err)
		}
		out[id] = sqlutil.FormatTime(at.Add(federation.RetiredKeyGrace))
	}
	return out, rows.Err()
}
