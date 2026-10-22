package pgstore

import (
	"context"
	"fmt"

	"github.com/kordloom/switchtender/internal/audit"
)

// attestationColumns is the select list every witness attestation read scans, in scan order.
const attestationColumns = "id, seq, kind, key_id, link, size, root, public_key, role, at, sig, " +
	"witness"

// attestationScanner reads one row. Both *sql.Row and *sql.Rows satisfy it.
type attestationScanner interface {
	// Scan copies the row's columns into dest.
	Scan(dest ...any) error
}

// SaveAttestation records one witness attestation, first write wins. The insert skips a row whose
// seq, kind, and key id are already held, and the held row is then read back and settled by
// audit.ResolveHeldAttestation: the same answer saved again succeeds under the held id, and a
// different one is refused with audit.ErrAttestationConflict. The held row is never changed.
func (s *auditStore) SaveAttestation(ctx context.Context, a *audit.WitnessAttestation) error {
	const q = `INSERT INTO audit_attestations (` + attestationColumns + `)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
ON CONFLICT (seq, kind, key_id) DO NOTHING`
	res, err := s.db.ExecContext(ctx, q, a.ID, a.Seq, a.Kind, a.KeyID, a.Link, a.Size, a.Root,
		a.PublicKey, a.Role, a.At, a.Sig, a.Witness)
	if err != nil {
		return fmt.Errorf("save attestation: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("save attestation: %w", err)
	}
	if n == 1 {
		return nil
	}
	held, err := scanAttestation(s.db.QueryRowContext(ctx, "SELECT "+attestationColumns+
		" FROM audit_attestations WHERE seq = $1 AND kind = $2 AND key_id = $3",
		a.Seq, a.Kind, a.KeyID))
	if err != nil {
		return fmt.Errorf("save attestation: read the held record: %w", err)
	}
	return audit.ResolveHeldAttestation(held, a)
}

// Attestations returns every witness attestation at or below seq, oldest first with ties broken on
// id. A seq of zero or less returns all of them.
func (s *auditStore) Attestations(ctx context.Context, seq int64) ([]*audit.WitnessAttestation, error) {
	q := "SELECT " + attestationColumns + " FROM audit_attestations"
	args := []any{}
	if seq > 0 {
		q += " WHERE seq <= $1"
		args = append(args, seq)
	}
	// The id tie-break compares bytes, as the memory and SQLite stores do, rather than following the
	// database's collation.
	q += ` ORDER BY seq ASC, id COLLATE "C" ASC`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list attestations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []*audit.WitnessAttestation{}
	for rows.Next() {
		a, err := scanAttestation(rows)
		if err != nil {
			return nil, fmt.Errorf("list attestations: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list attestations: %w", err)
	}
	return out, nil
}

// DeleteAttestation removes the witness attestation with the given id, or reports
// audit.ErrAttestationNotFound.
func (s *auditStore) DeleteAttestation(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, "DELETE FROM audit_attestations WHERE id = $1", id)
	if err != nil {
		return fmt.Errorf("delete attestation: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete attestation: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("delete attestation %s: %w", id, audit.ErrAttestationNotFound)
	}
	return nil
}

// scanAttestation reads one witness attestation row in attestationColumns order.
func scanAttestation(row attestationScanner) (*audit.WitnessAttestation, error) {
	var a audit.WitnessAttestation
	if err := row.Scan(&a.ID, &a.Seq, &a.Kind, &a.KeyID, &a.Link, &a.Size, &a.Root, &a.PublicKey,
		&a.Role, &a.At, &a.Sig, &a.Witness); err != nil {
		return nil, fmt.Errorf("scan attestation: %w", err)
	}
	return &a, nil
}
