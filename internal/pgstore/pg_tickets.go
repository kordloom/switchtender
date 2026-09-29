package pgstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/sqlutil"
)

// SaveStreamTicket records a ticket, sweeping expired rows and holding the bounds by evicting the
// caller's own oldest first, then anyone's oldest, the same fairness the in-process table kept.
// The bounds are approximate under concurrent mints across replicas, which is the same promise
// the in-memory table made within one: nobody grows the table without limit.
func (s *store) SaveStreamTicket(ctx context.Context, t run.StreamTicket,
	perActorCap, totalCap int) error {
	if _, err := s.db.ExecContext(ctx,
		"DELETE FROM stream_tickets WHERE expires_at < "+pgNowText); err != nil {
		return fmt.Errorf("sweep stream tickets: %w", err)
	}
	if perActorCap > 0 {
		if _, err := s.db.ExecContext(ctx, `DELETE FROM stream_tickets WHERE secret_hash IN (
SELECT secret_hash FROM stream_tickets WHERE actor_key=$1 ORDER BY expires_at DESC
OFFSET $2)`, t.ActorKey, perActorCap-1); err != nil {
			return fmt.Errorf("bound caller tickets: %w", err)
		}
	}
	if totalCap > 0 {
		if _, err := s.db.ExecContext(ctx, `DELETE FROM stream_tickets WHERE secret_hash IN (
SELECT secret_hash FROM stream_tickets ORDER BY expires_at DESC OFFSET $1)`,
			totalCap-1); err != nil {
			return fmt.Errorf("bound stream tickets: %w", err)
		}
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO stream_tickets (secret_hash, run_id, actor_key, actor, expires_at)
VALUES ($1,$2,$3,$4,$5)`,
		t.SecretHash, t.RunID, t.ActorKey, string(t.Actor),
		sqlutil.FormatTime(t.ExpiresAt)); err != nil {
		return fmt.Errorf("save stream ticket: %w", err)
	}
	return nil
}

// RedeemStreamTicket consumes the ticket exactly once across every replica: the delete is the
// claim, and whichever replica's delete removes the row wins it.
func (s *store) RedeemStreamTicket(ctx context.Context, secretHash, runID string,
	now time.Time) ([]byte, bool, error) {
	var actor, expires, storedRun string
	err := s.db.QueryRowContext(ctx,
		`DELETE FROM stream_tickets WHERE secret_hash=$1 RETURNING run_id, actor, expires_at`,
		secretHash).Scan(&storedRun, &actor, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("redeem stream ticket: %w", err)
	}
	exp, err := sqlutil.ParseTime(expires)
	if err != nil || now.After(exp) || storedRun != runID {
		return nil, false, nil
	}
	return []byte(actor), true, nil
}
