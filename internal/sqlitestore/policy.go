package sqlitestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/kordloom/switchtender/internal/policy"
)

// policyStore is a policy.Store backed by the shared SQLite database.
type policyStore struct {
	// db is the open database handle shared with the run store.
	db *sql.DB
}

// policyColumns lists the policy columns in a stable order for reads and writes.
const policyColumns = `id, name, tool, command_contains, inventory_id, exclude_dry_run, max_destroy, created_at`

// Save stores a policy, inserting or replacing by id.
func (s *policyStore) Save(ctx context.Context, p *policy.Policy) error {
	const q = `
INSERT INTO policies (id, name, tool, command_contains, inventory_id, exclude_dry_run, max_destroy, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
	name=excluded.name, tool=excluded.tool, command_contains=excluded.command_contains,
	inventory_id=excluded.inventory_id, exclude_dry_run=excluded.exclude_dry_run,
	max_destroy=excluded.max_destroy`
	_, err := s.db.ExecContext(ctx, q,
		p.ID, p.Name, p.Tool, p.CommandContains, p.InventoryID,
		boolToInt(p.ExcludeDryRun), p.MaxDestroy, formatTime(p.CreatedAt))
	if err != nil {
		return fmt.Errorf("save policy: %w", err)
	}
	return nil
}

// List returns every policy, oldest first.
func (s *policyStore) List(ctx context.Context) ([]*policy.Policy, error) {
	const q = "SELECT " + policyColumns + " FROM policies ORDER BY created_at, id"
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list policies: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []*policy.Policy{}
	for rows.Next() {
		p, err := scanPolicy(rows)
		if err != nil {
			return nil, fmt.Errorf("list policies: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list policies: %w", err)
	}
	return out, nil
}

// Get returns the policy with the given id, or policy.ErrNotFound when it does not exist.
func (s *policyStore) Get(ctx context.Context, id string) (*policy.Policy, error) {
	const q = "SELECT " + policyColumns + " FROM policies WHERE id=?"
	p, err := scanPolicy(s.db.QueryRowContext(ctx, q, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, policy.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get policy: %w", err)
	}
	return p, nil
}

// Delete removes a policy by id, returning policy.ErrNotFound when it does not exist.
func (s *policyStore) Delete(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, "DELETE FROM policies WHERE id=?", id)
	if err != nil {
		return fmt.Errorf("delete policy: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete policy: %w", err)
	}
	if n == 0 {
		return policy.ErrNotFound
	}
	return nil
}

// scanPolicy reads one policy row.
func scanPolicy(sc scanner) (*policy.Policy, error) {
	var (
		p       policy.Policy
		dry     int
		created string
	)
	if err := sc.Scan(&p.ID, &p.Name, &p.Tool, &p.CommandContains, &p.InventoryID, &dry,
		&p.MaxDestroy, &created); err != nil {
		return nil, err
	}
	p.ExcludeDryRun = dry != 0
	at, err := parseTime(created)
	if err != nil {
		return nil, err
	}
	p.CreatedAt = at
	return &p, nil
}
