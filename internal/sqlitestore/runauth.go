package sqlitestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/sqlutil"
)

// RunAuthFor returns what decides a run's readability, from the run while it exists and from the
// retained decision once retention has deleted it.
//
// One accessor on purpose. Derived rows outlive their runs, so a caller checking whether somebody
// may read a summary, a drift row, or a state reading has to get the same answer before and after
// the run is purged. Two lookups that are meant to agree would eventually disagree, silently, in
// the direction of showing somebody something.
func (s *store) RunAuthFor(ctx context.Context, id string) (*run.RunAuth, error) {
	const live = `
SELECT id, org_id, project_id, inventory_id, pull_credential_id, credential_ids
FROM runs WHERE id = ?`
	auth, err := scanRunAuth(s.db.r.QueryRowContext(ctx, live, id))
	if err == nil {
		return auth, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("run authorization: %w", err)
	}
	const retained = `
SELECT run_id, org_id, project_id, inventory_id, pull_credential_id, credential_ids
FROM run_auth WHERE run_id = ?`
	auth, err = scanRunAuth(s.db.r.QueryRowContext(ctx, retained, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, run.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("run authorization: %w", err)
	}
	return auth, nil
}

// scanRunAuth reads one authorization row, from either table, which carry the same columns in the
// same order so a single scanner cannot read one of them wrongly.
func scanRunAuth(row *sql.Row) (*run.RunAuth, error) {
	var (
		auth  run.RunAuth
		creds string
	)
	if err := row.Scan(&auth.ID, &auth.OrgID, &auth.ProjectID, &auth.InventoryID,
		&auth.PullCredentialID, &creds); err != nil {
		return nil, err
	}
	auth.CredentialIDs = sqlutil.SplitIDs(creds)
	return &auth, nil
}
