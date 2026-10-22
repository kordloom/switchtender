package outcome

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/run"
)

// StartRecord is the canonical body a START entry commits: which run started, at which attempt, and
// a digest of the seal the run carried when it started, so the entry names the sealed row that ran
// and not only its id.
type StartRecord struct {
	// RunID is the run that started.
	RunID string `json:"run_id"`
	// Attempt is which try of the run started.
	Attempt int `json:"attempt"`
	// SpecSealDigest is the SHA-256 of the run's spec seal, empty when the row carried none. The
	// seal is keyed and never leaves the process, so only its digest is committed.
	SpecSealDigest string `json:"spec_seal_digest"`
}

// StartBody assembles the canonical start record for r and returns its JSON.
func StartBody(r *run.Run) ([]byte, error) {
	rec := StartRecord{RunID: r.ID, Attempt: r.Attempt}
	if r.SpecSeal != "" {
		sum := sha256.Sum256([]byte(r.SpecSeal))
		rec.SpecSealDigest = "sha256:" + hex.EncodeToString(sum[:])
	}
	return json.Marshal(rec)
}

// StartEntry builds the START entry a run's start appends before any tool runs, without appending
// it. It commits the start body under the exact digest, names starter as the system actor that
// started the run on behalf of whoever fired it, and takes the id audit.StartID fixes from the run
// and its attempt, so a second start of the same attempt builds an entry the chain refuses as a
// duplicate. now reads the entry's time, and a nil function falls back to time.Now.
func StartEntry(r *run.Run, starter string, now func() time.Time) (*audit.Entry, error) {
	body, err := StartBody(r)
	if err != nil {
		return nil, err
	}
	digest, nonce, err := audit.ExactDigestOf(body)
	if err != nil {
		return nil, err
	}
	if now == nil {
		now = time.Now
	}
	return &audit.Entry{
		ID: audit.StartID(r.ID, r.Attempt), At: now(),
		Actor: starter, ActorType: "system", OnBehalfOf: r.Actor,
		Method: audit.MethodStart, Path: audit.StartPath(r.ID, r.Attempt),
		ContentDigest: digest, Nonce: nonce,
	}, nil
}
