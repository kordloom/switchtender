package pgstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/kordloom/switchtender/internal/factcache"
	"github.com/kordloom/switchtender/internal/sqlutil"
)

// factCacheStore is a factcache.Store backed by PostgreSQL.
type factCacheStore struct {
	// db is the open database handle shared with the run store.
	db *sql.DB
}

// SaveFacts validates every entry and then upserts them in one transaction, so a batch is written
// whole or not at all.
//
// The upsert keeps the facts gathered later. Every save used to replace the stored document
// whatever its age, so a run that gathered first and finished last wrote its older facts over the
// newer ones and stamped them as the newest. Each stamp is compared the way TimeOrder sorts them,
// without the trailing Z, so two stamps inside one second still compare in time order.
//
// An entry whose inventory is no longer stored is not written. A run collects its facts when it
// ends, and an inventory deleted while it ran took its facts with it, so writing them would bring
// back what every host reported about itself with no inventory left to delete it through. The
// inventory's row is read under a key share lock, the lock a foreign key check takes: a delete
// already under way makes this wait and then find the row gone, and one that starts after waits for
// this to commit and then deletes what it wrote.
func (s *factCacheStore) SaveFacts(ctx context.Context, entries []factcache.Entry) error {
	for _, e := range entries {
		if err := factcache.Validate(e); err != nil {
			return err
		}
	}
	if len(entries) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("save cached facts: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	stored := map[string]bool{}
	for _, e := range entries {
		if _, seen := stored[e.InventoryID]; seen {
			continue
		}
		var one int
		err := tx.QueryRowContext(ctx, "SELECT 1 FROM inventories WHERE id=$1 FOR KEY SHARE",
			e.InventoryID).Scan(&one)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("save cached facts: %w", err)
		}
		stored[e.InventoryID] = err == nil
	}
	stmt, err := tx.PrepareContext(ctx, `
INSERT INTO host_fact_cache (inventory_id, host, facts, run_id, modified_at)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT(inventory_id, host) DO UPDATE SET
	facts=excluded.facts, run_id=excluded.run_id, modified_at=excluded.modified_at
WHERE rtrim(host_fact_cache.modified_at, 'Z') <= rtrim(excluded.modified_at, 'Z')`)
	if err != nil {
		return fmt.Errorf("save cached facts: %w", err)
	}
	defer func() { _ = stmt.Close() }()
	for _, e := range entries {
		if !stored[e.InventoryID] {
			continue
		}
		if _, err := stmt.ExecContext(ctx, e.InventoryID, e.Host, string(e.Facts), e.RunID,
			sqlutil.FormatTime(e.ModifiedAt)); err != nil {
			return fmt.Errorf("save cached facts: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("save cached facts: %w", err)
	}
	return nil
}

// Facts returns one host's entry with its fact document, or factcache.ErrNotFound.
func (s *factCacheStore) Facts(ctx context.Context, inventoryID, host string) (*factcache.Entry, error) {
	const q = `SELECT inventory_id, host, facts, run_id, modified_at FROM host_fact_cache
	WHERE inventory_id=$1 AND host=$2`
	e, err := scanFactEntry(s.db.QueryRowContext(ctx, q, inventoryID, host), true)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, factcache.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read cached facts: %w", err)
	}
	return e, nil
}

// List returns an inventory's entries ordered by host, with the documents only when withFacts.
func (s *factCacheStore) List(ctx context.Context, inventoryID string, withFacts bool) ([]factcache.Entry, error) {
	// A summary reads the document's length rather than the document, so listing a large inventory
	// does not pull every fact set off disk to throw it away.
	column := "octet_length(facts)"
	if withFacts {
		column = "facts"
	}
	q := "SELECT inventory_id, host, " + column + `, run_id, modified_at FROM host_fact_cache
	WHERE inventory_id=$1 ORDER BY host`
	rows, err := s.db.QueryContext(ctx, q, inventoryID)
	if err != nil {
		return nil, fmt.Errorf("list cached facts: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []factcache.Entry
	for rows.Next() {
		e, err := scanFactEntry(rows, withFacts)
		if err != nil {
			return nil, fmt.Errorf("list cached facts: %w", err)
		}
		out = append(out, *e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list cached facts: %w", err)
	}
	return out, nil
}

// Clear removes the named hosts' facts from an inventory.
func (s *factCacheStore) Clear(ctx context.Context, inventoryID string, hosts ...string) (int, error) {
	if len(hosts) == 0 {
		return 0, nil
	}
	args := make([]any, 0, len(hosts)+1)
	args = append(args, inventoryID)
	for _, h := range hosts {
		args = append(args, h)
	}
	marks := make([]string, len(hosts))
	for i := range hosts {
		marks[i] = fmt.Sprintf("$%d", i+2)
	}
	q := "DELETE FROM host_fact_cache WHERE inventory_id=$1 AND host IN (" +
		strings.Join(marks, ",") + ")"
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		return 0, fmt.Errorf("clear cached facts: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("clear cached facts: %w", err)
	}
	return int(n), nil
}

// ClearInventory removes every host's facts from an inventory.
func (s *factCacheStore) ClearInventory(ctx context.Context, inventoryID string) (int, error) {
	res, err := s.db.ExecContext(ctx, "DELETE FROM host_fact_cache WHERE inventory_id=$1", inventoryID)
	if err != nil {
		return 0, fmt.Errorf("clear cached facts: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("clear cached facts: %w", err)
	}
	return int(n), nil
}

// scanFactEntry reads one fact cache row. With withFacts the third column is the document, and
// without it the third column is the document's length.
func scanFactEntry(sc scanner, withFacts bool) (*factcache.Entry, error) {
	var (
		e        factcache.Entry
		facts    string
		size     int
		modified string
	)
	third := any(&size)
	if withFacts {
		third = &facts
	}
	if err := sc.Scan(&e.InventoryID, &e.Host, third, &e.RunID, &modified); err != nil {
		return nil, err
	}
	at, err := sqlutil.ParseTime(modified)
	if err != nil {
		return nil, err
	}
	e.ModifiedAt = at
	if withFacts {
		e.Facts = []byte(facts)
		size = len(facts)
	}
	e.Bytes = size
	return &e, nil
}
