package pgstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/kordloom/switchtender/internal/forgelink"
	"github.com/kordloom/switchtender/internal/sqlutil"
)

// forgeLinkColumns is the shared select list for forge link reads.
const forgeLinkColumns = `id, user_id, provider, api_url, forge_user_id, created_at`

// forgeLinkStore is a forgelink.Store backed by the shared PostgreSQL database. The two unique
// indexes on forge_links decide both linking rules, so two replicas racing to link one forge
// account, or one account twice on one forge, leave exactly one row.
type forgeLinkStore struct {
	// db is the open database handle shared with the run store.
	db *sql.DB
}

// ForgeLinks returns the store of links between forge accounts and SwitchTender accounts.
func (d *DB) ForgeLinks() forgelink.Store {
	return &forgeLinkStore{db: d.db}
}

// Create inserts l, refusing a forge account or a per-forge account link that already exists.
func (s *forgeLinkStore) Create(ctx context.Context, l *forgelink.Link) error {
	const q = `INSERT INTO forge_links (` + forgeLinkColumns + `) VALUES ($1, $2, $3, $4, $5, $6)`
	if _, err := s.db.ExecContext(ctx, q, l.ID, l.UserID, l.Provider, l.APIURL, l.ForgeUserID,
		sqlutil.FormatTime(l.CreatedAt)); err != nil {
		if isKeyConflict(err) {
			return forgelink.ErrLinked
		}
		return fmt.Errorf("create forge link: %w", err)
	}
	return nil
}

// Lookup returns the link of the forge account forgeUserID on the forge provider at apiURL.
func (s *forgeLinkStore) Lookup(ctx context.Context, provider, apiURL string,
	forgeUserID int64) (*forgelink.Link, error) {
	l, err := scanForgeLink(s.db.QueryRowContext(ctx, "SELECT "+forgeLinkColumns+
		" FROM forge_links WHERE provider=$1 AND api_url=$2 AND forge_user_id=$3",
		provider, apiURL, forgeUserID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, forgelink.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("look up forge link: %w", err)
	}
	return l, nil
}

// ForUser returns the links of userID, oldest first.
func (s *forgeLinkStore) ForUser(ctx context.Context, userID string) ([]*forgelink.Link, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT "+forgeLinkColumns+" FROM forge_links WHERE user_id=$1", userID)
	if err != nil {
		return nil, fmt.Errorf("list forge links: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []*forgelink.Link{}
	for rows.Next() {
		l, err := scanForgeLink(rows)
		if err != nil {
			return nil, fmt.Errorf("list forge links: %w", err)
		}
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list forge links: %w", err)
	}
	// Ordered here rather than in SQL, because the stored text sorts an instant with a fraction after
	// the whole second it falls in.
	forgelink.SortLinks(out)
	return out, nil
}

// Delete removes the link id of userID in one statement and returns the row it removed, so a link
// belonging to another account is never touched.
func (s *forgeLinkStore) Delete(ctx context.Context, userID, id string) (*forgelink.Link, error) {
	l, err := scanForgeLink(s.db.QueryRowContext(ctx, "DELETE FROM forge_links "+
		"WHERE id=$1 AND user_id=$2 RETURNING "+forgeLinkColumns, id, userID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, forgelink.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("delete forge link: %w", err)
	}
	return l, nil
}

// DeleteUser removes every link of userID and reports how many it removed.
func (s *forgeLinkStore) DeleteUser(ctx context.Context, userID string) (int, error) {
	res, err := s.db.ExecContext(ctx, "DELETE FROM forge_links WHERE user_id=$1", userID)
	if err != nil {
		return 0, fmt.Errorf("delete forge links: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("delete forge links: %w", err)
	}
	return int(n), nil
}

// scanForgeLink reads one forge link row.
func scanForgeLink(sc scanner) (*forgelink.Link, error) {
	var (
		l  forgelink.Link
		at string
	)
	if err := sc.Scan(&l.ID, &l.UserID, &l.Provider, &l.APIURL, &l.ForgeUserID, &at); err != nil {
		return nil, err
	}
	when, err := sqlutil.ParseTime(at)
	if err != nil {
		return nil, fmt.Errorf("parse forge link time: %w", err)
	}
	l.CreatedAt = when
	return &l, nil
}
